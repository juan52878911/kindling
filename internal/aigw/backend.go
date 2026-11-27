package aigw

import (
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
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
}

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
}

// HealthConfig es Health tal como se escribe en ai.json, con las duraciones
// como texto de time.ParseDuration ("5s"). Z1 lo convierte en Health.
type HealthConfig struct {
	Path    string `json:"path,omitempty"`
	Expect  int    `json:"expect,omitempty"`
	Timeout string `json:"timeout,omitempty"`
	Every   string `json:"every,omitempty"`
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
	return out, errors.Join(errs...)
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
	return nil
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
	}
	var admitidos map[string]bool
	switch {
	case m.Kind == KindHTTP:
		admitidos = nil // todo
	case m.Kind == KindVON:
		admitidos = map[string]bool{"health": true, "max_inflight": true, "idle": true, "tenants": true, "limits": true}
	case m.Kind == KindEmbed:
		admitidos = map[string]bool{"health": true, "max_inflight": true, "idle": true, "tenants": true}
	case m.Kind == KindChispa && m.Backend == BackendMicroVM:
		admitidos = map[string]bool{"health": true, "max_inflight": true, "idle": true}
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
		if contrato != ContractOpenAI {
			add("limits apply to the openai contract (max_tokens and n of a completion)")
		}
		if l.MaxTokens < 0 || l.MaxTokens > maxLimitTokens || l.N < 0 || l.N > maxLimitN {
			add("limits.max_tokens must be 0..%d and limits.n 0..%d", maxLimitTokens, maxLimitN)
		}
	}
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

// resolveAuthFiles hace absoluto auth.file respecto a dir (el directorio del
// ai.json). Con dir "" (ParseConfig sin fichero) lo deja como está, y
// Validate rechaza una ruta relativa.
func (c *Config) resolveAuthFiles(dir string) {
	if dir == "" {
		return
	}
	for _, m := range c.Models {
		if m != nil && m.Auth != nil && m.Auth.File != "" && !filepath.IsAbs(m.Auth.File) {
			m.Auth.File = filepath.Join(dir, m.Auth.File)
		}
	}
}
