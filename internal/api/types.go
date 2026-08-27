// Package api define el contrato entre el CLI y el daemon.
package api

import (
	"encoding/json"
	"errors"
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

	// FailedAt es cuándo pasó a failed. Es lo que permite recogerla después de
	// un tiempo de gracia: una failed sin fecha no se puede envejecer, y las
	// fallidas se acumulaban para siempre, una por intento de instanciación roto.
	FailedAt *time.Time `json:"failed_at,omitempty"`

	// From es el snapshot dorado del que se restauró, si lo hubo.
	From string `json:"from,omitempty"`

	// IP por la que el host alcanza esta microVM. Dentro, todas las máquinas
	// usan la misma dirección: la diferenciación vive en el host.
	IP       string `json:"ip,omitempty"`
	NetIndex int    `json:"net_index,omitempty"`
	Egress   string `json:"egress,omitempty"`

	// AllowDomains son los dominios permitidos con egress "allowlist". Viajan con
	// la máquina para que descongelarla rehaga el mismo filtro (ver thaw).
	AllowDomains []string `json:"allow_domains,omitempty"`

	TTLSeconds int `json:"ttl_seconds,omitempty"`
	CPUPct     int `json:"cpu_pct,omitempty"`

	// Volumes son los volúmenes montados, en el orden en que van los discos.
	Volumes []VolumeAttachment `json:"volumes,omitempty"`

	// Labels agrupa máquinas. La clave "service" es convencional: identifica de
	// qué servidor MCP es instancia esta microVM, y es por donde agrupan tanto
	// `topo` como el HTML exportado.
	Labels map[string]string `json:"labels,omitempty"`

	// HasSecrets marca que se inyectó un secreto de sesión por MMDS en esta
	// microVM viva. Importa para la CONGELACIÓN: un secreto inyectado vive en la
	// RAM del invitado, y congelar vuelca esa RAM a mem.file —que, si la máquina
	// es o llega a ser un snapshot dorado, se COMPARTE con todas las copias—. Por
	// eso Freeze se niega a congelar una máquina marcada así (ver Freeze).
	HasSecrets bool `json:"has_secrets,omitempty"`

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

	// Egress: "none" (por defecto), "internet" o "allowlist". Nunca hay acceso a
	// redes privadas: el código de dentro se considera hostil.
	Egress string `json:"egress,omitempty"`

	// AllowDomains son los dominios permitidos cuando Egress es "allowlist".
	// Todo lo demás se descarta (IP directa, resolver ajeno, dominio no listado).
	// Se ignora en los otros modos.
	AllowDomains []string `json:"allow_domains,omitempty"`

	// TTLSeconds congela la máquina automáticamente pasado ese tiempo. Es la
	// pieza que hace "serverless" el modelo: una herramienta ociosa deja de
	// costar CPU y RAM sin intervención de nadie.
	TTLSeconds int `json:"ttl_seconds,omitempty"`

	// CPUPct acota el uso de CPU (100 = un core completo).
	CPUPct int `json:"cpu_pct,omitempty"`

	// Volumes son los volúmenes a montar, en orden. Es la forma completa.
	Volumes []VolumeAttachment `json:"volumes,omitempty"`

	// AllowExec enciende /exec dentro del invitado. Solo lo pone el daemon, y
	// solo para las microVMs de un solo uso que pueblan un volumen: nunca para
	// un servicio.
	AllowExec bool `json:"-"`

	// Volume monta un volumen persistente como tercer disco. VolumeMount es
	// dónde aparece dentro del invitado (por defecto /data).
	//
	// Es la respuesta a "quiero que lo que escriba mi herramienta sobreviva":
	// el overlay de cada máquina muere con ella, y un directorio del host
	// montado dentro rompería el aislamiento que justifica usar microVMs.
	Volume      string `json:"volume,omitempty"`
	VolumeMount string `json:"volume_mount,omitempty"`
	// VolumeReadOnly monta el volumen en SOLO LECTURA. Es lo que permite que
	// varias microVMs compartan uno: un ext4 no admite dos escritores, pero sí
	// muchos lectores. Sirve para una biblioteca de paquetes común.
	VolumeReadOnly bool `json:"volume_read_only,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`
}

// GuestPort es donde escucha el puente dentro de la microVM. Vive aquí, y no en
// el gateway, porque también lo necesita el manager para pedirle al invitado que
// vacíe su volumen antes de morir — y machine no puede importar gateway.
const GuestPort = 8080

// VolumeAttachment es un volumen montado en una microVM: cuál, dónde y cómo.
//
// Se admite más de uno porque los dos usos naturales se estorban: un servicio
// quiere su almacenamiento propio en escritura Y la biblioteca de paquetes
// compartida en lectura, y con un solo disco había que elegir.
//
// El ORDEN importa y es parte del contrato: el orden de esta lista es el orden
// en que se enganchan los discos (/dev/vdc, /dev/vdd, ...) y el orden en que
// viajan los puntos de montaje en la línea de comandos del kernel. Reordenarla
// entre el arranque y la restauración montaría cada volumen en el sitio del
// otro.
type VolumeAttachment struct {
	Name     string `json:"name"`
	Mount    string `json:"mount"`
	ReadOnly bool   `json:"read_only,omitempty"`
	// DriveID es cómo se llama este disco DENTRO del VMM.
	//
	// Queda fijado en el primer arranque en frío y viaja con cada snapshot de la
	// cadena: a una VM restaurada solo se le puede reapuntar un disco por el
	// nombre con el que se congeló. Deducirlo del meta.json rompía la cadena,
	// porque el meta SÍ cambia en cada commit y el nombre del disco no.
	//
	// Vacío en máquinas y snapshots anteriores a este campo; quien restaura
	// aplica entonces la heurística de siempre.
	DriveID string `json:"drive_id,omitempty"`
}

// MaxVolumes acota cuántos puede llevar una microVM.
//
// El límite existe porque cada volumen es un disco más, y los discos se nombran
// por letra: vda es la base, vdb el overlay, y de vdc en adelante los volúmenes.
// Cuatro cubre de sobra los usos reales y mantiene la correspondencia legible.
const MaxVolumes = 4

// VolumeBootParam es el parámetro de la línea de comandos del kernel por el que
// el invitado sabe dónde montar su volumen.
//
// Viaja por ahí y no dentro de la imagen para que el mismo snapshot dorado sirva
// con volúmenes distintos, o sin ninguno.
const VolumeBootParam = "kling.volume"

// LegacyVolumeDriveID es como se llamaba el disco de volumen cuando solo podía
// haber uno. Los snapshots congelados entonces lo llevan grabado dentro y no se
// pueden reescribir, así que restaurarlos exige seguir usando este nombre.
const LegacyVolumeDriveID = "volume"

// LayerBootParam dice al invitado en qué disco está la capa de servicio.
//
// Una imagen por capas son dos discos de solo lectura: la base compartida por
// todos los servicios (vda) y el delta propio de este (un disco extra). El
// device viaja aquí, y no por posición, porque la posición depende de cuántos
// volúmenes lleve la máquina — el mismo motivo por el que el punto de montaje
// del volumen viaja en VolumeBootParam.
//
// Ausente = imagen monolítica: el invitado hace el overlay de siempre sobre /.
const LayerBootParam = "kling.layer"

// ExecBootParam enciende la ejecución de comandos dentro del invitado.
//
// Solo lo pone el anfitrión, y solo en las microVMs de un solo uso que pueblan
// un volumen. Una microVM de servicio no lo lleva nunca: el invitado no puede
// concederse a sí mismo esa capacidad porque la línea de comandos del kernel la
// escribe quien arranca la máquina.
const ExecBootParam = "kling.exec"

// Image es una imagen de rootfs ya construida ($ROOT/images/$NAME.ext4): la base
// de la que se arrancan las microVMs. Hasta que existió GET /images no se podían
// ni enumerar.
type Image struct {
	Name string `json:"name"`
	// SizeBytes es el tamaño LÓGICO del .ext4; DiskBytes lo REALMENTE asignado en
	// disco (bloques × 512). Con ficheros dispersos difieren, así que el lógico
	// solo no dice cuánto se recupera al borrar. Mismo par que Volume.
	//
	// En una imagen por capas miden LA CAPA, no la suma con la base: la base no
	// es suya, se comparte con todas las demás, y sumársela a cada una haría creer
	// que el disco está N veces más lleno de lo que está. Lo que cuesta este
	// servicio, y lo que se recupera al borrarlo, es su capa.
	SizeBytes int64 `json:"size_bytes"`
	DiskBytes int64 `json:"disk_bytes"`
	HasRecipe bool  `json:"has_recipe"` // se guardó cómo se construyó
	UsedBy    int   `json:"used_by"`    // snapshots dorados que salieron de aquí

	// Base es la imagen sobre la que se apoya, si va por capas. Vacío =
	// monolítica, que es como se construía todo antes.
	Base string `json:"base,omitempty"`

	// Layers cuenta las imágenes por capas que usan ESTA como base.
	//
	// Es lo que dice si se puede retirar: una base con capas encima no se puede
	// borrar aunque no tenga snapshots propios — se llevaría por delante todos
	// esos servicios, que solo guardan su delta.
	Layers int `json:"layers,omitempty"`
}

// Volume es almacenamiento que sobrevive a la microVM que lo usa.
type Volume struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// SizeBytes es el tamaño lógico; UsedBytes lo realmente asignado en disco,
	// que con un fichero disperso no tiene nada que ver.
	SizeBytes int64 `json:"size_bytes"`
	UsedBytes int64 `json:"used_bytes"`
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

	// Volumes son los volúmenes que tenía la plantilla, en el orden de los
	// discos. Se recuerdan para que despertar una instancia no exija repetirlos:
	// el gateway despierta servicios por nombre y no sabe nada de volúmenes.
	//
	// Y sobre todo porque el CONJUNTO DE DISCOS queda fijado al congelar:
	// Firecracker no admite añadir ni quitar discos a una VM restaurada, así que
	// al restaurar hay que reenganchar exactamente estos, en este orden.
	Volumes []VolumeAttachment `json:"volumes,omitempty"`

	// Los tres campos siguientes son de los snapshots de una sola unidad, de
	// antes de que una microVM pudiera llevar varios. Se conservan para poder
	// leerlos: un snapshot ya congelado no se puede reescribir, y romperlos
	// obligaría a reimportar todos los servicios existentes. VolumeSet() los
	// normaliza a la lista y es lo único que debe consultar el resto del código.
	Volume         string `json:"volume,omitempty"`
	HasVolume      bool   `json:"has_volume,omitempty"`
	VolumeMount    string `json:"volume_mount,omitempty"`
	VolumeReadOnly bool   `json:"volume_read_only,omitempty"`

	// Egress es la política de red con la que se importó el servicio.
	//
	// Vive aquí porque las instancias se crean DESDE el snapshot, no desde la
	// máquina original: sin esto, un servicio importado con acceso a internet
	// despertaba sin él y toda llamada suya al exterior fallaba, para siempre y
	// sin explicación.
	Egress string `json:"egress,omitempty"`

	// CPUPct es el techo de CPU (% de un core) con el que se importó el servicio.
	// Viaja con el snapshot por la misma razón que Egress: las instancias nacen
	// DESDE él y el techo es un límite de cgroup en runtime, no algo que quede
	// dentro del volcado de memoria. Sin grabarlo, toda restauración caía al
	// defaultCPUPct=50 del daemon aunque el servicio se hubiera importado con más
	// —y en Mac ese estrangulamiento a media vCPU dobla el arranque en frío de
	// node (medido: 16 s a 50 % → 6.9 s a 100 %)—. 0 = usar el defecto del daemon
	// (compatibilidad con snapshots anteriores a este campo).
	CPUPct int `json:"cpu_pct,omitempty"`

	// AllowDomains es la lista de dominios permitidos cuando Egress es
	// "allowlist". Se graba junto al snapshot por la misma razón que Egress: las
	// instancias nacen DESDE el snapshot, y sin esto despertarían con la lista
	// vacía —es decir, sin poder salir a ninguno de sus dominios— para siempre.
	AllowDomains []string `json:"allow_domains,omitempty"`

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

	// INTEGRIDAD. sha256 del overlay dorado (rootfs) y del volcado de estado
	// (snap.file), calculados al congelar y verificados al restaurar. Detectan que
	// un snapshot se corrompió en disco —bit rot, una copia a medias, un tercero
	// que lo tocó— antes de despertar una microVM en un estado que ya no es el que
	// se congeló, y que sin esto no daría una sola señal.
	//
	// El mem.file NO se hashea a propósito: es el fichero grande y restaurar
	// promete ~30 ms; leerlo entero por sha256 en cada thaw mataría esa cifra.
	// Ver Manager.verifyIntegrity.
	RootfsSHA256 string `json:"rootfs_sha256,omitempty"`
	SnapSHA256   string `json:"snap_sha256,omitempty"`

	// SALUD. Resultado del último sondeo (`kling mcp health`): se instancia una
	// microVM efímera del snapshot y se le pide tools/list; si contesta, "healthy".
	// Vacío mientras no se haya sondeado nunca.
	Health    string     `json:"health,omitempty"` // "healthy" | "unhealthy" | ""
	HealthAt  *time.Time `json:"health_at,omitempty"`
	HealthErr string     `json:"health_err,omitempty"` // por qué falló, si "unhealthy"
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

// HealthRequest anota en un snapshot el resultado de un sondeo de salud. El
// sondeo lo hace quien puede arrancar la microVM efímera (el CLI, vía el daemon);
// aquí solo se persiste el veredicto en el meta.
type HealthRequest struct {
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"` // por qué falló, si no está sano
}

// CommitRequest congela una máquina en marcha como snapshot reutilizable.
//
// Replace pide reemplazar un snapshot que ya exista con ese nombre. Es opt-in a
// propósito: pisar un snapshot destruye el anterior, y eso no debe pasar por un
// nombre repetido sin querer. El caso que lo hace necesario es real: los
// snapshots quedan atados al TSC del host y un reinicio los invalida TODOS, así
// que rehacerlos es operación rutinaria, no excepción.
type CommitRequest struct {
	Name    string `json:"name"`
	Replace bool   `json:"replace,omitempty"`
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

// ProcStat es la foto de recursos de UNA microVM.
//
// PSS y no RSS a propósito: las instancias de un mismo snapshot dorado mapean el
// MISMO fichero de memoria en copy-on-write, así que el RSS de cada proceso
// cuenta el mem.file entero y las N copias suman N veces lo que en realidad
// ocupa una. El PSS reparte cada página compartida entre quienes la mapean, y es
// la única cifra que suma lo que de verdad cuesta el host (~7 MiB por copia).
type ProcStat struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Service string `json:"service,omitempty"`
	From    string `json:"from,omitempty"` // snapshot dorado de origen, si lo hubo
	State   State  `json:"state"`
	PID     int    `json:"pid,omitempty"`
	PSSMiB  int64  `json:"pss_mib"`
}

// ProcStats es la respuesta de GET /procstats: el consumo del conjunto.
type ProcStats struct {
	Machines    []ProcStat     `json:"machines"`
	ByState     map[string]int `json:"by_state"`      // running/warm/… -> cuántas
	TotalPSSMiB int64          `json:"total_pss_mib"` // suma del PSS de las vivas
	// Memoria del host, leída de /proc/meminfo. 0 en dev sobre macOS (sin /proc).
	AvailableMiB int64 `json:"available_mib"` // MemAvailable
	FreeMiB      int64 `json:"free_mib"`      // MemFree
}

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
	// PIP son paquetes de Python que preinstalar, por la misma razón que NPM:
	// dentro no hay internet en tiempo de ejecución.
	PIP []string `json:"pip,omitempty"`
	// Env son variables de entorno ("KEY=value") que se HORNEAN en el
	// entrypoint de la imagen, en texto plano — no valen para secretos.
	//
	// El caso que las motivó es semgrep: hace phone-home de métricas al
	// arrancar y, con el egress cerrado, espera ~2 minutos al timeout en cada
	// arranque en frío. SEMGREP_SEND_METRICS=off lo corta de raíz, y sin este
	// campo la única forma de fijarlo era empaquetar a mano con el script.
	Env []string `json:"env,omitempty"`
	// Cmd es el comando que arranca el servidor MCP dentro del invitado.
	Cmd []string `json:"cmd"`
	// GrowMB agranda la imagen. 0 deja que el script decida.
	GrowMB int `json:"grow_mb,omitempty"`
	// Bundle empaqueta el servidor node en un solo fichero con esbuild al
	// construir. Acelera el arranque en frío dentro de la microVM (sobre todo en
	// arm64/Mac, donde cargar cientos de ficheros de node_modules se amplifica
	// bajo KVM anidado): carga 1 fichero en vez de todo el árbol. Solo node (NPM).
	Bundle bool `json:"bundle,omitempty"`
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

// VolumeSet devuelve los volúmenes del snapshot, vengan de la lista o de los
// campos sueltos de un snapshot antiguo.
//
// Existe para que el resto del código no tenga que saber de qué época es cada
// snapshot. Los que se congelaron con un solo volumen siguen arrancando.
func (s *Snapshot) VolumeSet() []VolumeAttachment {
	if len(s.Volumes) > 0 {
		return s.Volumes
	}
	if s.Volume == "" && !s.HasVolume {
		return nil
	}
	// Los snapshots de una sola unidad llamaban al disco "volume" a secas.
	return []VolumeAttachment{{Name: s.Volume, Mount: s.VolumeMount,
		ReadOnly: s.VolumeReadOnly, DriveID: LegacyVolumeDriveID}}
}

// VolumeSet devuelve los volúmenes pedidos, normalizando la forma corta.
//
// La forma corta (-volume X -mount Y) es la de un solo volumen y se conserva por
// compatibilidad: peticiones de un CLI de v0.1.0 contra un daemon nuevo. Si se
// dan las dos, manda la lista.
func (r RunRequest) VolumeSet() []VolumeAttachment {
	if len(r.Volumes) > 0 {
		return r.Volumes
	}
	if r.Volume == "" {
		return nil
	}
	return []VolumeAttachment{{Name: r.Volume, Mount: r.VolumeMount, ReadOnly: r.VolumeReadOnly}}
}

// PopulateRequest es una instalación de paquetes DENTRO de una microVM
// desechable, con el volumen montado en escritura.
//
// Existe para no instalar en el anfitrión. Instalar paquetes es ejecutar código
// de terceros, y hacerlo fuera de la frontera que kindling levanta contradice la
// razón de ser del proyecto: con esto, un paquete con sorpresas se lleva por
// delante una máquina que se destruye a continuación.
type PopulateRequest struct {
	Volume string   `json:"volume"`
	Mount  string   `json:"mount,omitempty"`
	Image  string   `json:"image,omitempty"`
	Cmd    []string `json:"cmd"`
	MemMiB int      `json:"mem_mib,omitempty"`
}

// PopulateResult es lo que salió de ahí.
type PopulateResult struct {
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
	Machine  string `json:"machine"`
	UsedMiB  int64  `json:"used_mib"`
}

// BridgeRefresh es lo que pasó con una imagen al poner el puente actual dentro.
//
// Se informa TAMBIÉN de las que no hacía falta tocar y de las que se saltaron:
// saber que una imagen sigue con el puente viejo porque la está usando alguien
// es justo lo que hay que saber, y omitirlo la haría parecer al día.
type BridgeRefresh struct {
	Image   string `json:"image"`
	Updated bool   `json:"updated"`
	Skipped bool   `json:"skipped,omitempty"`
	Error   string `json:"error,omitempty"`

	// Busy separa el salto que se cura parando una microVM del que no.
	//
	// Se saltan imágenes por dos motivos que no se arreglan igual: una en uso hay
	// que pararla y repetir; una que no lleva puente propio —una base mínima, o
	// una capa cuyo puente vive en su base— no hay nada que repetir. Sin este
	// campo, quien lo lee acaba adivinándolo del texto del error, y el consejo
	// "párala y vuelve a intentarlo" sale también cuando no hay nada que parar.
	Busy bool `json:"busy,omitempty"`
}

// ImageRecipe es CÓMO se construyó una imagen.
//
// Se guarda junto a ella porque, si no, una imagen no es reproducible: lo único
// que sobrevive del comando es el /entrypoint de DENTRO, y para leerlo hay que
// montar la imagen. Los paquetes no sobreviven en ninguna parte — hay que
// deducirlos, y deducir mal significa reconstruir algo que no es lo que había.
//
// El caso que lo motivó: actualizar el puente de cuatro servicios exigió sacar
// el comando de cada imagen con `debugfs`, y aun así los paquetes hubo que
// adivinarlos.
type ImageRecipe struct {
	Name     string    `json:"name"`
	Base     string    `json:"base,omitempty"`
	Packages []string  `json:"packages,omitempty"`
	NPM      []string  `json:"npm,omitempty"`
	PIP      []string  `json:"pip,omitempty"`
	Env      []string  `json:"env,omitempty"`
	Cmd      []string  `json:"cmd"`
	GrowMB   int       `json:"grow_mb,omitempty"`
	Bundle   bool      `json:"bundle,omitempty"`
	BuiltAt  time.Time `json:"built_at"`
	KlingVer string    `json:"kling_version,omitempty"`
}

// Capabilities son las capacidades que una imagen declara sobre lo que su
// servidor MCP necesita, detectadas de su árbol de dependencias al construirla.
// El import las usa para configurar el egress solo y avisar de módulos nativos.
type Capabilities struct {
	Browser bool     `json:"browser"`          // usa un navegador (Chromium)
	Egress  string   `json:"egress,omitempty"` // "none" | "internet" | "allowlist"
	Native  []string `json:"native,omitempty"` // módulos nativos npm detectados

	// NativeMissing son los módulos nativos que quedaron SIN su binario: el
	// empaquetado con --ignore-scripts no compila, y ni traían prebuild en el
	// tarball ni un paquete de plataforma que lo aportara. El servidor arranca e
	// introspecciona igual, pero la PRIMERA herramienta que los use peta en
	// caliente. Por eso el import lo trata como ERROR y no como aviso: es mejor
	// fallar al construir el catálogo que entregar un servicio que revienta luego.
	NativeMissing []string `json:"native_missing,omitempty"`

	// System son binarios del SISTEMA (no-npm) que el servidor invoca —git,
	// ffmpeg, ripgrep, pandoc, python…— y que el build horneó en la imagen con
	// apk. Sin ellos, la herramienta que los llame fallaría con "command not
	// found", un síntoma que no se parece a su causa. Es informativo: ya están
	// dentro.
	System []string `json:"system,omitempty"`

	// AllowDomains es la SEMILLA de dominios que el build extrajo de los literales
	// de URL del árbol npm. Es una pista editable, no una lista exhaustiva ni
	// autoritativa: el import la usa solo si se elige el modo "allowlist".
	AllowDomains []string `json:"allow_domains,omitempty"`
}

// StatusError es un error de la API que conserva el código HTTP.
//
// Existe porque quien llama a veces necesita distinguir QUÉ clase de negativa
// recibió, no solo leer un texto. El caso que lo motivó: el gateway tiene que
// saber que un arranque falló por falta de memoria —y no por otra cosa— para
// poder hacer sitio congelando otra instancia y reintentar. Comparar cadenas
// para eso es frágil: el día que alguien reescriba el mensaje, el gateway deja
// de hacer sitio y nadie relaciona una cosa con la otra.
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// SqueezeResult informa de un apretón de globo: cuánta RAM se devolvió al host
// sin congelar la microVM. ReclaimedMiB y RSSMiB son 0 en un host sin /proc
// (macOS de desarrollo); GuestFreeMiB sale de las estadísticas del propio globo.
type SqueezeResult struct {
	ID           string `json:"id"`
	ReclaimedMiB int    `json:"reclaimed_mib"`  // caída medida del RSS del proceso firecracker
	GuestFreeMiB int    `json:"guest_free_mib"` // memoria libre que reportaba el invitado
	RSSMiB       int    `json:"rss_mib"`        // RSS del proceso tras apretar
}

// StatusInsufficientMemory es la negativa por falta de memoria en el anfitrión.
// 507 es "Insufficient Storage", que es lo más cerca que hay en HTTP.
const StatusInsufficientMemory = 507

// IsInsufficientMemory dice si un error es una negativa por falta de memoria.
func IsInsufficientMemory(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == StatusInsufficientMemory
}
