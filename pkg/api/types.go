// Package api define el contrato entre el CLI y el daemon.
package api

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// State es el ciclo de vida de una microVM.
//
// StateWarm es lo que distingue a kindling de un runtime de contenedores: la
// máquina está congelada en un snapshot y despierta en decenas de milisegundos.
// No consume CPU ni RAM mientras está así, solo disco. Desde 0.14 se llama
// "frozen" —es lo que produce `kling freeze`—; hasta 0.13 era "warm", y ese
// nombre se sigue leyendo (UnmarshalJSON) de daemons y ficheros anteriores.
type State string

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateWarm    State = "frozen"
	// StateWarmLegacy es cómo llamaban a StateWarm los daemons hasta 0.13.
	StateWarmLegacy = "warm"
	// StatePaused: el VMM sigue vivo con el invitado en pausa (sin volcar
	// nada a disco). Despertarla es solo reanudar: ~1 ms, pero retiene su RAM.
	// Es el nivel "pausada" del planificador (docs/despertar.md).
	StatePaused  State = "paused"
	StateStopped State = "stopped"
	StateFailed  State = "failed"
)

// UnmarshalJSON acepta el nombre antiguo del estado congelado: un CLI nuevo
// contra un daemon 0.13, o un daemon nuevo leyendo el estado que guardó el
// anterior, ven "warm" y lo entienden como "frozen".
func (s *State) UnmarshalJSON(b []byte) error {
	var v string
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v == StateWarmLegacy {
		v = string(StateWarm)
	}
	*s = State(v)
	return nil
}

// Machine es una microVM gestionada por el daemon.
type Machine struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	State   State  `json:"state"`
	VCPUs   int    `json:"vcpus"`
	MemMiB  int    `json:"mem_mib"`
	PID     int    `json:"pid,omitempty"`
	LastErr string `json:"last_error,omitempty"`
	// MemMaxMiB es el techo al que se puede subir MemMiB en caliente (resize).
	// 0 = sin elasticidad: MemMiB es fija, como siempre.
	MemMaxMiB int   `json:"mem_max_mib,omitempty"`
	SnapSize  int64 `json:"snapshot_bytes,omitempty"`

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

	// Forwards traduce un puerto del invitado a la dirección del host por la que
	// se alcanza ("8080" -> "127.0.0.1:61234"). Solo lo rellena el backend de
	// macOS: allí todos los invitados comparten IP y no hay ruta hacia ella, así
	// que cada uno se alcanza por puertos que su ayudante abre en loopback. En
	// Linux va vacío e IP basta. No leerlo a mano: usar Addr.
	Forwards map[string]string `json:"forwards,omitempty"`

	// AllowDomains son los dominios permitidos con egress "allowlist". Viajan con
	// la máquina para que descongelarla rehaga el mismo filtro (ver thaw).
	AllowDomains []string `json:"allow_domains,omitempty"`

	TTLSeconds int `json:"ttl_seconds,omitempty"`
	CPUPct     int `json:"cpu_pct,omitempty"`

	// AllowExec: la máquina acepta exec y ficheros (ver RunRequest.AllowExec).
	AllowExec bool `json:"allow_exec,omitempty"`
	// OnTTL: "remove" si se destruye al vencer el TTL; vacío = se congela.
	OnTTL string `json:"on_ttl,omitempty"`

	// TTLAt es cuándo empezó a contar TTLSeconds.
	//
	// Es un reloj aparte de StartedAt a propósito: StartedAt se reescribe en cada
	// thaw, así que una máquina que se congela y despierta reiniciaba su TTL en
	// cada ciclo y podía no vencer nunca. Lo renueva `renew`, y solo eso.
	TTLAt *time.Time `json:"ttl_at,omitempty"`

	// Volumes son los volúmenes montados, en el orden en que van los discos.
	Volumes []VolumeAttachment `json:"volumes,omitempty"`

	// Shares son las carpetas del host compartidas con la máquina. La posición
	// de una carpeta viva es su tag (ver docs/compartir.md).
	Shares []ShareAttachment `json:"shares,omitempty"`

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

	// Wake desglosa el último despertar (thaw o reanudación) por fases. ThawMS
	// es solo la carga del snapshot; Wake.TotalMS es lo que de verdad esperó
	// quien pidió el thaw (ver docs/despertar.md).
	Wake *WakePhases `json:"wake,omitempty"`
}

// WakePhases es el desglose de un despertar en el daemon, en milisegundos.
// Cada campo es el tiempo de pared de esa fase; TotalMS los suma todos más lo
// que quede entre ellos.
type WakePhases struct {
	// Tier es de dónde se despertó: "frozen" (snapshot en disco: VMM nuevo y
	// LoadSnapshot) o "paused" (VMM vivo en pausa: solo Resume).
	Tier string `json:"tier"`
	// WaitMS es la espera del candado de la máquina y de la puerta de arranque.
	WaitMS float64 `json:"wait_ms"`
	// CheckMS es comprobar que no quede ya un VMM vivo de esta máquina.
	CheckMS float64 `json:"check_ms"`
	// NetMS es montar (o comprobar) el namespace, el veth, el tap y las reglas.
	NetMS float64 `json:"net_ms"`
	// SpawnMS es lanzar el VMM (firecracker o jailer) y preparar su jaula.
	SpawnMS float64 `json:"spawn_ms"`
	// SocketMS es esperar a que su socket de API conteste.
	SocketMS float64 `json:"socket_ms"`
	// LoadMS es LoadSnapshot con reanudación (lo que siempre midió ThawMS), o
	// el Resume de una pausada.
	LoadMS float64 `json:"load_ms"`
	// ForwardsMS es abrir los reenvíos de puertos (solo macOS).
	ForwardsMS float64 `json:"forwards_ms,omitempty"`
	// ResyncMS es la resincronización de reloj y entropía del invitado.
	ResyncMS float64 `json:"resync_ms"`
	// CgroupMS es volver a meter el VMM en su cgroup con techo de CPU.
	CgroupMS float64 `json:"cgroup_ms"`
	// FinishMS es apuntar el estado y el resto hasta contestar.
	FinishMS float64 `json:"finish_ms"`
	TotalMS  float64 `json:"total_ms"`
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
	// MemMaxMiB arranca la máquina con este techo y el globo reteniendo la
	// diferencia con MemMiB, para poder subirla o bajarla sin reiniciar
	// (POST /machines/{ref}/resize). 0 = memoria fija.
	MemMaxMiB int `json:"mem_max_mib,omitempty"`

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

	// Shares son carpetas del host a meter en la máquina: una copia de solo
	// lectura (modo copy, subida antes con POST /shares/uploads) o el directorio
	// vivo (ro, rw). Solo al arrancar en frío: con From se rechaza.
	Shares []ShareSpec `json:"shares,omitempty"`

	// AllowExec enciende la ejecución de comandos y el acceso a ficheros dentro
	// del invitado (POST /machines/{ref}/exec, /files). Es opt-in explícito y se
	// decide al arrancar: viaja en la línea de comandos del kernel, que se congela
	// con la memoria. Con From, el snapshot tiene que haberse hecho de una
	// máquina con AllowExec (si no, 409); y al revés, las máquinas de un snapshot
	// con AllowExec la tienen siempre, se pida o no.
	AllowExec bool `json:"allow_exec,omitempty"`

	// OnTTL es qué pasa cuando vence TTLSeconds: "freeze" (por defecto) o
	// "remove".
	OnTTL string `json:"on_ttl,omitempty"`

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
	MemMaxMiB int       `json:"mem_max_mib,omitempty"` // techo de resize; ver Machine.MemMaxMiB
	MemBytes  int64     `json:"mem_bytes"`             // ocupación real del fichero de memoria
	DiskBytes int64     `json:"disk_bytes"`            // total del snapshot en disco
	Instances int       `json:"instances"`             // máquinas vivas restauradas de aquí

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

	// AllowExec: la plantilla tenía la ejecución encendida, y por tanto la
	// tienen todas sus instancias (la puerta se congeló con la memoria).
	AllowExec bool `json:"allow_exec,omitempty"`

	// Labels heredadas de la máquina de la que se hizo commit. Las instancias
	// las reciben salvo que se sobrescriban.
	Labels map[string]string `json:"labels,omitempty"`

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
	// Signature es el HMAC-SHA256, con la clave del host, de los hashes y la
	// política del snapshot. Detecta manipulación y snapshots traídos de otro
	// host, que los sha256 solos no detectan.
	Signature string `json:"signature,omitempty"`

	// Annotations son datos opacos que una extensión cuelga del snapshot: el
	// daemon los guarda en meta.json y los devuelve, sin interpretarlos
	// (kindling-mcp guarda su catálogo en "mcp.tools" y su salud en
	// "mcp.health").
	Annotations map[string]json.RawMessage `json:"annotations,omitempty"`
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
	EvAnnotated = "snapshot.annotated"
	EvStored    = "store.updated"
	EvFailed    = "machine.failed"
	EvResized   = "machine.resized"
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
	// Capabilities lista las capacidades del API que sirve el daemon (p. ej.
	// "annotations", "store"). Un daemon anterior no la envía: vacía significa
	// "solo el API de siempre".
	Capabilities []string `json:"capabilities,omitempty"`
	// EncryptedAtRest dice si Root está sobre un disco cifrado (dm-crypt). nil =
	// no se sabe (daemon anterior, u otro sistema). Ver docs/cifrado.md.
	EncryptedAtRest *bool `json:"encrypted_at_rest,omitempty"`
	// Backend es el VMM con el que el daemon arranca las microVMs: "firecracker"
	// en Linux, "vz" en macOS (kling-vz). Vacío = daemon anterior, que siempre
	// era firecracker.
	Backend string `json:"backend,omitempty"`
	// Arch es la arquitectura del host del daemon (GOARCH: "amd64", "arm64").
	// Las imágenes y el kernel son de una arquitectura: `kling images copy`
	// la compara antes de mover gigas que luego no arrancarían.
	Arch string `json:"arch,omitempty"`
	// ShareRoots son los directorios del host bajo los que se pueden compartir
	// carpetas en vivo (daemon.share_roots). Vacío = ninguno.
	ShareRoots []string `json:"share_roots,omitempty"`
}

// Has dice si el daemon anuncia la capacidad c.
func (i *Info) Has(c string) bool {
	for _, x := range i.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// BuildImageRequest pide al daemon que construya una imagen con un constructor.
//
// La construcción vive en el daemon porque monta un loopback y hace chroot: son
// operaciones de root en el host con KVM, y el CLI corre en otra máquina. El
// daemon no sabe construir nada por sí mismo: ejecuta el constructor de nombre
// Builder, instalado por el administrador en /usr/local/lib/kindling/builders/,
// con esta petición (ver docs/api.md). El del núcleo es "base"; kindling-mcp
// trae "mcp".
type BuildImageRequest struct {
	// Name es el nombre de la imagen: componente de ruta y nombre de servicio.
	Name string `json:"name"`
	// Base es la imagen base sobre la que se construye la capa.
	Base string `json:"base,omitempty"`
	// GrowMB reserva sitio para la capa.
	GrowMB int `json:"grow_mb,omitempty"`
	// Builder es el constructor; Spec, lo que él entiende. El daemon no lo mira.
	Builder string          `json:"builder"`
	Spec    json.RawMessage `json:"spec,omitempty"`
}

// BuildImageResult describe la imagen construida.
type BuildImageResult struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Output string `json:"output,omitempty"`
}

// ImageFileStat es la respuesta de GET /images/{name}/files?path=...&stat=1.
type ImageFileStat struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	Size   int64  `json:"size,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

// PutImageFileRequest pone un fichero dentro de una imagen ya construida.
// Lleva el contenido (ContentB64) o el nombre de un fichero del directorio de
// librerías del daemon (FromHost, relativo a /usr/local/lib/kindling), que es
// donde una extensión deja lo que quiere meter en sus imágenes.
type PutImageFileRequest struct {
	Path       string `json:"path"`
	Mode       string `json:"mode,omitempty"` // octal, "0644" por defecto
	ContentB64 string `json:"content_b64,omitempty"`
	FromHost   string `json:"from_host,omitempty"`
	// Create permite crear el fichero si no existe. Sin él solo se reemplaza,
	// que es lo seguro para poner al día algo que la imagen ya traía.
	Create bool `json:"create,omitempty"`
}

// ImageFileResult cuenta qué pasó al poner un fichero en una imagen.
type ImageFileResult struct {
	Image   string `json:"image"`
	Path    string `json:"path"`
	Updated bool   `json:"updated"`           // se cambió (false = ya era idéntico)
	Skipped bool   `json:"skipped,omitempty"` // no estaba y no se pidió crearlo
	Busy    bool   `json:"busy,omitempty"`    // la imagen está en uso
	Error   string `json:"error,omitempty"`
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
	Port int `json:"port,omitempty"` // GuestPort (8080) si no se dice otra cosa

	// Path es la ruta dentro del invitado. VACÍO es el comportamiento de v0.4,
	// que se conserva por compatibilidad hasta v0.6: /mcp, con la cabecera
	// Accept de MCP y devolviendo Mcp-Session-Id. Un cliente nuevo pasa siempre
	// la ruta y las cabeceras que quiere; el daemon no añade nada de MCP.
	Path    string            `json:"path,omitempty"`
	Method  string            `json:"method,omitempty"` // POST si no se dice otra cosa
	Body    string            `json:"body,omitempty"`
	Headers map[string]string `json:"headers,omitempty"` // Content-Type es application/json si no se dice

	// ResponseHeaders son las cabeceras de la respuesta del invitado que se
	// devuelven en GuestResponse.Headers. Vacío = solo Content-Type.
	ResponseHeaders []string `json:"response_headers,omitempty"`

	// MaxBodyBytes es el tamaño máximo de la respuesta del invitado. Por
	// encima, la llamada falla (no se trunca). 0 = GuestMaxBody; tope GuestMaxBodyCap.
	MaxBodyBytes int64 `json:"max_body_bytes,omitempty"`

	// WaitMS espera a que el puerto abra antes de mandar nada. Un servidor recién
	// arrancado tarda en escuchar, y sin esto la primera llamada falla siempre.
	WaitMS int `json:"wait_ms,omitempty"`

	// ProbeOnly se conforma con que el puerto abra: no manda ninguna petición.
	// Sirve para separar "no arrancó" de "arrancó y contestó mal", que se
	// diagnostican de forma muy distinta.
	ProbeOnly bool `json:"probe_only,omitempty"`
}

const (
	// GuestMaxBody es el tamaño máximo por defecto de una respuesta del
	// invitado a través del proxy: un invitado que se desmadre no puede agotar
	// la memoria del daemon.
	GuestMaxBody int64 = 8 << 20
	// GuestMaxBodyCap es lo máximo que se puede pedir con MaxBodyBytes.
	GuestMaxBodyCap int64 = 64 << 20
)

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

	// Builder y Spec son los de la petición cuando la construyó un constructor
	// externo.
	Builder string          `json:"builder,omitempty"`
	Spec    json.RawMessage `json:"spec,omitempty"`
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

// ResizeRequest cambia la memoria de una máquina en caliente, dentro de su
// techo (MemMaxMiB).
type ResizeRequest struct {
	MemMiB int `json:"mem_mib"`
}

// SqueezeResult informa de un apretón de globo: cuánta RAM se devolvió al host
// sin congelar la microVM. ReclaimedMiB y RSSMiB son 0 en un host sin /proc
// (macOS de desarrollo); GuestFreeMiB sale de las estadísticas del propio globo.
type SqueezeResult struct {
	ID           string `json:"id"`
	ReclaimedMiB int    `json:"reclaimed_mib"`  // caída medida del RSS del proceso firecracker
	GuestFreeMiB int    `json:"guest_free_mib"` // memoria libre que reportaba el invitado
	RSSMiB       int    `json:"rss_mib"`        // RSS del proceso tras apretar
}

// StatusMachineLimit es la negativa por haber llegado al tope de máquinas del
// daemon. 409 y no 507: no es falta de memoria, y confundirlos hace que quien
// escala intente hacer sitio congelando —que no sube el contador— en vez de
// retirar máquinas o subir el tope.
const StatusMachineLimit = 409

// IsMachineLimit dice si un error es la negativa por el tope de máquinas.
func IsMachineLimit(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == StatusMachineLimit && strings.Contains(se.Message, machineLimitMark)
}

// machineLimitMark marca los errores de tope de máquinas para poder
// reconocerlos: 409 lo usan más cosas.
const machineLimitMark = "machine limit"

// StatusDiskFull es la negativa por quedar poco disco en el anfitrión. 503 y no
// 507: quien recibe un 507 congela instancias para hacer sitio, y congelar
// escribe en disco justo lo que falta.
const StatusDiskFull = 503

// IsDiskFull dice si un error es la negativa por disco casi lleno.
func IsDiskFull(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == StatusDiskFull && strings.Contains(se.Message, diskFullMark)
}

// diskFullMark marca los errores de disco lleno: 503 lo usan más cosas.
const diskFullMark = "of disk left under"

// StatusInsufficientMemory es la negativa por falta de memoria en el anfitrión.
// 507 es "Insufficient Storage", que es lo más cerca que hay en HTTP.
const StatusInsufficientMemory = 507

// IsInsufficientMemory dice si un error es una negativa por falta de memoria.
func IsInsufficientMemory(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == StatusInsufficientMemory
}
