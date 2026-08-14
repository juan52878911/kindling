package main

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
//	  "env":      { "VAR_COMUN": "valor" },              // comunes a TODAS las sesiones
//	  "sessions": { "<Mcp-Session-Id>": { "TOKEN": "..." } }  // por sesión concreta
//	}
//
// El bridge, al lanzar el hijo de una sesión, añade a su entorno los pares "env"
// comunes MÁS los de "sessions[<su-id>]". Si no hay MMDS, no hay store, o no hay
// entrada, no añade nada: el comportamiento sin secretos queda intacto.

import (
	"encoding/json"
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

// mmdsStore es el documento JSON que sirve el metadata service.
type mmdsStore struct {
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
func setupMMDSRoute() {
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

// fetchMMDS lee el store completo del metadata service con el flujo v2. Devuelve
// nil sin ruido si MMDS no está disponible (la mayoría de servicios no usan
// secretos): un fallo aquí NO debe impedir arrancar la sesión.
func fetchMMDS() *mmdsStore {
	// Cliente de vida corta y plazo agresivo: el link-local es local, y si no hay
	// nadie sirviendo MMDS no queremos retrasar el arranque de cada sesión.
	client := &http.Client{Timeout: 2 * time.Second}

	// 1) Token de sesión (PUT). v2 lo exige para poder leer.
	treq, err := http.NewRequest(http.MethodPut, mmdsTokenURL, nil)
	if err != nil {
		return nil
	}
	treq.Header.Set("X-metadata-token-ttl-seconds", mmdsTokenTTL)
	tresp, err := client.Do(treq)
	if err != nil {
		// Sin MMDS configurado o sin ruta: silencioso, es el caso normal.
		return nil
	}
	defer tresp.Body.Close()
	if tresp.StatusCode != http.StatusOK {
		return nil
	}
	tok, err := io.ReadAll(io.LimitReader(tresp.Body, 4<<10))
	if err != nil || len(tok) == 0 {
		return nil
	}

	// 2) Store completo (GET /). Accept: application/json hace que MMDS devuelva
	// todo el árbol como JSON en vez de texto.
	greq, err := http.NewRequest(http.MethodGet, mmdsBase+"/", nil)
	if err != nil {
		return nil
	}
	greq.Header.Set("X-metadata-token", strings.TrimSpace(string(tok)))
	greq.Header.Set("Accept", "application/json")
	gresp, err := client.Do(greq)
	if err != nil {
		return nil
	}
	defer gresp.Body.Close()
	if gresp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(gresp.Body, 1<<20))
	if err != nil {
		return nil
	}
	var store mmdsStore
	if json.Unmarshal(body, &store) != nil {
		// El store existe pero no encaja con el esquema esperado: lo ignoramos en
		// vez de tumbar la sesión. Un aviso, porque sí indica un store mal formado.
		log.Printf("mmds: store present but doesn't follow the {env,sessions} schema; ignoring it")
		return nil
	}
	return &store
}

// sessionEnv construye el entorno del proceso de una sesión: el entorno base del
// puente MÁS los secretos de MMDS que le correspondan (los comunes "env" y los de
// "sessions[<id>]"). Si no hay MMDS o no hay entrada, devuelve el entorno base tal
// cual, así que el comportamiento sin secretos queda intacto.
//
// Se lee MMDS EN CADA sesión, no una vez al arrancar: el store puede cambiar entre
// sesiones (un secreto se inyecta después del boot, en la microVM ya viva), y una
// caché lo dejaría sin ver justo lo recién inyectado.
func (b *bridge) sessionEnv(id string) []string {
	store := fetchMMDS()
	if store == nil {
		return b.env
	}

	// Se parte del entorno base y se AÑADEN/PISAN las claves de MMDS. append sobre
	// una copia para no mutar b.env, que comparten todas las sesiones.
	extra := make(map[string]string)
	for k, v := range store.Env { // comunes a todas las sesiones
		extra[k] = v
	}
	for k, v := range store.Sessions[id] { // los de ESTA sesión pisan a los comunes
		extra[k] = v
	}
	if len(extra) == 0 {
		return b.env
	}

	env := append([]string(nil), b.env...)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	log.Printf("session %s: %d MMDS secret(s) injected into the environment", id[:8], len(extra))
	return env
}
