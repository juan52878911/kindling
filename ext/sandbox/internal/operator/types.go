// Package operator es un operador fino de Kubernetes para sandboxes de
// kindling-sandbox. Kubernetes solo hace de plano de CONTROL declarativo: un
// objeto Sandbox describe qué microVM se quiere, y este operador la crea, la
// mantiene y la borra hablando con el frontal de kindling-sandbox por HTTP.
//
// Las microVMs NO corren dentro del clúster. El plano de datos (exec, shell,
// ficheros) sigue yendo directo al frontal, con su propio token; kubectl nunca
// ve un byte de eso, y este operador tampoco lo intermedia.
//
// Sin dependencias externas: nada de client-go ni controller-runtime. El API
// de Kubernetes es HTTP y JSON, así que un cliente mínimo con net/http basta.
package operator

import (
	"encoding/json"
	"time"
)

// Group, Version y Resource del CRD. Van en las rutas del API de Kubernetes:
// /apis/{Group}/{Version}/namespaces/{ns}/{Resource}/{name}.
const (
	Group    = "sandbox.kindling.dev"
	Version  = "v1alpha1"
	Resource = "sandboxes"
)

// CleanupFinalizer es lo que impide que Kubernetes borre un Sandbox antes de
// que el operador haya podido borrar la microVM en el frontal. Sin esto, un
// `kubectl delete` dejaría sandboxes huérfanos consumiendo cupo y RAM en los
// hosts sin que nada en el clúster lo recuerde.
const CleanupFinalizer = "sandbox.kindling.dev/cleanup"

// ObjectMeta es el subconjunto de metadata.* que el operador necesita. Se
// declara a mano, sin tirar de k8s.io/apimachinery, porque son media docena de
// campos y arrastrar ese módulo para leerlos rompería la regla de cero
// dependencias.
type ObjectMeta struct {
	Name              string     `json:"name"`
	Namespace         string     `json:"namespace,omitempty"`
	UID               string     `json:"uid,omitempty"`
	ResourceVersion   string     `json:"resourceVersion,omitempty"`
	Generation        int64      `json:"generation,omitempty"`
	DeletionTimestamp *time.Time `json:"deletionTimestamp,omitempty"`
	Finalizers        []string   `json:"finalizers,omitempty"`
}

// HasFinalizer dice si CleanupFinalizer ya está puesto.
func (m *ObjectMeta) HasFinalizer() bool {
	for _, f := range m.Finalizers {
		if f == CleanupFinalizer {
			return true
		}
	}
	return false
}

// SandboxSpec es spec.* del CRD: lo que el usuario declara que quiere.
type SandboxSpec struct {
	// Template o Image, igual que en el frontal: uno de los dos, no ambos.
	Template string `json:"template,omitempty"`
	Image    string `json:"image,omitempty"`
	// TTLSeconds es el único campo que se puede cambiar en caliente: un
	// cambio aquí se traduce en un renew contra el frontal, no en recrear.
	TTLSeconds int    `json:"ttlSeconds,omitempty"`
	OnTTL      string `json:"onTTL,omitempty"`
	Egress     string `json:"egress,omitempty"`
	MemMiB     int    `json:"memMiB,omitempty"`
	// AllowDomains son los dominios permitidos con egress: allowlist.
	AllowDomains []string `json:"allowDomains,omitempty"`
	VCPUs        int      `json:"vcpus,omitempty"`
	// Labels van al sandbox en el frontal. La etiqueta k8s-uid es del operador
	// y no se puede pisar desde aquí.
	Labels map[string]string `json:"labels,omitempty"`
	// Env son variables de entorno de la sesión "default" del sandbox.
	Env     map[string]string `json:"env,omitempty"`
	Volumes []VolumeSpec      `json:"volumes,omitempty"`
	// Paused pausa el sandbox (pause) y, al volver a false, lo reanuda
	// (resume). Sí se aplica en caliente. No despierta uno congelado por su
	// propio TTL: congelar es cosa del frontal.
	Paused bool `json:"paused,omitempty"`
}

// VolumeSpec es un volumen del tenant a montar en el sandbox.
type VolumeSpec struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// SandboxStatus es status.* del CRD: lo que el operador observó del frontal.
// Nunca lo escribe el usuario; el subrecurso /status ya lo impide en el API
// server, pero el operador tampoco lo lee de spec por si acaso.
type SandboxStatus struct {
	// ID es el id compuesto del frontal ("host/máquina"), la clave con la que
	// se habla de este sandbox en las siguientes llamadas.
	ID string `json:"id,omitempty"`
	// Host es solo para lectura humana (kubectl get -o wide); ID ya lo lleva.
	Host string `json:"host,omitempty"`
	// State es el último estado visto: running, warm, o "gone" (un estado que
	// el frontal no tiene: lo pone este operador cuando el sandbox desaparece
	// del frontal sin que el operador lo haya borrado él mismo).
	State     string     `json:"state,omitempty"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	// Endpoint es la URL del sandbox en el frontal (el plano de datos: exec,
	// ficheros...). Se llega con el token del tenant; no lleva ningún secreto.
	Endpoint string `json:"endpoint,omitempty"`
	// Message es un puntero, no un string a secas: PatchStatus manda un merge
	// patch a partir de un SandboxStatus a medio rellenar, y con omitempty un
	// string en cero (recuperado, sin error) nunca saldría en el JSON — el
	// merge patch omitiría la clave por completo y el mensaje de error viejo
	// se quedaría para siempre en Kubernetes aunque el frontal ya respondiera
	// bien. nil sigue significando "no toques este campo"; un puntero a ""
	// significa "bórralo de verdad". Comprobado contra un k3s real: sin esto,
	// status.message no se limpiaba nunca tras una caída del frontal.
	Message *string `json:"message,omitempty"`
	// ObservedGeneration es la generation de spec que ya se aplicó. Distinta
	// de metadata.generation es la señal de "hay un cambio de spec pendiente"
	// (hoy, solo ttlSeconds puede cambiar en caliente).
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// StateGone es el estado que el operador escribe cuando el sandbox ya no
// existe en el frontal (lo borró la limpieza por abandono, o alguien a mano).
const StateGone = "gone"

// Sandbox es el objeto completo, tal y como lo sirve el API de Kubernetes.
type Sandbox struct {
	APIVersion string        `json:"apiVersion,omitempty"`
	Kind       string        `json:"kind,omitempty"`
	Metadata   ObjectMeta    `json:"metadata"`
	Spec       SandboxSpec   `json:"spec"`
	Status     SandboxStatus `json:"status,omitempty"`
}

// Key identifica el objeto dentro del operador: "namespace/name".
func (s *Sandbox) Key() string { return s.Metadata.Namespace + "/" + s.Metadata.Name }

// PoolResource es el plural del segundo CRD, SandboxPool.
const PoolResource = "sandboxpools"

// PoolSpec es spec.* de un SandboxPool: cuántas copias despiertas de una
// plantilla se quieren. Size 0 apaga el fondo y lo vacía.
type PoolSpec struct {
	Template string `json:"template"`
	Size     int    `json:"size"`
}

// PoolStatus es status.* de un SandboxPool.
type PoolStatus struct {
	// Available son las copias ya despiertas y reclamables, sumadas en todos
	// los hosts que tienen la plantilla. Puntero: 0 es un valor que hay que
	// poder escribir en un merge patch.
	Available *int `json:"available,omitempty"`
	// Message es el último error (puntero a "" = limpiarlo; ver SandboxStatus).
	Message            *string `json:"message,omitempty"`
	ObservedGeneration int64   `json:"observedGeneration,omitempty"`
}

// Pool es un objeto SandboxPool.
type Pool struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PoolSpec   `json:"spec"`
	Status     PoolStatus `json:"status,omitempty"`
}

// Key identifica el objeto: "namespace/name".
func (p *Pool) Key() string { return p.Metadata.Namespace + "/" + p.Metadata.Name }

// PoolList es la respuesta de un LIST de SandboxPool.
type PoolList struct {
	Metadata listMeta `json:"metadata"`
	Items    []Pool   `json:"items"`
}

// listMeta es metadata.* de una lista: solo hace falta el resourceVersion
// desde el que arrancar el watch.
type listMeta struct {
	ResourceVersion string `json:"resourceVersion"`
}

// SandboxList es la respuesta de un LIST.
type SandboxList struct {
	Metadata listMeta  `json:"metadata"`
	Items    []Sandbox `json:"items"`
}

// watchEvent es una línea del stream de un WATCH: type es ADDED, MODIFIED,
// DELETED, BOOKMARK o ERROR; object pospone su decode hasta saber el tipo,
// porque en ERROR es un Status de Kubernetes, no un Sandbox.
type watchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// status es el objeto de error que devuelve el API de Kubernetes (kind:
// Status) tanto en respuestas de error normales como en eventos ERROR del
// watch.
type status struct {
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Code    int    `json:"code"`
}
