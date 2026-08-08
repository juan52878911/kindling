// Package api define el contrato entre el CLI y el daemon.
package api

import (
	"encoding/json"
	"time"
)

// State es el ciclo de vida de una microVM.
//
// StateWarm es lo que distingue a kindling de un runtime de contenedores: la
// máquina está congelada en un snapshot y despierta en decenas de milisegundos.
// No consume CPU ni RAM mientras está así, solo disco.
type State string

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateWarm    State = "warm"
	StateStopped State = "stopped"
	StateFailed  State = "failed"
)

// Machine es una microVM gestionada por el daemon.
type Machine struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Image    string `json:"image"`
	State    State  `json:"state"`
	VCPUs    int    `json:"vcpus"`
	MemMiB   int    `json:"mem_mib"`
	PID      int    `json:"pid,omitempty"`
	LastErr  string `json:"last_error,omitempty"`
	SnapSize int64  `json:"snapshot_bytes,omitempty"`

	// DiskBytes es la ocupación REAL en disco de esta máquina: bloques asignados,
	// no tamaño lógico. Con overlays dispersos la diferencia es de dos órdenes de
	// magnitud, así que el tamaño lógico no dice nada útil.
	DiskBytes int64 `json:"disk_bytes,omitempty"`

	CreatedAt time.Time  `json:"created_at"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	FrozenAt  *time.Time `json:"frozen_at,omitempty"`

	// From es el snapshot dorado del que se restauró, si lo hubo.
	From string `json:"from,omitempty"`

	// IP por la que el host alcanza esta microVM. Dentro, todas las máquinas
	// usan la misma dirección: la diferenciación vive en el host.
	IP       string `json:"ip,omitempty"`
	NetIndex int    `json:"net_index,omitempty"`
	Egress   string `json:"egress,omitempty"`

	TTLSeconds int `json:"ttl_seconds,omitempty"`
	CPUPct     int `json:"cpu_pct,omitempty"`

	// Labels agrupa máquinas. La clave "service" es convencional: identifica de
	// qué servidor MCP es instancia esta microVM, y es por donde agrupan tanto
	// `topo` como el HTML exportado.
	Labels map[string]string `json:"labels,omitempty"`

	// Milisegundos de la última operación, para ver el coste real de cada fase.
	BootMS   int64 `json:"boot_ms,omitempty"`
	FreezeMS int64 `json:"freeze_ms,omitempty"`
	ThawMS   int64 `json:"thaw_ms,omitempty"`
}

// RunRequest crea y arranca una microVM.
//
// Si From apunta a un snapshot dorado, la máquina se restaura desde él en vez de
// arrancar en frío, y el resto de campos se heredan del snapshot.
type RunRequest struct {
	Name   string `json:"name,omitempty"`
	Image  string `json:"image,omitempty"`
	From   string `json:"from,omitempty"`
	VCPUs  int    `json:"vcpus,omitempty"`
	MemMiB int    `json:"mem_mib,omitempty"`

	// Egress: "none" (por defecto) o "internet". Nunca hay acceso a redes
	// privadas: el código de dentro se considera hostil.
	Egress string `json:"egress,omitempty"`

	// TTLSeconds congela la máquina automáticamente pasado ese tiempo. Es la
	// pieza que hace "serverless" el modelo: una herramienta ociosa deja de
	// costar CPU y RAM sin intervención de nadie.
	TTLSeconds int `json:"ttl_seconds,omitempty"`

	// CPUPct acota el uso de CPU (100 = un core completo).
	CPUPct int `json:"cpu_pct,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`
}

// Snapshot es una microVM congelada y reutilizable: el artefacto del que se
// instancian N máquinas.
//
// Es a nivel de imagen, no de máquina. Todas las instancias mapean el MISMO
// fichero de memoria, así que el kernel comparte sus páginas en page cache: la
// segunda instancia y las siguientes salen casi gratis en RAM.
type Snapshot struct {
	Name      string    `json:"name"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
	VCPUs     int       `json:"vcpus"`
	MemMiB    int       `json:"mem_mib"`
	MemBytes  int64     `json:"mem_bytes"`  // ocupación real del fichero de memoria
	DiskBytes int64     `json:"disk_bytes"` // total del snapshot en disco
	Instances int       `json:"instances"`  // máquinas vivas restauradas de aquí

	// Labels heredadas de la máquina de la que se hizo commit. Las instancias
	// las reciben salvo que se sobrescriban.
	Labels map[string]string `json:"labels,omitempty"`

	// Tools es el catálogo de capacidades, capturado UNA VEZ al importar el
	// servicio y guardado junto al snapshot.
	//
	// Sin esto, responder "¿qué herramientas hay?" obligaría a despertar la
	// microVM: una pregunta de inventario acabaría arrancando máquinas. Con el
	// catálogo en disco, listar capacidades no toca el servicio.
	Tools   []ToolSpec `json:"tools,omitempty"`
	ToolsAt *time.Time `json:"tools_at,omitempty"`
}

// ToolSpec describe una herramienta tal y como la declaró su servidor MCP.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Link es un servidor MCP EXTERNO registrado en el agregador.
//
// No corre en una microVM: vive donde su dueño lo tenga —en el Mac, en otro
// host, en un servicio remoto— y kindling solo lo enruta. Sirve para traer
// capacidades que no tiene sentido meter en una máquina efímera, en particular
// las de memoria: un servicio al que todas las herramientas puedan escribir y
// del que puedan leer, sin que kindling tenga que implementar almacenamiento.
type Link struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	Description string            `json:"description,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Tools       []ToolSpec        `json:"tools,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

// Service devuelve el nombre de servicio del enlace.
func (l *Link) Service() string {
	if l.Labels != nil {
		if s := l.Labels[LabelService]; s != "" {
			return s
		}
	}
	return l.Name
}

// CatalogRequest adjunta el catálogo de capacidades a un snapshot.
type CatalogRequest struct {
	Tools []ToolSpec `json:"tools"`
}

// CommitRequest congela una máquina en marcha como snapshot reutilizable.
type CommitRequest struct {
	Name string `json:"name"`
}

// Event es un cambio de estado publicado en el bus del daemon.
type Event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	ID      string    `json:"id,omitempty"`
	Name    string    `json:"name,omitempty"`
	Message string    `json:"message,omitempty"`
}

// Tipos de evento.
const (
	EvCreated   = "machine.created"
	EvStarted   = "machine.started"
	EvFrozen    = "machine.frozen"
	EvThawed    = "machine.thawed"
	EvStopped   = "machine.stopped"
	EvCommitted = "snapshot.committed"
	EvFailed    = "machine.failed"
)

// Info describe el daemon.
type Info struct {
	Version   string `json:"version"`
	Root      string `json:"root"`
	KVM       bool   `json:"kvm"`
	Machines  int    `json:"machines"`
	Firecrack string `json:"firecracker,omitempty"`
}

// Error es la respuesta de error de la API.
type Error struct {
	Message string `json:"message"`
}
