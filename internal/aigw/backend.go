package aigw

import (
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Este fichero es la SEMILLA de la ronda 8 (analisis/ronda8-ia/plan.md §1.1 y
// §2): solo declaraciones, cero comportamiento. Es el contrato que comparten las
// zonas; su dueño es Z1 (registro y validación). Si otra zona necesita cambiar
// una firma de aquí, lo pide a quien orquesta en vez de editarla.
//
// Un «backend de modelo» es un servidor HTTP con contrato declarado: un dorado
// que el planificador restaura (Snapshot) o una URL fija que kindling no arranca
// (Upstream). Los kinds de hoy (von, embed, chispa con backend microvm) son
// atajos que Config.Backend expande a este bloque; el ai.json viejo sigue
// valiendo tal cual.

// KindHTTP es el kind nuevo: un backend HTTP genérico descrito entero en su
// bloque de ai.json (port, contract, routes, health…). Lo valida Z1 y lo
// escribe en ai.json `kling ai model deploy` (Z5).
const KindHTTP = "http"

// Contratos de un backend: qué rutas reenvía el gateway por defecto y con qué
// forma de error. Z1 los valida y deriva Backend.Routes de ellos; Z2 elige por
// ellos la ruta pública (§1.2); Z5 los escribe en ai.json.
const (
	// ContractOpenAI: /v1/chat/completions y /v1/completions por el campo
	// "model"; aparece en /v1/models. Atajo de kind von.
	ContractOpenAI = "openai"
	// ContractOpenAIEmbeddings: /v1/embeddings por el campo "model". Atajo de
	// kind embed.
	ContractOpenAIEmbeddings = "openai-embeddings"
	// ContractChispaClassify: el contrato de la réplica de kling-chispa de hoy
	// (kind chispa, backend microvm). Lo usan /v1/classify y /v1/decide
	// clásicos; no se publica bajo /v1/m/.
	ContractChispaClassify = "chispa-classify"
	// ContractChispaDecide: chispa-server nuevo. Solo por /v1/m/{modelo}/…;
	// rutas por defecto POST /v1/decide y salud /healthz. Es solo un nombre
	// para «rutas y salud por defecto»: ninguna lógica de Chispa entra aquí.
	ContractChispaDecide = "chispa-decide"
	// ContractRaw: sin rutas por defecto; solo lo que declare "routes", bajo
	// /v1/m/{modelo}/….
	ContractRaw = "raw"
)

// Políticas de una réplica ociosa (§1.4). Los textos son los mismos que los de
// scheduler.IdlePolicy: Z4 convierte con scheduler.IdlePolicy(b.Idle).
const (
	// IdleRemove borra la réplica ociosa; la siguiente petición restaura del
	// dorado. Por defecto en http, von y embed en Linux.
	IdleRemove = "remove"
	// IdlePause la pausa dentro del presupuesto de pausadas y, si no cabe, la
	// borra. Nunca la congela.
	IdlePause = "pause"
	// IdleFreeze es lo de hoy: congelar. Por defecto en chispa-classify y en
	// macOS con vz.
	IdleFreeze = "freeze"
)

// DefaultBackendPort es el puerto de la réplica cuando el bloque no dice otro:
// el de VON (von.Port) y kling-chispa, el de siempre.
const DefaultBackendPort = 8000

// Backend es un modelo servido por HTTP ya EXPANDIDO y validado: lo que
// devuelve Config.Backend, con los valores por defecto aplicados (atajos
// incluidos). Lo leen Z2 (proxy y rutas), Z4 (ganchos del planificador) y el
// banco de Z6; nadie lo construye a mano fuera de Z1 salvo en tests.
//
// Invariantes que garantiza Z1 al devolverlo:
//   - exactamente uno de Snapshot y Upstream no está vacío;
//   - Port en 1-65535 (DefaultBackendPort si el bloque no lo da); con Upstream
//     no se usa (el puerto va en la URL);
//   - Contract es una de las constantes Contract*; Routes solo está vacía con
//     ContractChispaClassify;
//   - Idle es una de las constantes Idle*, ya con el defecto aplicado;
//   - Auth.File, si hay Auth, es absoluto y el fichero era 0600 al validar.
type Backend struct {
	// Name es la clave del bloque en "models" de ai.json.
	Name string
	// Snapshot es el dorado que restaura el planificador; también es el
	// "service" de pkg/scheduler (las funciones *For lo reciben así).
	Snapshot string
	// Upstream es una URL http/https fija que kindling no arranca (runner GPU,
	// otra instancia, una API), sin credenciales en ella. Excluyente con
	// Snapshot.
	Upstream string
	// Port es el puerto en el que escucha el servidor dentro de la réplica.
	Port int
	// Contract es una de las constantes Contract*.
	Contract string
	// Routes es la lista CERRADA de lo que se reenvía bajo /v1/m/{Name}/…:
	// lo que no esté aquí es 404 del gateway (Z2).
	Routes []Route
	// Health es la sonda: tras fabricar el dorado (Z5), tras RESTAURAR una
	// réplica (Z3, vía ReadyFor) y periódica para Upstream (Z2).
	Health Health
	// MaxInflight son las peticiones simultáneas por réplica antes de pedir
	// otra (Z3, vía MaxInflightFor); con Upstream, el tamaño del semáforo de
	// Z2. 0 = el -max-inflight del gateway.
	MaxInflight int
	// MaxReplicas acota las réplicas; 0 = la del gateway. Ya existe en
	// ModelConfig y en el planificador (MaxReplicasFor).
	MaxReplicas int
	// Idle es una de las constantes Idle*; Z4 lo pasa a IdleFor.
	Idle string
	// UpstreamModel, si no está vacío, es lo que Z2 escribe en el campo
	// "model" al reenviar a un backend openai* (vLLM rechaza nombres que no
	// sirve). Vacío = no se reescribe.
	UpstreamModel string
	// Auth es la credencial que AÑADE el gateway al reenviar; la del cliente
	// nunca se reenvía. nil = ninguna.
	Auth *Auth
	// Limits son los topes de un contrato openai; el cero no topa nada.
	Limits Limits
	// Tenants son los inquilinos (TenantConfig.Name) que pueden usar el
	// modelo; vacío = todos. El token principal siempre puede. Fuera de la
	// lista, Z2 responde 403.
	Tenants []string

	// Campos de la ronda 9 (plan-r9.md). Semilla: expandBackend los copia tal
	// cual; quien los valida y quien los lee va dicho en cada uno.

	// Spare son las réplicas de repuesto del modelo (C9-08): restauradas del
	// dorado y ya TOCADAS (con la petición de calentamiento del contrato)
	// fuera del camino de la petición. 0 = ninguna, lo de hoy. Solo con
	// Snapshot. Valida Z5 (0..MaxReplicas); lo lee Z4 vía scheduler.SpareFor.
	// Un repuesto no cuenta como réplica ocupada y se repone al reclamarlo.
	Spare int
	// Pin pide al daemon que ancle el mem.file del dorado en RAM (C9-10,
	// api.WarmSnapshotRequest.Pin). Solo con Snapshot. Lo lee Z4 al arrancar y
	// en cada Reload; con o sin Pin, Z4 precalienta la caché de todos los
	// dorados de ai.json.
	Pin bool
	// ReplicaAuth dice quién autentica las peticiones que llegan a la réplica
	// (pendiente 1 de la ronda 8). Es una de las constantes ReplicaAuth*; Z5
	// lo valida y, vacío, lo resuelve: ReplicaAuthKey si el bloque lleva Auth,
	// ReplicaAuthNetwork si no (lo de hoy en los dos casos). Con Upstream
	// queda vacío: no hay réplica (la credencial hacia la URL es Auth).
	ReplicaAuth string
	// Secrets son los secretos del backend ya validados por Z4
	// (validateSecrets), con File absoluto. Los entrega Z4; Z5 no los toca.
	Secrets []SecretConfig
	// Alias es el nombre común bajo el que se publican los bloques de varios
	// inquilinos (pendiente 4): POST /v1/m/<alias>/… resuelve al bloque cuyo
	// Tenants contiene al inquilino del token. Valida y resuelve Z5; lo
	// escribe Z3 con `kling ai model deploy -alias`.
	Alias string
	// Shared exime a un backend chispa-decide de la regla "exactamente un
	// inquilino" cuando ai.json declara inquilinos (pendiente 4). Valida Z5.
	Shared bool
	// LoadWait es el plazo de carga del modelo al fabricar o rehacer su
	// dorado (pendiente 3); 0 = automático por tamaño. Z5 valida el texto; lo
	// leen `kling ai prime` y `kling ai up` (Z3). El gateway no lo usa.
	LoadWait time.Duration
	// CAFile es el PEM (ruta absoluta tras cargar) con el que el gateway
	// verifica el TLS de Upstream (pendiente 5). Vacío = raíces del sistema.
	// Valida y usa Z5.
	CAFile string
}

// Modos de Backend.ReplicaAuth: quién es la frontera de una réplica.
//
// Decisión de la ronda 9: los dos modos son soportados y ninguno se retira.
// La réplica escucha en un puerto al que en Linux llega CUALQUIER proceso
// local del host, de cualquier uid (ni otra microVM ni la LAN), y en macOS es
// un reenvío en 127.0.0.1. Por eso:
//
//   - ReplicaAuthNetwork vale cuando el host es dedicado a kindling (nadie
//     más ejecuta código en él) y en desarrollo;
//   - ReplicaAuthKey es el modo para hosts compartidos o multiusuario, para
//     macOS fuera de desarrollo y siempre que el backend sepa autenticar.
const (
	// ReplicaAuthNetwork: la frontera es el gateway y la red del host. La
	// réplica corre sin claves de entrada; se puede calentar y sus claves no
	// se rotan porque no hay.
	ReplicaAuthNetwork = "network"
	// ReplicaAuthKey: la réplica exige una clave de entrada (su verificador va
	// en el dorado) y el gateway la añade con Auth. Exige el bloque Auth.
	ReplicaAuthKey = "key"
)

// Route es una ruta reenviable: método HTTP en mayúsculas y ruta absoluta sin
// ".." (Z1 lo valida). En ai.json se escribe como una cadena "POST /v1/decide".
type Route struct {
	Method string
	Path   string
}

// Health es la sonda de un backend ya resuelta (duraciones parseadas). Con
// Path vacío la sonda es el dial TCP de hoy.
type Health struct {
	// Path es la ruta GET de la sonda, p. ej. "/healthz".
	Path string
	// Expect es el código que da por buena la sonda; 0 = 200.
	Expect int
	// Timeout acota cada intento de la sonda; 0 = el plazo del llamante.
	Timeout time.Duration
	// Every es el periodo del sondeo de un Upstream (Z2: tres fallos
	// seguidos abren el circuito). Con Snapshot no hay sondeo periódico.
	Every time.Duration
	// Grace es cuánto se tolera, tras cargar o recargar, que un Upstream aún
	// no conteste a la sonda (un vLLM tarda minutos en cargar): durante la
	// gracia el gateway responde 503 con Retry-After en vez de 502 y no abre
	// el circuito. 0 = sin gracia, lo de hoy. Semilla de la ronda 9; Z5.
	Grace time.Duration
}

// HealthConfig es Health tal como se escribe en ai.json, con las duraciones
// como texto de time.ParseDuration ("5s"). Z1 lo convierte en Health.
type HealthConfig struct {
	Path    string `json:"path,omitempty"`
	Expect  int    `json:"expect,omitempty"`
	Timeout string `json:"timeout,omitempty"`
	Every   string `json:"every,omitempty"`
	// Grace: ver Health.Grace; solo con upstream.
	Grace string `json:"grace,omitempty"`
}

// Auth es la cabecera que el gateway añade al reenviar:
// Header: Scheme + " " + <contenido de File, sin espacios finales>; Scheme
// vacío = solo el contenido. File es relativo al ai.json en el fichero; Z1 lo
// hace absoluto y exige 0600. Lo lee Z2 al reenviar, no Z1 al cargar.
type Auth struct {
	Header string `json:"header"`
	Scheme string `json:"scheme,omitempty"`
	File   string `json:"file"`
}

// Limits son topes de un contrato openai que Z2 aplica a la petición antes de
// reenviarla (recorta o responde 400). 0 = sin tope.
type Limits struct {
	MaxTokens int `json:"max_tokens,omitempty"`
	N         int `json:"n,omitempty"`

	// Semilla de la ronda 9 (pendiente 5); los aplica Z5. 0 o vacío = sin tope.

	// TopLogprobs topa top_logprobs (y logprobs entero de completions).
	TopLogprobs int `json:"top_logprobs,omitempty"`
	// PromptLogprobs topa prompt_logprobs, la extensión de vLLM: un valor alto
	// multiplica el coste por token de entrada.
	PromptLogprobs int `json:"prompt_logprobs,omitempty"`
	// Timeout es el plazo de una petición a este modelo, como texto de
	// time.ParseDuration; vacío = el -proxy-timeout del gateway.
	Timeout string `json:"timeout,omitempty"`
}

// Backend devuelve el modelo nombre expandido a backend HTTP (atajos von,
// embed y chispa con backend microvm incluidos), o nil si no existe o no es un
// backend HTTP (chispa en proceso). El resultado es de solo lectura y se
// comparte entre peticiones; un Reload da un Config nuevo con backends nuevos.
//
// Busca por la clave de "models", no por el dorado: quien parta de un dorado
// recorre Backends.
//
// Tras Validate (ParseConfig y LoadConfig lo llaman) sale de lo calculado al
// validar, sin asignar; en un Config hecho a mano que nunca se validó se
// expande en el momento y no se guarda (así no hay escritura que compita con
// otros lectores).
func (c *Config) Backend(nombre string) *Backend {
	if c.backends != nil {
		return c.backends[nombre]
	}
	m := c.Models[nombre]
	if m == nil {
		return nil
	}
	return expandBackend(nombre, m)
}

// Backends devuelve todos los backends HTTP del registro, expandidos como en
// Backend y ordenados por Name. Lo usan Z2 (/v1/models completo, sondeo de
// upstreams) y Z4 (recalcular los ganchos del planificador en Reload y barrer
// réplicas de modelos retirados). El slice es nuevo en cada llamada; los
// *Backend, compartidos y de solo lectura.
func (c *Config) Backends() []*Backend {
	var out []*Backend
	for _, n := range sortedKeys(c.Models) {
		if b := c.Backend(n); b != nil {
			out = append(out, b)
		}
	}
	return out
}

// Topes de los campos de un backend. Como los del registro (config.go): el
// fichero lo escribe una persona y lo lee un servicio de red.
const (
	maxBackendInflight = 4096
	maxBackendRoutes   = 64
	maxRoutePath       = 512
	maxUpstreamURL     = 2048
	maxUpstreamModel   = 256
	maxLimitTokens     = 1 << 17
	maxLimitN          = 128
	minHealthEvery     = time.Second
	maxHealthEvery     = time.Hour
	maxHealthTimeout   = 5 * time.Minute
	// Ronda 9.
	maxHealthGrace   = time.Hour     // un vLLM grande carga en minutos, no en horas
	maxLimitTimeout  = time.Hour     // plazo de una petición (limits.timeout)
	maxLoadWait      = 6 * time.Hour // plazo de carga al fabricar un dorado
	maxLimitLogprobs = 1024          // top_logprobs / prompt_logprobs
	maxSpare         = 64            // como max_replicas
	maxCAFileBytes   = 1 << 20
)

// metodosRuta son los métodos que puede declarar "routes".
var metodosRuta = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
}

// cabecerasVetadas no las puede poner auth: las de salto a salto y las que
// fijan el encuadre o el destino de la petición reenviada.
var cabecerasVetadas = map[string]bool{
	"Host": true, "Content-Length": true, "Content-Type": true, "Transfer-Encoding": true,
	"Connection": true, "Keep-Alive": true, "Upgrade": true, "Te": true, "Trailer": true,
	"Proxy-Authorization": true, "Proxy-Connection": true, "Cookie": true, "X-Request-Id": true,
}

// esBackendHTTP dice si m se expande a Backend: todo lo que se sirve por HTTP
// (réplicas y upstreams), es decir, todo menos Chispa en proceso.
func (m *ModelConfig) esBackendHTTP() bool {
	return m.Kind == KindHTTP || m.isReplica()
}

// contratoDe es el contrato de m: el declarado en kind http, el implícito en
// los atajos.
func contratoDe(m *ModelConfig) string {
	switch m.Kind {
	case KindVON:
		return ContractOpenAI
	case KindEmbed:
		return ContractOpenAIEmbeddings
	case KindChispa:
		return ContractChispaClassify
	}
	return m.Contract
}

// rutasDeContrato son las rutas por defecto de un contrato (slice nuevo en
// cada llamada: cada Backend tiene el suyo).
func rutasDeContrato(contrato string) []Route {
	switch contrato {
	case ContractOpenAI:
		return []Route{{http.MethodPost, "/v1/chat/completions"}, {http.MethodPost, "/v1/completions"}}
	case ContractOpenAIEmbeddings:
		return []Route{{http.MethodPost, "/v1/embeddings"}}
	case ContractChispaDecide:
		return []Route{{http.MethodPost, "/v1/decide"}}
	}
	return nil
}

// saludDeContrato es la ruta de la sonda por defecto; "" = dial TCP.
func saludDeContrato(contrato string) string {
	switch contrato {
	case ContractOpenAI, ContractOpenAIEmbeddings:
		return "/health" // llama-server, VON y vLLM
	case ContractChispaDecide:
		return "/healthz" // chispa-server
	}
	return ""
}

// expandBackend construye el Backend de un modelo YA validado (los errores de
// parseo no pueden darse aquí; si se dieran, el campo queda en su cero). nil
// si m no es un backend HTTP.
func expandBackend(nombre string, m *ModelConfig) *Backend {
	if !m.esBackendHTTP() {
		return nil
	}
	b := &Backend{
		Name: nombre, Snapshot: m.Snapshot, Upstream: m.Upstream, Port: m.Port,
		Contract: contratoDe(m), MaxInflight: m.MaxInflight, MaxReplicas: m.MaxReplicas,
		Idle: m.Idle, UpstreamModel: m.UpstreamModel,
	}
	if b.Port == 0 {
		b.Port = DefaultBackendPort
	}
	if len(m.Routes) > 0 {
		for _, s := range m.Routes {
			if r, err := parseRoute(s); err == nil {
				b.Routes = append(b.Routes, r)
			}
		}
	} else {
		b.Routes = rutasDeContrato(b.Contract)
	}
	if m.Health != nil {
		b.Health, _ = parseHealth(m.Health)
	}
	if b.Health.Path == "" {
		b.Health.Path = saludDeContrato(b.Contract)
	}
	if b.Upstream == "" {
		// Decisión de la ronda 9 (plan-r9.md, «frontera de la réplica»): vacío
		// es lo de hoy, deducido del bloque.
		switch {
		case m.ReplicaAuth != "":
			b.ReplicaAuth = m.ReplicaAuth
		case m.Auth != nil:
			b.ReplicaAuth = ReplicaAuthKey
		default:
			b.ReplicaAuth = ReplicaAuthNetwork
		}
	}
	if b.Idle == "" {
		// Decisión de la ronda 8 (§1.4): borrar lo ocioso para que toda réplica
		// viva comparta el mem.file del dorado; chispa-classify sigue congelando
		// (128 MiB y un thaw de 2 ms son su gracia).
		b.Idle = IdleRemove
		if b.Contract == ContractChispaClassify {
			b.Idle = IdleFreeze
		}
	}
	if m.Auth != nil {
		a := *m.Auth
		b.Auth = &a
	}
	if m.Limits != nil {
		b.Limits = *m.Limits
	}
	if len(m.Tenants) > 0 {
		b.Tenants = append([]string(nil), m.Tenants...)
	}
	// Ronda 9: ya validados; CAFile llega absoluto (resolveAuthFiles).
	b.Spare, b.Pin = m.Spare, m.Pin
	b.Alias, b.Shared, b.CAFile = m.Alias, m.Shared, m.CAFile
	b.LoadWait, _ = time.ParseDuration(m.LoadWait)
	if len(m.Secrets) > 0 {
		b.Secrets = append([]SecretConfig(nil), m.Secrets...)
	}
	return b
}

// parseRoute lee "MÉTODO /ruta". La ruta es una lista cerrada que el gateway
// compara tal cual, así que se exige su forma canónica: absoluta, sin
// segmentos . ni .., sin // ni escapes % (que el backend podría decodificar a
// otra ruta), sin query ni fragmento, y con un juego de caracteres corto.
func parseRoute(s string) (Route, error) {
	metodo, ruta, ok := strings.Cut(s, " ")
	if !ok || !metodosRuta[metodo] {
		return Route{}, fmt.Errorf("route %q: needs \"METHOD /path\" with METHOD one of GET, HEAD, POST, PUT, PATCH, DELETE", s)
	}
	if err := checkPath(ruta); err != nil {
		return Route{}, fmt.Errorf("route %q: %w", s, err)
	}
	return Route{Method: metodo, Path: ruta}, nil
}

// checkPath valida una ruta de routes o de health.
func checkPath(p string) error {
	if !strings.HasPrefix(p, "/") || len(p) > maxRoutePath {
		return fmt.Errorf("path must start with / and be at most %d bytes", maxRoutePath)
	}
	for i := 0; i < len(p); i++ {
		ch := p[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.IndexByte("/._~-:@+,;=", ch) >= 0) {
			return fmt.Errorf("path has %q (allowed: letters, digits and / . _ ~ - : @ + , ; =)", ch)
		}
	}
	if p == "/" {
		return nil
	}
	segs := strings.Split(strings.TrimSuffix(p[1:], "/"), "/")
	for _, sg := range segs {
		if sg == "" || sg == "." || sg == ".." {
			return fmt.Errorf("path must be canonical (no empty, . or .. segments)")
		}
	}
	return nil
}

// parseHealth convierte la forma de fichero en Health.
func parseHealth(h *HealthConfig) (Health, error) {
	out := Health{Path: h.Path, Expect: h.Expect}
	var errs []error
	if h.Path != "" {
		if err := checkPath(h.Path); err != nil {
			errs = append(errs, fmt.Errorf("health.path: %w", err))
		}
	}
	if h.Expect != 0 && (h.Expect < 100 || h.Expect > 599) {
		errs = append(errs, fmt.Errorf("health.expect must be an HTTP status (100-599)"))
	}
	if h.Timeout != "" {
		d, err := time.ParseDuration(h.Timeout)
		if err != nil || d <= 0 || d > maxHealthTimeout {
			errs = append(errs, fmt.Errorf("health.timeout must be a duration in (0, %s]", maxHealthTimeout))
		}
		out.Timeout = d
	}
	if h.Every != "" {
		d, err := time.ParseDuration(h.Every)
		if err != nil || d < minHealthEvery || d > maxHealthEvery {
			errs = append(errs, fmt.Errorf("health.every must be a duration in [%s, %s]", minHealthEvery, maxHealthEvery))
		}
		out.Every = d
	}
	if h.Grace != "" {
		d, err := duracionEn("health.grace", h.Grace, maxHealthGrace)
		if err != nil {
			errs = append(errs, err)
		}
		out.Grace = d
	}
	return out, errors.Join(errs...)
}

// duracionEn lee s como duración en (0, max]; campo nombra el error.
func duracionEn(campo, s string, max time.Duration) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 || d > max {
		return 0, fmt.Errorf("%s must be a duration in (0, %s], like \"90s\" or \"10m\"", campo, max)
	}
	return d, nil
}

// checkUpstream: URL http/https con host y sin credenciales. La query y el
// fragmento también se rechazan: ahí suelen ir claves de API, y la credencial
// va en auth (un fichero 0600), no en ai.json.
func checkUpstream(s string) error {
	if len(s) > maxUpstreamURL {
		return fmt.Errorf("upstream longer than %d bytes", maxUpstreamURL)
	}
	u, err := url.Parse(s)
	if err != nil {
		return fmt.Errorf("upstream is not a URL")
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return fmt.Errorf("upstream must be http:// or https://")
	case u.Opaque != "" || u.Hostname() == "":
		return fmt.Errorf("upstream needs a host")
	case u.User != nil:
		return fmt.Errorf("upstream must not carry credentials (use auth with a 0600 file)")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "":
		return fmt.Errorf("upstream must not have a query or fragment (credentials go in auth)")
	}
	// Destino (pendiente 5): las mismas prohibiciones que un upstream fijado
	// del proxy de credenciales. Aquí solo se ve una IP literal; un nombre se
	// comprueba al marcar (destinoVetado en el Dialer del upstream).
	host := u.Hostname()
	puerto, _ := strconv.Atoi(u.Port())
	if puerto == 0 {
		puerto = 80
		if u.Scheme == "https" {
			puerto = 443
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if credproxy.UpstreamIPProhibida(ip) {
			return fmt.Errorf("upstream %s is a forbidden destination (link-local, cloud metadata, multicast or kindling's own network)", host)
		}
		if ip.Unmap().IsLoopback() && credproxy.PuertoReservado(puerto) {
			return errReenvio(puerto)
		}
	} else if strings.EqualFold(host, "localhost") && credproxy.PuertoReservado(puerto) {
		return errReenvio(puerto)
	}
	return nil
}

func errReenvio(puerto int) error {
	return fmt.Errorf("upstream port %d on loopback is in kindling's reserved forward range (%d-%d, where machines' guests are exposed)",
		puerto, credproxy.ForwardPortMin, credproxy.ForwardPortMax)
}

// destinoVetado dice si marcar address ("ip:puerto", ya resuelto) iría a un
// destino prohibido. Lo llama el Dialer de un upstream en cada conexión, así
// que un nombre que resuelva (o pase a resolver) ahí tampoco llega.
func destinoVetado(address string) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return fmt.Errorf("upstream dial to %q: not an IP address", address)
	}
	ip := ap.Addr().Unmap()
	if credproxy.UpstreamIPProhibida(ip) || (ip.IsLoopback() && credproxy.PuertoReservado(int(ap.Port()))) {
		return fmt.Errorf("upstream dial to %s: forbidden destination (link-local, metadata, kindling's network or its forward range)", address)
	}
	return nil
}

// esLoopback dice si el host de una URL es el loopback (nombre o IP).
func esLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// cargarCA lee el PEM de p (absoluto) como conjunto de raíces.
func cargarCA(p string) (*x509.CertPool, error) {
	if !filepath.IsAbs(p) {
		return nil, fmt.Errorf("ca_file %q is relative: it is resolved against the config file, so load it with LoadConfig", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("ca_file: %w", err)
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("ca_file %s is not a regular file", p)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxCAFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("ca_file: %w", err)
	}
	pool := x509.NewCertPool()
	if len(raw) > maxCAFileBytes || !pool.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("ca_file %s has no PEM certificate (or is larger than %d bytes)", p, maxCAFileBytes)
	}
	return pool, nil
}

// esToken dice si s es un token de RFC 9110 (nombre de cabecera, esquema).
func esToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.IndexByte("!#$%&'*+-.^_`|~", ch) >= 0) {
			return false
		}
	}
	return true
}

// checkAuth valida auth; File ya llega absoluto si el registro vino de
// LoadConfig (resolveAuthFiles). El fichero tiene que ser regular y no
// legible ni escribible por grupo u otros, como ai.token.
func checkAuth(a *Auth) []error {
	var errs []error
	if !esToken(a.Header) || cabecerasVetadas[textproto.CanonicalMIMEHeaderKey(a.Header)] {
		errs = append(errs, fmt.Errorf("auth.header must be a valid header name, not a hop-by-hop or framing one"))
	}
	if a.Scheme != "" && (!esToken(a.Scheme) || len(a.Scheme) > 64) {
		errs = append(errs, fmt.Errorf("auth.scheme must be a single word"))
	}
	switch {
	case a.File == "":
		errs = append(errs, fmt.Errorf("auth.file is required"))
	case !filepath.IsAbs(a.File):
		errs = append(errs, fmt.Errorf("auth.file %q is relative: it is resolved against the config file, so load it with LoadConfig", a.File))
	default:
		fi, err := os.Stat(a.File)
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("auth.file: %w", err))
		case !fi.Mode().IsRegular():
			errs = append(errs, fmt.Errorf("auth.file %s is not a regular file", a.File))
		case fi.Mode().Perm()&0o077 != 0:
			errs = append(errs, fmt.Errorf("auth.file %s must be 0600 (it is %04o)", a.File, fi.Mode().Perm()))
		}
	}
	return errs
}

// validateBackend comprueba los campos de la ronda 8 de un modelo (el kind,
// path, backend y max_replicas ya los mira Validate). Cada atajo admite solo lo
// que tiene sentido para él: una clave que no hace nada es un error que no se
// ve.
func validateBackend(n string, m *ModelConfig, tenants map[string]bool) []error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("model %q: "+format, append([]any{n}, a...)...))
	}
	nuevos := map[string]bool{
		"upstream": m.Upstream != "", "port": m.Port != 0, "contract": m.Contract != "",
		"routes": len(m.Routes) > 0, "health": m.Health != nil, "max_inflight": m.MaxInflight != 0,
		"idle": m.Idle != "", "upstream_model": m.UpstreamModel != "", "auth": m.Auth != nil,
		"limits": m.Limits != nil, "tenants": len(m.Tenants) > 0,
		// Ronda 9.
		"spare": m.Spare != 0, "pin": m.Pin, "replica_auth": m.ReplicaAuth != "", "alias": m.Alias != "",
		"shared": m.Shared, "load_wait": m.LoadWait != "", "ca_file": m.CAFile != "",
	}
	var admitidos map[string]bool
	switch {
	case m.Kind == KindHTTP:
		admitidos = nil // todo
	case m.Kind == KindVON:
		admitidos = map[string]bool{"health": true, "max_inflight": true, "idle": true, "tenants": true, "limits": true,
			"spare": true, "pin": true, "replica_auth": true, "load_wait": true}
	case m.Kind == KindEmbed:
		admitidos = map[string]bool{"health": true, "max_inflight": true, "idle": true, "tenants": true,
			"spare": true, "pin": true, "replica_auth": true, "load_wait": true}
	case m.Kind == KindChispa && m.Backend == BackendMicroVM:
		admitidos = map[string]bool{"health": true, "max_inflight": true, "idle": true,
			"spare": true, "pin": true, "replica_auth": true}
	default:
		admitidos = map[string]bool{} // chispa en proceso: ninguno
	}
	if admitidos != nil {
		var sobran []string
		for _, k := range sortedKeys(nuevos) {
			if nuevos[k] && !admitidos[k] {
				sobran = append(sobran, k)
			}
		}
		if len(sobran) > 0 {
			kind := m.Kind
			if m.Backend != "" {
				kind += " with backend " + m.Backend
			}
			add("%s: not for kind %s (use kind http for a backend described in full)", strings.Join(sobran, ", "), kind)
			return errs
		}
	}
	contrato := contratoDe(m)
	if m.Kind == KindHTTP {
		if m.Path != "" || m.Backend != "" {
			add("path and backend are for kind chispa")
		}
		switch {
		case (m.Snapshot == "") == (m.Upstream == ""):
			add("kind http needs exactly one of snapshot (a golden the gateway restores) and upstream (a fixed URL)")
		case m.Upstream != "":
			if err := checkUpstream(m.Upstream); err != nil {
				add("%v", err)
			}
			// Lo que gobierna réplicas no aplica a una URL que kindling no arranca.
			if m.Port != 0 || m.Idle != "" || m.MaxReplicas != 0 {
				add("port, idle and max_replicas apply to a snapshot, not to an upstream (the port goes in the URL)")
			}
		}
		switch contrato {
		case ContractOpenAI, ContractOpenAIEmbeddings, ContractChispaDecide, ContractRaw:
		case "":
			add("kind http needs a contract: openai, openai-embeddings, chispa-decide or raw")
		case ContractChispaClassify:
			add("contract chispa-classify is the kind chispa replica: use kind chispa with backend microvm")
		default:
			add("contract must be openai, openai-embeddings, chispa-decide or raw, not %q", contrato)
		}
		if m.Port < 0 || m.Port > 65535 {
			add("port must be 1-65535")
		}
		if len(m.Routes) > maxBackendRoutes {
			add("at most %d routes", maxBackendRoutes)
		}
		visto := map[Route]bool{}
		for _, s := range m.Routes {
			r, err := parseRoute(s)
			if err != nil {
				add("%v", err)
				continue
			}
			if visto[r] {
				add("route %q repeated", s)
			}
			visto[r] = true
		}
		if contrato == ContractRaw && len(m.Routes) == 0 {
			add("contract raw has no default routes: declare them in routes")
		}
		if m.UpstreamModel != "" {
			if contrato != ContractOpenAI && contrato != ContractOpenAIEmbeddings {
				add("upstream_model is for openai contracts (it rewrites the \"model\" field)")
			}
			if len(m.UpstreamModel) > maxUpstreamModel || strings.IndexFunc(m.UpstreamModel, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
				add("upstream_model must be at most %d bytes without control characters", maxUpstreamModel)
			}
		}
		if m.Auth != nil {
			for _, err := range checkAuth(m.Auth) {
				add("%v", err)
			}
		}
	}
	if m.Health != nil {
		if _, err := parseHealth(m.Health); err != nil {
			add("%v", err)
		}
		if m.Health.Every != "" && m.Upstream == "" {
			add("health.every polls an upstream; a snapshot is probed after each restore")
		}
		if m.Health.Grace != "" && m.Upstream == "" {
			add("health.grace is for an upstream that takes long to load; a snapshot is probed after each restore")
		}
	}
	if m.MaxInflight < 0 || m.MaxInflight > maxBackendInflight {
		add("max_inflight must be 0..%d", maxBackendInflight)
	}
	switch m.Idle {
	case "", IdleRemove, IdlePause, IdleFreeze:
	default:
		add("idle must be remove, pause or freeze, not %q", m.Idle)
	}
	if l := m.Limits; l != nil {
		// timeout vale en cualquier contrato; el resto topa campos de OpenAI.
		if contrato != ContractOpenAI && (l.MaxTokens != 0 || l.N != 0 || l.TopLogprobs != 0 || l.PromptLogprobs != 0) {
			add("limits max_tokens, n, top_logprobs and prompt_logprobs apply to the openai contract (only limits.timeout applies to any)")
		}
		if l.MaxTokens < 0 || l.MaxTokens > maxLimitTokens || l.N < 0 || l.N > maxLimitN {
			add("limits.max_tokens must be 0..%d and limits.n 0..%d", maxLimitTokens, maxLimitN)
		}
		if l.TopLogprobs < 0 || l.TopLogprobs > maxLimitLogprobs || l.PromptLogprobs < 0 || l.PromptLogprobs > maxLimitLogprobs {
			add("limits.top_logprobs and limits.prompt_logprobs must be 0..%d", maxLimitLogprobs)
		}
		if l.Timeout != "" {
			if _, err := duracionEn("limits.timeout", l.Timeout, maxLimitTimeout); err != nil {
				add("%v", err)
			}
		}
	}
	errs = append(errs, validarRonda9(n, m)...)
	visto := map[string]bool{}
	for _, tn := range m.Tenants {
		if !tenants[tn] {
			add("tenant %q is not in \"tenants\"", tn)
		}
		if visto[tn] {
			add("tenant %q repeated", tn)
		}
		visto[tn] = true
	}
	return errs
}

// validarRonda9 comprueba spare, pin, load_wait, replica_auth y ca_file de un
// modelo que ya pasó el filtro de campos por kind. alias y shared, que
// dependen de otros bloques, los mira validateAliases (alias.go).
func validarRonda9(n string, m *ModelConfig) []error {
	var errs []error
	add := func(format string, a ...any) {
		errs = append(errs, fmt.Errorf("model %q: "+format, append([]any{n}, a...)...))
	}
	dorado := m.Snapshot != ""
	if m.Spare < 0 || m.Spare > maxSpare {
		add("spare must be 0..%d", maxSpare)
	} else if m.MaxReplicas > 0 && m.Spare > m.MaxReplicas {
		add("spare (%d) cannot be more than max_replicas (%d): a spare is a replica", m.Spare, m.MaxReplicas)
	}
	if (m.Spare != 0 || m.Pin) && !dorado {
		add("spare and pin apply to a snapshot (replicas restored from a golden), not to an upstream")
	}
	if m.LoadWait != "" {
		if !dorado {
			add("load_wait is the load deadline when building a snapshot; it does not apply here")
		} else if _, err := duracionEn("load_wait", m.LoadWait, maxLoadWait); err != nil {
			add("%v", err)
		}
	}
	switch m.ReplicaAuth {
	case "":
	case ReplicaAuthNetwork, ReplicaAuthKey:
		switch {
		case !dorado:
			add("replica_auth is about a replica's port; an upstream has none (its credential is auth)")
		case m.ReplicaAuth == ReplicaAuthKey && m.Auth == nil:
			add("replica_auth key needs auth: the key the gateway adds and the replica checks")
		case m.ReplicaAuth == ReplicaAuthNetwork && m.Auth != nil:
			add("replica_auth network contradicts auth (the replica would check a key): drop one of them")
		}
	default:
		add("replica_auth must be network or key, not %q", m.ReplicaAuth)
	}
	if m.CAFile != "" {
		if u, err := url.Parse(m.Upstream); m.Upstream == "" || err != nil || u.Scheme != "https" {
			add("ca_file verifies the TLS of an https:// upstream")
		} else if _, err := cargarCA(m.CAFile); err != nil {
			add("%v", err)
		}
	}
	return errs
}

// BackendNotes son los avisos (no errores) del registro para un gateway que
// corre en goos (runtime.GOOS): la frontera de las réplicas en modo network
// (plan-r9.md, «Decisión de seguridad») y las credenciales que cruzan la red
// en claro. Un registro de hoy en Linux sin inquilinos no da ninguno.
func (c *Config) BackendNotes(goos string) []string {
	var red, out []string
	for _, b := range c.Backends() {
		if b.Snapshot != "" && b.ReplicaAuth == ReplicaAuthNetwork {
			red = append(red, strconv.Quote(b.Name))
		}
		if b.Upstream != "" && b.Auth != nil {
			if u, err := url.Parse(b.Upstream); err == nil && u.Scheme == "http" && !esLoopback(u.Hostname()) {
				out = append(out, fmt.Sprintf("model %q: upstream %s is http:// off loopback, so its auth credential crosses the network in clear; use https:// (with ca_file for a private CA)", b.Name, u.Host))
			}
		}
	}
	if len(red) > 0 {
		lista := strings.Join(red, ", ")
		switch {
		case goos == "darwin":
			out = append(out, fmt.Sprintf("models %s: replica_auth network on macOS: each replica is a forward on 127.0.0.1 that any process on this Mac can reach; fine for development, otherwise use replica_auth key (auth with a key the replica checks)", lista))
		case len(c.Tenants) > 0:
			out = append(out, fmt.Sprintf("models %s: replica_auth network with tenants: any local process of any user on this host reaches the replica's port, bypassing the gateway; use replica_auth key if the host is not dedicated to kindling", lista))
		}
	}
	return out
}

// resolveAuthFiles hace absolutos auth.file, ca_file y secrets[].file
// respecto a dir (el directorio del ai.json). Con dir "" (ParseConfig sin
// fichero) los deja como están, y Validate rechaza una ruta relativa.
func (c *Config) resolveAuthFiles(dir string) {
	if dir == "" {
		return
	}
	rel := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	for _, m := range c.Models {
		if m == nil {
			continue
		}
		if m.Auth != nil {
			rel(&m.Auth.File)
		}
		rel(&m.CAFile)
		for i := range m.Secrets {
			rel(&m.Secrets[i].File)
		}
	}
}
