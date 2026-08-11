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

	// Volume es el volumen persistente montado, si lo hay, y dónde.
	Volume      string `json:"volume,omitempty"`
	VolumeMount string `json:"volume_mount,omitempty"`

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

	// Volume monta un volumen persistente como tercer disco. VolumeMount es
	// dónde aparece dentro del invitado (por defecto /data).
	//
	// Es la respuesta a "quiero que lo que escriba mi herramienta sobreviva":
	// el overlay de cada máquina muere con ella, y un directorio del host
	// montado dentro rompería el aislamiento que justifica usar microVMs.
	Volume      string `json:"volume,omitempty"`
	VolumeMount string `json:"volume_mount,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`
}

// GuestPort es donde escucha el puente dentro de la microVM. Vive aquí, y no en
// el gateway, porque también lo necesita el manager para pedirle al invitado que
// vacíe su volumen antes de morir — y machine no puede importar gateway.
const GuestPort = 8080

// VolumeBootParam es el parámetro de la línea de comandos del kernel por el que
// el invitado sabe dónde montar su volumen.
//
// Viaja por ahí y no dentro de la imagen para que el mismo snapshot dorado sirva
// con volúmenes distintos, o sin ninguno.
const VolumeBootParam = "kling.volume"

// Volume es almacenamiento que sobrevive a la microVM que lo usa.
type Volume struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// SizeBytes es el tamaño lógico; UsedBytes lo realmente asignado en disco,
	// que con un fichero disperso no tiene nada que ver.
	SizeBytes    int64     `json:"size_bytes"`
	UsedBytes    int64     `json:"used_bytes"`
	CreatedAt    time.Time `json:"created_at"`
	LastModified time.Time `json:"last_modified"`
	// UsedBy son las máquinas que lo tienen montado ahora mismo.
	UsedBy []string `json:"used_by,omitempty"`
}

// CreateVolumeRequest crea un volumen.
type CreateVolumeRequest struct {
	Name    string `json:"name"`
	SizeMiB int    `json:"size_mib,omitempty"`
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

	// Volume es el volumen que tenía la plantilla. Se recuerda para que
	// despertar una instancia no exija repetirlo: el gateway despierta
	// servicios por nombre y no sabe nada de volúmenes.
	Volume string `json:"volume,omitempty"`

	// HasVolume dice si el snapshot lleva el DISPOSITIVO de volumen dentro.
	//
	// Firecracker no permite añadir discos a una VM restaurada, así que esto no
	// se puede cambiar después: un servicio importado sin volumen no podrá
	// tener uno sin reimportarlo. VolumeMount recuerda dónde lo monta.
	HasVolume   bool   `json:"has_volume,omitempty"`
	VolumeMount string `json:"volume_mount,omitempty"`

	// Egress es la política de red con la que se importó el servicio.
	//
	// Vive aquí porque las instancias se crean DESDE el snapshot, no desde la
	// máquina original: sin esto, un servicio importado con acceso a internet
	// despertaba sin él y toda llamada suya al exterior fallaba, para siempre y
	// sin explicación.
	Egress string `json:"egress,omitempty"`

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

// BuildImageRequest pide al daemon que empaquete un servidor MCP de stdio.
//
// La construcción vive en el daemon porque monta un loopback y hace chroot: son
// operaciones de root en el host con KVM, y el CLI corre en otra máquina.
type BuildImageRequest struct {
	// Name es el de la imagen y, después, el del servicio.
	Name string `json:"name"`
	// Base es la imagen de partida (por defecto: min).
	Base string `json:"base,omitempty"`
	// Packages son paquetes de apk que instalar en el invitado.
	Packages []string `json:"packages,omitempty"`
	// NPM son paquetes de node que PREINSTALAR. Es obligatorio y no una
	// comodidad: las microVMs arrancan sin salida a internet, así que un
	// `npx -y` en tiempo de ejecución fallaría al intentar descargar.
	NPM []string `json:"npm,omitempty"`
	// Cmd es el comando que arranca el servidor MCP dentro del invitado.
	Cmd []string `json:"cmd"`
	// GrowMB agranda la imagen. 0 deja que el script decida.
	GrowMB int `json:"grow_mb,omitempty"`
}

// BuildImageResult describe la imagen construida.
type BuildImageResult struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Output string `json:"output,omitempty"`
}

// Error es la respuesta de error de la API.
type Error struct {
	Message string `json:"message"`
}

// GuestRequest pide al daemon que hable con el servidor que corre DENTRO de una
// microVM. El cliente no puede hacerlo por su cuenta: las IP de los invitados
// solo existen en la red del host, así que con transporte SSH un sondeo directo
// se queda esperando hasta agotar el plazo.
type GuestRequest struct {
	Port    int               `json:"port,omitempty"`   // 8080 si no se dice otra cosa
	Path    string            `json:"path,omitempty"`   // /mcp si no se dice otra cosa
	Method  string            `json:"method,omitempty"` // POST si no se dice otra cosa
	Body    string            `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// WaitMS espera a que el puerto abra antes de mandar nada. Un servidor recién
	// arrancado tarda en escuchar, y sin esto la primera llamada falla siempre.
	WaitMS int `json:"wait_ms,omitempty"`

	// ProbeOnly se conforma con que el puerto abra: no manda ninguna petición.
	// Sirve para separar "no arrancó" de "arrancó y contestó mal", que se
	// diagnostican de forma muy distinta.
	ProbeOnly bool `json:"probe_only,omitempty"`
}

// GuestResponse es lo que contestó el invitado, tal cual.
type GuestResponse struct {
	Status  int               `json:"status"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers,omitempty"`
}
