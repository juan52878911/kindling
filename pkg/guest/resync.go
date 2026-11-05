package guest

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// POST /resync: el daemon manda la hora del host y entropía fresca justo
// después de restaurar la microVM de un snapshot (ver api.GuestResyncPath).
//
// Sin esto, todas las instancias de un mismo dorado comparten reloj parado y
// estado del CSPRNG. Lo segundo es el fallo grave: getrandom() devolvía los
// mismos bytes en réplicas independientes, y con ellos los mismos ids de sesión,
// nonces y claves efímeras.
//
// Quién puede llamarla: solo quien alcanza el puerto del agente, que es el host
// (el daemon, y los clientes de su socket, que ya son root). El gateway MCP NO
// debe reenviarle esta ruta a sus clientes: ver IsControlPath. Aun así, lo que
// entra se valida: tamaño acotado, entropía dentro de límites y una hora
// plausible. Mezclar bytes conocidos en el pool no resta entropía —el kernel
// los combina con un hash—, así que lo peor que puede hacer un llamante con
// acceso es mover el reloj, no predecir los aleatorios.

// setClock y mixEntropy son las dos llamadas al sistema de /resync. Variables
// para que el manejador se pruebe sin root y fuera de Linux.
var (
	setClock   = setClockOS
	mixEntropy = mixEntropyOS
)

// Ventana de horas aceptables. Una hora fuera de aquí no es un host con el
// reloj algo desviado: es basura o un intento de romper la validación de
// certificados del invitado.
var (
	resyncMinTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	resyncMaxTime = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

// resyncMu serializa: dos resync a la vez no rompen nada, pero el desfase que
// se devuelve sería el de la otra.
var resyncMu sync.Mutex

// ResyncHandler sirve POST /resync.
func ResyncHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, api.GuestResyncMaxBody+1))
		if err != nil {
			http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) > api.GuestResyncMaxBody {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		var req api.GuestResync
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := validarResync(req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		resyncMu.Lock()
		defer resyncMu.Unlock()
		// La entropía primero: es lo que importa para la seguridad, y un fallo
		// del reloj no debe dejar el CSPRNG sin resembrar.
		if err := mixEntropy(req.Entropy); err != nil {
			http.Error(w, "reseeding the kernel RNG: "+err.Error(), http.StatusInternalServerError)
			return
		}
		host := time.Unix(0, req.UnixNano)
		skew := time.Until(host)
		// Tras reanudar una pausada el reloj suele estar bien (KVM no para el
		// contador): ponerlo ahí solo sumaría lo que tardó la petición. Se
		// pone si va desfasado de verdad (kling-hv sí lo para).
		poner := req.Restore != ResyncResume || skew > resyncUmbralReanudar || skew < -resyncUmbralReanudar
		if poner {
			if err := setClock(host); err != nil {
				http.Error(w, "setting the clock: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		// Un arranque en frío o una reanudación no son restauraciones: ni
		// suben la generación ni cambian la identidad.
		gen := generation
		if !resyncSinRestaurar(req.Restore) {
			gen = bumpGeneration()
		}
		// Una copia de un dorado no es la misma máquina: machine-id propio.
		// Al descongelar la misma máquina no se toca.
		var renewed []string
		if req.Restore == api.ResyncInstance {
			renewed = renewMachineID(machineIDFiles)
		}
		// El estado de "listo" va en la respuesta: al daemon le dice, sin otra
		// petición, si la imagen declara ganchos que lanzar (POST /hooks, que
		// pide él cuando ya montó volúmenes y entregó credenciales) o una
		// sonda que esperar (ver ready.go).
		st := readyState.snapshot()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.GuestResyncResult{SkewMS: skew.Milliseconds(), Ready: &st, Generation: gen, Renewed: renewed})
	}
}

// Valores de GuestResync.Restore que no son restauraciones, y la capacidad
// de /healthz que dice que el agente los entiende. Un agente anterior los
// tomaría por una restauración más (subiría la generación), así que el host
// solo los manda a quien anuncia CapResyncClock.
const (
	// ResyncBoot: justo tras un arranque en frío. El kernel toma la hora del
	// RTC, que va a segundos (el PL031 de arm64), y le suma medio segundo:
	// el invitado nace hasta ±0,5 s desfasado del host.
	ResyncBoot = "boot"
	// ResyncResume: tras reanudar una máquina pausada (sin volcarla).
	ResyncResume = "resume"
	// CapResyncClock: el agente entiende ResyncBoot y ResyncResume.
	CapResyncClock = "resync-clock"
)

// resyncUmbralReanudar: tras ResyncResume, un desfase menor que esto no se
// corrige (sería ruido de la propia petición).
const resyncUmbralReanudar = 5 * time.Millisecond

// resyncSinRestaurar dice si kind es de los que solo ponen el reloj y la
// entropía, sin contar como restauración.
func resyncSinRestaurar(kind string) bool { return kind == ResyncBoot || kind == ResyncResume }

// generation cuenta las restauraciones de este invitado; generationFile es
// donde se publica (variable para los tests).
var (
	generation     uint64
	generationFile = api.GuestGenerationFile
)

// bumpGeneration sube el contador y lo escribe en generationFile con un
// rename, para que quien lo vigile no lea nunca un fichero a medias. Un fallo
// al escribirlo se apunta y no tumba el /resync: el kernel ya está resembrado.
// Se llama con resyncMu tomado.
func bumpGeneration() uint64 {
	generation++
	if err := writeGeneration(generationFile, generation); err != nil {
		log.Printf("resync: writing %s: %v", generationFile, err)
	}
	return generation
}

func writeGeneration(p string, n uint64) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatUint(n, 10)+"\n"), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func validarResync(req api.GuestResync) error {
	if n := len(req.Entropy); n < api.GuestResyncMinEntropy || n > api.GuestResyncMaxEntropy {
		return errors.New("entropy must be between 32 and 512 bytes")
	}
	t := time.Unix(0, req.UnixNano)
	if req.UnixNano <= 0 || t.Before(resyncMinTime) || !t.Before(resyncMaxTime) {
		return errors.New("unix_nano is not a plausible wall-clock time")
	}
	return nil
}

// controlPaths son las rutas del agente que solo debe usar el host (y todo lo
// que cuelga de ellas). Un proxy que reenvía peticiones de terceros al puerto
// del agente —el gateway MCP— las tiene que cortar: /resync mueve el reloj,
// /volume/release desmonta los volúmenes por debajo del servidor, /exec ejecuta,
// /share conecta una carpeta del host.
var controlPaths = []string{api.GuestResyncPath, "/volume", "/exec", "/files", "/dns", "/share",
	api.GuestReadyPath, api.GuestHooksPath, api.GuestMemInfoPath, api.GuestServicePath, api.GuestProcsPath,
	"/machine"}

// IsControlPath dice si p (una ruta ya limpia, con path.Clean) es una ruta de
// control del agente y no algo que un cliente del servicio deba alcanzar.
func IsControlPath(p string) bool {
	for _, c := range controlPaths {
		if p == c || strings.HasPrefix(p, c+"/") {
			return true
		}
	}
	return false
}
