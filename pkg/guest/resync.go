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
// Quién puede llamarla: solo el host (el daemon, y los clientes de su socket,
// que ya son root). El puerto del agente también lo alcanza un proceso del
// propio invitado, por loopback o por su IP: Register la envuelve en soloHost,
// que se lo niega. El gateway MCP NO debe reenviarle esta ruta a sus clientes:
// ver IsControlPath. Aun así, lo que
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
// se devuelve sería el de la otra. Con el segundo tiempo en segundo plano, se
// suelta cuando este acaba, no al contestar: el siguiente resync no pisa al
// anterior.
var resyncMu sync.Mutex

// Pasos del segundo tiempo; variables para los tests.
var (
	renovarIdentidad = func() []string { return renewMachineID(machineIDFiles) }
	cortarProcesos   = cortarExecs
	limpiarFondo     = limpiarProcesos
)

// ResyncTiempos son los tiempos (µs) de cada paso del último /resync, medidos
// dentro del invitado: la traza del host solo ve el total. GET /resync (solo
// host) los devuelve. Los pasos del segundo tiempo se rellenan al acabar:
// Diferido es false mientras tanto.
type ResyncTiempos struct {
	Restore    string `json:"restore,omitempty"`
	Origen     int64  `json:"origen_us"`
	Cuerpo     int64  `json:"cuerpo_us"`
	Entropia   int64  `json:"entropia_us"`
	Reloj      int64  `json:"reloj_us"`
	Generacion int64  `json:"generacion_us"`
	MachineID  int64  `json:"machine_id_us"`
	FicheroGen int64  `json:"fichero_generacion_us"`
	Exec       int64  `json:"exec_us"`
	Procesos   int64  `json:"procesos_us"`
	Respuesta  int64  `json:"respuesta_us"`
	Critico    int64  `json:"critico_us"`
	Diferido   bool   `json:"diferido_hecho"`
	DiferidoUS int64  `json:"diferido_us"`
}

var (
	tiemposMu      sync.Mutex
	ultimosTiempos ResyncTiempos
)

func guardarTiempos(f func(t *ResyncTiempos)) {
	tiemposMu.Lock()
	defer tiemposMu.Unlock()
	f(&ultimosTiempos)
}

func us(d time.Duration) int64 { return d.Microseconds() }

// ResyncHandler sirve POST /resync (y GET, los tiempos del último).
func ResyncHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			tiemposMu.Lock()
			t := ultimosTiempos
			tiemposMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(t)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "use POST", http.StatusMethodNotAllowed)
			return
		}
		inicio := time.Now()
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
		tCuerpo := time.Since(inicio)

		resyncMu.Lock()
		// Lo que se hace tras contestar: la generación en fichero y, en una
		// copia de un dorado, identidad y limpieza. Si lo hay, la puerta se
		// cierra ya, antes de tocar nada, y se abre al acabarlo.
		restaura := !resyncSinRestaurar(req.Restore)
		diferido := false
		defer func() {
			if !diferido {
				resyncMu.Unlock()
			}
		}()
		if restaura {
			puertaResync.Cerrar()
			defer func() {
				if !diferido {
					puertaResync.Abrir()
				}
			}()
		}

		// La entropía primero: es lo que importa para la seguridad, y un fallo
		// del reloj no debe dejar el CSPRNG sin resembrar.
		t0 := time.Now()
		if err := mixEntropy(req.Entropy); err != nil {
			http.Error(w, "reseeding the kernel RNG: "+err.Error(), http.StatusInternalServerError)
			return
		}
		tEntropia := time.Since(t0)
		host := time.Unix(0, req.UnixNano)
		skew := time.Until(host)
		// Tras reanudar una pausada el reloj suele estar bien (KVM no para el
		// contador): ponerlo ahí solo sumaría lo que tardó la petición. Se
		// pone si va desfasado de verdad (kling-hv sí lo para).
		poner := req.Restore != ResyncResume || skew > resyncUmbralReanudar || skew < -resyncUmbralReanudar
		var tReloj time.Duration
		if poner {
			t0 = time.Now()
			if err := setClock(host); err != nil {
				http.Error(w, "setting the clock: "+err.Error(), http.StatusInternalServerError)
				return
			}
			tReloj = time.Since(t0)
		}
		// Un arranque en frío o una reanudación no son restauraciones: ni
		// suben la generación ni cambian la identidad.
		t0 = time.Now()
		gen := generation
		if restaura {
			generation++
			gen = generation
		}
		tGen := time.Since(t0)
		// El estado de "listo" va en la respuesta: al daemon le dice, sin otra
		// petición, si la imagen declara ganchos que lanzar (POST /hooks, que
		// pide él cuando ya montó volúmenes y entregó credenciales) o una
		// sonda que esperar (ver ready.go).
		st := readyState.snapshot()
		// Renewed sale vacío: el host no lo usa.
		w.Header().Set("Content-Type", "application/json")
		t0 = time.Now()
		_ = json.NewEncoder(w).Encode(api.GuestResyncResult{SkewMS: skew.Milliseconds(), Ready: &st, Generation: gen})
		tResp := time.Since(t0)
		critico := time.Since(inicio)
		guardarTiempos(func(t *ResyncTiempos) {
			*t = ResyncTiempos{Restore: req.Restore, Origen: t.Origen, Cuerpo: us(tCuerpo), Entropia: us(tEntropia),
				Reloj: us(tReloj), Generacion: us(tGen), Respuesta: us(tResp), Critico: us(critico), Diferido: !restaura}
		})
		if !restaura {
			return
		}
		diferido = true
		go vigilarSegundoTiempo(req.Restore, gen)
	}
}

// Tope del segundo tiempo y el paso que se ejecuta; variables para los tests.
var (
	topeSegundoTiempo  = 5 * time.Second
	hacerSegundoTiempo = segundoTiempo
)

// vigilarSegundoTiempo corre el segundo tiempo con recover y un tope: si
// entra en pánico o se cuelga (E/S bloqueada), abre la puerta y suelta
// resyncMu igualmente, y lo deja en el log. Lo que quede colgado termina por
// su cuenta sin bloquear a nadie. Se llama con resyncMu tomado y la puerta
// cerrada; los libera siempre, una sola vez.
func vigilarSegundoTiempo(restore string, gen uint64) {
	fn, topeT := hacerSegundoTiempo, topeSegundoTiempo
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("resync: second phase panicked: %v", r)
			}
		}()
		fn(restore, gen)
	}()
	tope := time.NewTimer(topeT)
	defer tope.Stop()
	select {
	case <-hecho:
	case <-tope.C:
		log.Printf("resync: second phase still running after %s; opening the door anyway", topeT)
	}
	puertaResync.Abrir()
	resyncMu.Unlock()
}

// segundoTiempo es lo que se hace tras contestar. Se llama con resyncMu
// tomado y la puerta cerrada.
func segundoTiempo(restore string, gen uint64) {
	ini := time.Now()
	var t ResyncTiempos
	t0 := time.Now()
	if err := writeGeneration(generationFile, gen); err != nil {
		log.Printf("resync: writing %s: %v", generationFile, err)
	}
	t.FicheroGen = us(time.Since(t0))
	// Una copia de un dorado no es la misma máquina: machine-id propio.
	// Al descongelar la misma máquina no se toca.
	if restore == api.ResyncInstance {
		t0 = time.Now()
		renovarIdentidad()
		t.MachineID = us(time.Since(t0))
		// Los exec que corrían al volcar el dorado no son de esta copia
		// (cortarExecs). Al descongelar la misma máquina no se tocan: su
		// petición puede seguir viva, y la red los corta si no.
		t0 = time.Now()
		if n := cortarProcesos(); n > 0 {
			log.Printf("resync: stopped %d command(s) left running in the snapshot", n)
		}
		t.Exec = us(time.Since(t0))
		// Y los procesos de fondo y las sesiones: eran de otra máquina.
		t0 = time.Now()
		if n := limpiarFondo(); n > 0 {
			log.Printf("resync: stopped %d background process(es) left running in the snapshot", n)
		}
		t.Procesos = us(time.Since(t0))
	}
	t.DiferidoUS = us(time.Since(ini))
	guardarTiempos(func(u *ResyncTiempos) {
		u.FicheroGen, u.MachineID, u.Exec, u.Procesos, u.DiferidoUS, u.Diferido = t.FicheroGen, t.MachineID, t.Exec, t.Procesos, t.DiferidoUS, true
	})
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
	"/machine", api.GuestDropCachesPath, api.GuestProcessesPath, api.GuestSessionsPath}

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
