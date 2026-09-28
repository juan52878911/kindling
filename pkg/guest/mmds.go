package guest

// Secretos de sesión por MMDS (el metadata service de Firecracker).
//
// MODELO DE AMENAZA. El invitado es hostil, pero el secreto es SUYO (de esa
// sesión): un token, una credencial. Lo que se busca es que ese secreto NO quede
// grabado en la imagen compartida ni en el snapshot dorado —artefactos que usan
// todas las instancias—, sino que se inyecte en la microVM ya VIVA y solo viva en
// su RAM mientras dure. Por eso el daemon se niega a congelar una máquina con
// secretos inyectados: congelar volcaría esa RAM a un mem.file compartido.
//
// FLUJO v2. MMDS v2 exige un token de sesión para leer (v1 dejaba leer sin
// credencial, y como el gateway reenvía peticiones a los invitados, un canal de
// metadatos legible sin token es una fuga esperando a ocurrir):
//
//	PUT http://169.254.169.254/latest/api/token   (X-metadata-token-ttl-seconds: N)  -> token
//	GET http://169.254.169.254/                    (X-metadata-token: <token>, Accept: json) -> store
//
// ESQUEMA DEL STORE. Un único documento JSON:
//
//	{
//	  "env":      { "VAR_COMUN": "valor" }              // comunes a TODAS las sesiones
//	}
//
// El bridge, al lanzar el hijo de una sesión, añade a su entorno los pares "env"
// comunes. Si no hay MMDS, no hay store, o no hay entrada, no añade nada: el
// comportamiento sin secretos queda intacto.
//
// CAMPO "sessions" RETIRADO (no es seguro). Antes guardaba secretos por sesión
// (ej: "sessions": { "<Mcp-Session-Id>": { "TOKEN": "..." } }), pero el id de sesión
// se acuña en el bridge DENTRO del invitado justo al lanzar el hijo, y el bridge
// (PID 1, root) y el servidor MCP corren como root y leen el almacén completo.
// Resultado: una sesión podría leer los secretos de todas las otras.
//
// ALTERNATIVAS SEGURAS:
// - VM efímera por sesión: cada sesión corre en su propia microVM, aislada.
// - Credenciales de plantilla: usar `kling template credential` para inyectar
//   credenciales vinculadas a la plantilla, no a la sesión: todas las sesiones
//   de una máquina las ven, pero es seguro si la máquina es efímera.
// - Proxy de credenciales: si el invitado tiene egress allowlist, usar
//   `kling machine credential` para entregar claves solo en peticiones HTTP
//   a dominios específicos (el proxy no es el invitado y solo sabe de esa máquina).
//
// El bridge sigue ACEPTANDO "sessions" en el esquema (para atrás-compatible) pero
// no lo usa: si encuentra sessions no vacío, avisa UNA VEZ al iniciar la máquina.

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

const (
	// mmdsIP es la link-local estándar del metadata service.
	mmdsBase     = "http://169.254.169.254"
	mmdsTokenURL = mmdsBase + "/latest/api/token"
	// mmdsTokenTTL es el plazo de vida del token de sesión. Corto a propósito: se
	// pide uno nuevo en cada lectura, así que no hace falta que dure.
	mmdsTokenTTL = "60"
)

// MMDSStore es el documento JSON que sirve el metadata service.
type MMDSStore struct {
	Env      map[string]string            `json:"env"`
	Sessions map[string]map[string]string `json:"sessions"`
}

// setupMMDSRoute añade la ruta a 169.254.169.254 por eth0. El bridge es PID 1 y
// corre como root dentro del invitado, así que puede tocar la tabla de rutas.
//
// PORQUÉ. El kernel del invitado configura la red por cmdline (ip=...:off), que
// deja una ruta por defecto vía la gateway. Los paquetes a 169.254.169.254 saldrían
// por esa ruta, pero una ruta /32 ON-LINK por eth0 es lo fiable: hace que el
// invitado haga ARP directamente por 169.254.169.254, que es a quien Firecracker
// responde interceptando en el dispositivo virtio-net.
//
// Best-effort: si falla (imagen mínima sin `ip`, kernel sin la ruta), se avisa y se
// sigue. Sin ruta, la lectura de MMDS fallará limpio en fetchMMDS y la sesión
// arranca sin secretos, como hasta ahora.
//
// VALIDACIÓN EN LAB. Esta es la pieza MÁS incierta del flujo: depende de que la
// imagen traiga `iproute2` y de que la ruta on-link baste para que Firecracker
// intercepte. Hay que confirmarlo en hardware; ver el reporte de la tarea.
func SetupMMDSRoute() {
	if _, err := exec.LookPath("ip"); err != nil {
		// La imagen mínima puede no traer iproute2. No hay un /sys equivalente para
		// añadir rutas (no viven ahí), así que aquí no hay plan B limpio: se deja
		// constancia para que se resuelva en el build de la imagen o se valide que
		// la ruta por defecto basta.
		log.Printf("mmds: cannot find `ip` in the guest; cannot set the route to 169.254.169.254 "+
			"(if the service doesn't use MMDS secrets, this is harmless): %v", err)
		return
	}
	// ip route add 169.254.169.254/32 dev eth0
	out, err := exec.Command("ip", "route", "add", "169.254.169.254/32", "dev", "eth0").CombinedOutput()
	if err != nil {
		// "File exists" si ya estaba: no es un fallo real. Cualquier otra cosa se
		// registra para diagnóstico en el lab.
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "File exists") {
			return
		}
		log.Printf("mmds: could not set the route to 169.254.169.254 via eth0 (%v: %s); "+
			"if this service doesn't use MMDS secrets, this is harmless", err, msg)
		return
	}
	log.Printf("mmds: route to 169.254.169.254 via eth0 ready")
}

// FetchMMDS lee el store completo del metadata service con el flujo v2.
//
// Distingue dos casos que antes se confundían: (nil, nil) es "MMDS responde pero
// no hay store" (lo normal en un servicio sin secretos), y (nil, err) es "no se
// pudo leer". El segundo NO es silencioso para quien llama: un secreto inyectado
// que no se ve se convierte en una sesión que arranca sin él sin que nadie lo
// note. Un fallo aquí tampoco impide arrancar la sesión.
func FetchMMDS() (*MMDSStore, error) {
	// Cliente de vida corta y plazo agresivo: el link-local es local, y si no hay
	// nadie sirviendo MMDS no queremos retrasar el arranque de cada sesión. Sin
	// proxy: el metadata service nunca está detrás de uno.
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
	}

	// 1) Token de sesión (PUT). v2 lo exige para poder leer.
	treq, err := http.NewRequest(http.MethodPut, mmdsTokenURL, nil)
	if err != nil {
		return nil, err
	}
	treq.Header.Set("X-metadata-token-ttl-seconds", mmdsTokenTTL)
	tresp, err := client.Do(treq)
	if err != nil {
		return nil, fmt.Errorf("token: %w", err)
	}
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token: HTTP %d", tresp.StatusCode)
	}
	tok, err := io.ReadAll(io.LimitReader(tresp.Body, 4<<10))
	if err != nil || len(tok) == 0 {
		return nil, fmt.Errorf("token: empty or unreadable (%v)", err)
	}

	// 2) Store completo (GET /). Accept: application/json hace que MMDS devuelva
	// todo el árbol como JSON en vez de texto.
	greq, err := http.NewRequest(http.MethodGet, mmdsBase+"/", nil)
	if err != nil {
		return nil, err
	}
	greq.Header.Set("X-metadata-token", strings.TrimSpace(string(tok)))
	greq.Header.Set("Accept", "application/json")
	gresp, err := client.Do(greq)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	defer gresp.Body.Close()
	if gresp.StatusCode == http.StatusNotFound {
		return nil, nil // MMDS vivo, store vacío: nada que inyectar
	}
	if gresp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("store: HTTP %d", gresp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(gresp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	var store MMDSStore
	if err := json.Unmarshal(body, &store); err != nil {
		// El store existe pero no encaja con el esquema esperado: lo ignoramos en
		// vez de tumbar la sesión. Un aviso, porque sí indica un store mal formado.
		log.Printf("mmds: store present but doesn't follow the {env,sessions} schema; ignoring it")
		return nil, nil
	}
	return &store, nil
}
