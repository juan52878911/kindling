package aigw

import "time"

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
// Semilla: devuelve nil. Lo implementa Z1.
func (c *Config) Backend(nombre string) *Backend {
	return nil
}

// Backends devuelve todos los backends HTTP del registro, expandidos como en
// Backend y ordenados por Name. Lo usan Z2 (/v1/models completo, sondeo de
// upstreams) y Z4 (recalcular los ganchos del planificador en Reload y barrer
// réplicas de modelos retirados).
//
// Semilla: devuelve nil. Lo implementa Z1.
func (c *Config) Backends() []*Backend {
	return nil
}
