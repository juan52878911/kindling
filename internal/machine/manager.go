package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/durable"
	"github.com/juan52878911/kindling/pkg/esquema"
	"github.com/juan52878911/kindling/pkg/panico"
)

// La raíz se monta en SOLO LECTURA y el init es overlay-init, que superpone el
// disco propio de cada máquina (/dev/vdb) sobre la imagen base compartida
// (/dev/vda). Así N microVMs comparten una base de cientos de MB en vez de
// copiarla N veces.
const bootArgsBase = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/sbin/overlay-init quiet"

// archBootArg añade parámetros de kernel específicos de la arquitectura para
// acortar el arranque. En amd64 apaga la emulación de i8042 (teclado/ratón
// PS/2): Firecracker no la necesita y el sondeo del controlador cuesta cientos
// de ms. arm64 no tiene ese controlador que apagar. El backend vz recibe esta
// misma línea (ver spec.TranslateBootArgs) y no toca estos parámetros.
func archBootArg() string {
	if runtime.GOARCH == "amd64" {
		return " i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
	}
	return ""
}

// bootArgs arma la línea de comandos del kernel del invitado. La red la aplica
// el propio kernel con el parámetro ip=, sin herramientas dentro de la imagen.
//
// El punto de montaje del volumen viaja aquí y no dentro de la imagen: así el
// mismo snapshot dorado sirve con volúmenes distintos, o sin ninguno. Lo mismo
// vale para layerDev, el disco de la capa de servicio: vacío en las imágenes
// monolíticas, que siguen arrancando con la línea de siempre.
//
// ipv6Stack: la receta de la imagen pide el módulo IPv6 cargado (knet.BootArg).
func bootArgs(vols []api.VolumeAttachment, allowExec bool, layerDev string, ipv6Stack bool) string {
	return bootArgsBase + archBootArg() + " " + knet.BootArg(ipv6Stack) + volumeBootArg(vols) + execBootArg(allowExec) + layerBootArg(layerDev)
}

// defaultOverlayMiB es el tamaño lógico del disco escribible por máquina. Al ser
// disperso, el coste real en disco es solo lo que la microVM llegue a escribir.
const defaultOverlayMiB = 512

// minOverlayMiB y maxOverlayMiB acotan RunRequest.DiskMiB: por debajo del
// mínimo ext4 no deja sitio ni para el agente; el máximo es solo cordura (el
// fichero es disperso) frente a un cero de más.
const (
	minOverlayMiB = 64
	maxOverlayMiB = 256 << 10
)

// Límites de seguridad. El código que corre dentro se considera hostil, así que
// una sola microVM no debe poder degradar el host ni a las demás.
const (
	// defaultMaxMachines evita que un cliente comprometido agote el host creando
	// máquinas sin fin. Es un tope de seguridad, no una medida de capacidad: con
	// sandboxes efímeros y un host grande se queda corto, así que se puede subir
	// con KLING_MAX_MACHINES (ver maxMachines).
	defaultMaxMachines = 256

	// Caudal máximo por dispositivo. Sin esto una microVM satura el disco o la
	// red del host y tumba a todas las demás.
	diskBytesPerSec = 128 << 20 // 128 MiB/s
	netBytesPerSec  = 16 << 20  // 16 MiB/s

	// defaultCPUPct: media vCPU por microVM. Una herramienta MCP responde a
	// peticiones puntuales; darle un core entero solo sirve para que un bucle
	// infinito degrade a las vecinas.
	defaultCPUPct = 50
)

// Manager gestiona todas las microVMs del daemon.
type Manager struct {
	root        string // /var/lib/kindling
	fcBin       string
	priv        *Privileges
	PrivWarning string

	// cgroupRoot vacío = sin límite de CPU; el motivo queda en CgroupWarning.
	cgroupRoot    string
	CgroupWarning string

	// jailerJailed dice si las microVMs de este proceso arrancan dentro de
	// jailer (ver decidirJailer). Se decide UNA vez al construir el Manager y
	// no cambia en caliente, para que Freeze/Commit puedan preguntar "¿esta
	// máquina ya viva corre jailed?" sin volver a mirar el entorno.
	jailerJailed bool

	// JailerBlocked, si no está vacío, es el motivo por el que Run, runFrom y
	// Thaw se niegan a arrancar máquinas NUEVAS: automático (KLING_JAILER sin
	// fijar) y falta el binario de jailer o el usuario sin privilegios. Se
	// imprime una vez al arrancar el daemon (ver internal/daemon) y de ahí en
	// adelante se devuelve como el error de cada intento de arranque, hasta
	// reiniciar el daemon con lo que falta instalado o con KLING_JAILER=0.
	JailerBlocked string

	// JailerWarning es el aviso de SEGURIDAD que hay que imprimir una vez al
	// arrancar cuando alguien apagó jailer a propósito con KLING_JAILER=0:
	// las microVMs corren sin la barrera de chroot/pivot_root.
	JailerWarning string

	// consolaLeida es hasta dónde leyó revisarErroresDisco la consola de cada
	// máquina (errores_disco.go), con consolaMu; inicioDaemon, cuándo arrancó
	// este Manager.
	consolaMu    sync.Mutex
	consolaLeida map[string]posConsola
	inicioDaemon time.Time

	// cow es el modo de copia de discos en uso (daemon.cow) y alm el almacén
	// XFS propio, si la plataforma lo tiene (nil en macOS). Ver cow.go.
	cow estadoCoW
	alm *almacenCoW

	bus  *events.Bus
	mu   sync.RWMutex
	byID map[string]*api.Machine
	// nombresNaciendo son los nombres de las máquinas que run está creando y
	// aún no están en byID (reservarNombre). Bajo mu.
	nombresNaciendo map[string]bool

	// metaMu serializa las escrituras de meta.json de snapshots existentes
	// (anotaciones). Leer-modificar-escribir sin él dejaba que dos anotaciones
	// simultáneas —el gateway marcando salud y el CLI guardando el catálogo— se
	// pisaran y una se perdiera sin error.
	metaMu sync.Mutex
	socket map[string]string // id -> ruta del socket de firecracker
	// entornoPendiente es el entorno de las máquinas cuyo agente aún no lo
	// ha leído de MMDS (entorno.go): un PutMMDS en ese rato lo conserva.
	entornoPendiente map[string]map[string]string

	// reserved son los ids cuyo directorio se está CONSTRUYENDO ahora mismo, aún
	// sin entrada en byID, y los snapshots ("snap:<nombre>") que un commit está
	// escribiendo o una restauración está leyendo. Ver reserveDir: sin esto el
	// barrido de huérfanos los borra bajo los pies de quien los está llenando.
	// El valor es cuántos lo tienen reservado a la vez. Se toca bajo mu.
	reserved map[string]int

	// Dorados cuya integridad ya se comprobó, por huella de sus ficheros. Ver
	// verifyIntegrity: hashear el overlay cuesta el 67% de una instanciación y
	// un dorado no cambia desde que se congela.
	integridad map[string]huellaSnapshot

	// kernelSHA cachea el sha256 de KernelPath() por tamaño+fecha del fichero
	// (K2): se pide en cada Commit y en cada runFrom, y el vmlinux no cambia
	// entre un arranque del daemon y el siguiente. Ver kernelHash.
	kernelSHA huellaKernel

	// origen es con qué trabaja este daemon (versión de kling, VMM, macOS):
	// se graba en cada dorado y se compara con el de los que ya hay. Lo fija
	// FijarOrigen; nil es "no consta". Ver meta.go.
	origen atomic.Pointer[origenHost]

	// snapCache memoriza, por nombre de snapshot, el meta.json ya parseado y la
	// ocupación en disco del directorio (M-08): Snapshots() se llama en cada
	// tick del reaper del gateway y en cada arranque desde un dorado, y sin
	// esto cada llamada releía y parseaba TODOS los meta.json y recorría TODOS
	// los directorios de snapshot. Se invalida a mano en cuanto se escribe el
	// meta.json (writeMeta) o se borra el directorio (removeSnapshot). Ver
	// loadSnapshotCached e invalidateSnapCache.
	snapCache map[string]snapCacheEntry

	// memAllocCache memoriza los bytes REALMENTE asignados del mem.file de cada
	// snapshot dorado (M-12): es inmutable desde que se congela, así que
	// stat-earlo bajo m.mu en cada Run/runFrom/Resize (hotMemFilesMiBLocked) no
	// aporta nada sobre calcularlo una vez. Se invalida junto con snapCache.
	memAllocCache map[string]int64

	// gcPausadoHasta: hasta cuando NO se expulsa por disco. Se pone cuando una
	// pasada completa no libera nada, lo que significa que el disco lo llena algo
	// ajeno a kindling y seguir expulsando solo cuesta warm-pooling.
	gcPausadoHasta time.Time

	// volReservas: volumenes comprometidos entre la comprobacion y la
	// publicacion en byID, por nombre de volumen. Cierra la ventana en la que
	// dos arranques simultaneos veian los dos "cero escritores". Se lee y se
	// escribe SIEMPRE bajo m.mu, igual que byID.
	volReservas map[string][]reservaVolumen

	// netCursor rota los índices de red en vez de reutilizar el menor libre.
	// Ver asignarRed. Lo protege netMu, no mu: asignar mira el host.
	netMu     sync.Mutex
	netCursor int

	// layerOK memoriza qué bases traen un overlay-init que entiende las capas.
	// Es dato de un fichero que no cambia, y preguntarlo cuesta un debugfs en el
	// camino de arranque en frío. Ver baseSupportsLayers.
	layerOK sync.Map

	// bridgeOK memoriza qué pares (base, capa) llevan agente de invitado
	// (M-14), igual que layerOK: la respuesta no cambia mientras ninguno de los
	// dos ficheros cambie, y preguntarlo cuesta hasta 4 debugfs en el camino de
	// arranque en frío de cada microVM con volúmenes o carpetas compartidas.
	// Ver imageHasBridgeCached.
	bridgeOK sync.Map
	// listoDeclarado memoriza, igual, qué imágenes declaran sonda o ganchos
	// de "listo" (ver imagenDeclaraListo).
	listoDeclarado sync.Map

	// resyncAvisado recuerda por imagen que ya se avisó de que su agente no
	// resincroniza (ver resyncGuest): un aviso por imagen, no uno por thaw.
	resyncAvisado sync.Map
	// resyncSinAgente: qué restauraciones no tienen agente al que resincronizar
	// (claveThaw, claveSnapshot; ver resyncSinAgenteTTL).
	resyncSinAgente sync.Map

	// avisosAnyDB: credenciales de almacén antiguo promovidas a AnyDatabase de
	// las que ya se avisó (dueño + variable), para avisar una vez y no en cada
	// carga (ver avisarAnyDatabase).
	avisosAnyDB sync.Map

	// ipv6Avisado recuerda, por nombre de dorado, si ya se avisó (log + evento)
	// de que sus instancias conservan el módulo IPv6 del kernel del invitado
	// (F2: dorados congelados antes de la barrera IPv6, sin GuestIPv6Off). Un
	// aviso por dorado, no uno por runFrom: igual que resyncAvisado.
	ipv6Avisado sync.Map

	// redMontada son las máquinas cuya red (namespace, veth, tap y reglas)
	// montó ESTE proceso del daemon y sigue en pie. Freeze ya no la desmonta:
	// Thaw la reutiliza si está aquí y el namespace sigue existiendo, y se
	// ahorra la docena de ip/iptables de montarla (ver red.go).
	redMontada sync.Map

	// pendingMiB es la memoria de las microVMs que están ARRANCANDO ahora mismo,
	// aún sin proceso que la ocupe. checkHostMemory la resta de lo disponible:
	// sin esto, dos arranques concurrentes ven los dos la misma memoria libre,
	// pasan los dos, y juntos no caben — el OOM del anfitrión que el check existe
	// para impedir. Se toca bajo mu.
	pendingMiB int

	// volcandoMiB es el disco que tienen reservado los volcados en curso
	// (freeze, save, el thaw de un diferencial sin reflink) y que aún no han
	// escrito: reservarDiscoParaVolcado lo suma a lo que pide cada uno, como
	// pendingMiB con la memoria. Se toca bajo mu.
	volcandoMiB int

	// snapPending cuenta, por snapshot dorado, cuántas instancias están
	// arrancando de él ahora mismo. Sirve a reserveMemory para saber si el
	// mem.file ya está anclado y cobrar solo la fracción divergente a las copias.
	// Se toca bajo mu.
	snapPending map[string]int

	// freezeFails cuenta los fallos CONSECUTIVOS de congelación por TTL de cada
	// máquina. Existe para que el segador pueda rendirse: sin memoria de cuántas
	// veces ha fallado, reintentaba para siempre sobre máquinas irrecuperables.
	// Se toca bajo mu; se limpia al congelar con éxito o al darla por perdida.
	freezeFails map[string]int

	// squeezedAt es cuándo se apretó por última vez el globo de cada máquina
	// para hacer sitio. Ver makeRoom.
	squeezedAt map[string]time.Time

	// orphanSeen cuenta las vueltas seguidas del vigilante en las que un VMM
	// pareció huérfano. Ver sweepOrphanVMMs: matar a la primera es una carrera
	// con la máquina que está naciendo.
	orphanSeen map[string]int

	// templateMu serializa la construcción de la plantilla de overlay: dos
	// arranques a la vez sobre un host limpio la formatearían por duplicado.
	templateMu sync.Mutex

	// launchGate acota cuántas microVMs ENCIENDEN a la vez. Ver launch.go: una
	// tormenta de arranques simultáneos —todos creando vCPU y mapeando memoria en
	// KVM en el mismo instante— cuelga el kernel bajo virtualización anidada.
	// Capacidad = KLING_MAX_PARALLEL_BOOT. nil en un Manager de test = sin límite.
	launchGate chan struct{}

	// lifecycle serializa las operaciones sobre UNA MISMA máquina. Sin esto, dos
	// thaw concurrentes rehacen su namespace a la vez y el segundo encuentra el
	// veth a medio crear: "Cannot find device vh-...".
	//
	// Con contador de referencias: la entrada se retira sola cuando sale el
	// ultimo que la usaba. Ver cerrojos.go para por que un sync.Map no bastaba.
	lifecycle cerrojos

	// pruebaTrasPublicar, si no es nil, se llama en Run justo después de
	// publicar la máquina en byID, con su cerrojo de ciclo de vida tomado. Un
	// error la abandona como cualquier otro fallo del arranque. Solo lo ponen
	// las pruebas: es la única forma de parar un arranque en frío a mitad sin
	// KVM, red ni firecracker, y comprobar que Stop/Remove esperan a que acabe.
	pruebaTrasPublicar func(id string) error

	// pruebasCPU sustituye la escritura de cpu.max y la espera al agente del
	// techo de arranque (arranque_cpu.go). Solo lo ponen las pruebas.
	pruebasCPU *ganchosCPU

	// impulsos es el impulso de CPU de arranque vigente de cada máquina
	// (arranque_cpu.go), con su candado: bajo él se registra uno nuevo y se
	// escribe el techo al deshacerlo. No toma m.mu dentro.
	impulsosMu sync.Mutex
	impulsos   map[string]*impulsoCPU

	// vigiasListo numera las vigías de "listo" de cada máquina (listo.go):
	// una nueva jubila a la anterior.
	vigiasListo sync.Map
	// pruebasListo y pruebasGanchos sustituyen GET /ready y POST /hooks del
	// agente. Solo lo ponen las pruebas.
	pruebasListo   func(ctx context.Context, id string) (api.GuestReady, error)
	pruebasGanchos func(ctx context.Context, id, kind string) (api.GuestReady, error)
	pruebasMeminfo func(ctx context.Context, id string) (api.GuestMemInfo, error)

	// secretos: por máquina, las inyecciones por MMDS y si unos ganchos las
	// consumieron (ver "LEVANTAR LA MARCA DE SECRETOS"). Bajo mu.
	secretos map[string]*estadoSecreto

	// transicion es la operación de ciclo de vida en curso de cada máquina
	// que va a sacarla de su estado (api.Transition*). Con m.mu. Solo se
	// copia a lo que devuelven List y Get: las entradas de byID no la llevan,
	// así que tampoco llega al estado persistido. Ver marcarTransicion.
	transicion map[string]string

	// muertes serializa killMachine por máquina (ver killMachine). Aparte del
	// cerrojo de ciclo de vida, que quien mata ya suele tener tomado.
	muertes cerrojos

	// credPlantillaMu serializa la lectura-fusión-escritura del almacén de
	// credenciales de las plantillas (SetSnapshotCredentials).
	credPlantillaMu sync.Mutex

	// Escritura del estado, fuera del lock. Ver persist().
	stateMu sync.Mutex
	pending []api.Machine // último snapshot sin escribir; el nuevo pisa al viejo
	hasPend bool
	// pendGen numera las fotos; escritoGen es la última que llegó a disco.
	// Hacen falta desde que hay dos escritores (persistLoop y persistirYa):
	// sin ellos, una foto vieja escrita tarde pisaría a una nueva.
	pendGen    uint64
	escritoGen uint64
	// estadoIlegible no está vacío si state.json no se pudo leer (ver load):
	// el daemon no borra ni mata nada por no conocerlo. estadoIntocable, si
	// además no se pudo apartar: tampoco se escribe encima. Se fijan en load,
	// antes de que arranque nada concurrente, y después solo se leen.
	estadoIlegible  string
	estadoIntocable bool
	// escrituraMu serializa las escrituras de state.json: durable.Escribir usa
	// un temporal de nombre fijo y dos a la vez se lo pisarían.
	escrituraMu sync.Mutex

	// shares lleva las conexiones de las carpetas compartidas en vivo, y
	// shareCfg de dónde sale su configuración (ver shares.go).
	shares     *shareSup
	sharesOnce sync.Once
	shareCfg   func() ShareConfig

	// uploadMu serializa el check-and-reserve del tope de subidas pendientes
	// (M-19): sin él, N subidas a la vez pasan todas la comprobación antes de
	// que ninguna termine, y se cuelan hasta N ext4 de sobra en el disco.
	uploadMu       sync.Mutex
	uploadReserved int

	// Grafos de microVMs (grafo.go): por ID, bajo mu, porque el resolvedor de
	// cada arista los lee con mu tomado en cada conexión. despMu y desp son
	// los despertares en vuelo, uno por nodo (grafo_red.go); virtuales, el ID
	// de nodo de cada máquina de un grafo, para cortar las sesiones que van a
	// su nodo al congelarla, pararla o borrarla.
	grafos    map[string]*api.Graph
	despMu    sync.Mutex
	desp      map[string]*despertar
	virtuales sync.Map

	// El broker de enlaces de macOS (broker.go, broker_vz.go): dónde escucha
	// y el listener. Vacíos en Linux y en los managers de prueba.
	brokerRuta string
	brokerLn   *net.UnixListener
	// marcarBrokerPrueba sustituye el dial del broker. Solo las pruebas.
	marcarBrokerPrueba func(ctx context.Context, addr string) (net.Conn, error)

	// Clave de firma de snapshots (firma.go), cargada una vez.
	firmaOnce  sync.Once
	firmaClave []byte
	firmaErr   error
	wake       chan struct{}
	quit       chan struct{}
	quitOnce   sync.Once
	persistWG  sync.WaitGroup
}

// lock serializa las operaciones de ciclo de vida de una máquina concreta.
func (m *Manager) lock(id string) func() { return m.lifecycle.tomar(id) }

// lockUnaVez es lock con una liberación idempotente: la función devuelta se
// puede llamar varias veces y solo suelta la primera. La usan Run y runFrom,
// que la difieren para todos sus returns y además la llaman a mano antes de
// cualquier operación pública sobre la misma máquina (el cerrojo no es
// reentrante; ver doc.go).
func (m *Manager) lockUnaVez(id string) func() {
	soltar := m.lock(id)
	var una sync.Once
	return func() { una.Do(soltar) }
}

// tryLock es lock sin esperar: (nil, false) si otro tiene la máquina.
func (m *Manager) tryLock(id string) (func(), bool) { return m.lifecycle.intentar(id) }

func NewManager(root, fcBin, runAs string, bus *events.Bus) (*Manager, error) {
	for _, d := range []string{root, filepath.Join(root, "machines"), filepath.Join(root, "images")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := comprobarVersionEstado(root); err != nil {
		return nil, err
	}
	priv, warn := privilegiosPlataforma(runAs)
	jailed, jailerBlocked, jailerWarn := decidirJailer(jailerPosible, os.Getenv("KLING_JAILER"), jailerBinPresent(), priv.Enabled, priv.Motivo, os.Geteuid() == 0)
	m := &Manager{
		root: root, fcBin: fcBin, bus: bus, priv: priv, PrivWarning: warn,
		jailerJailed: jailed, JailerBlocked: jailerBlocked, JailerWarning: jailerWarn,
		byID:        make(map[string]*api.Machine),
		socket:      make(map[string]string),
		wake:        make(chan struct{}, 1),
		quit:        make(chan struct{}),
		launchGate:  make(chan struct{}, maxParallelLaunch()),
		snapPending: make(map[string]int),
		freezeFails: make(map[string]int),
	}
	m.persistWG.Add(1)
	go m.persistLoop()
	if cg, err := delegacionCgroups(); err != nil {
		m.CgroupWarning = err.Error()
	} else {
		m.cgroupRoot = cg
	}

	priv.EnsureReadable(filepath.Join(root, "images"))
	priv.EnsureReadable(filepath.Join(root, "snapshots"))
	if entradas, err := os.ReadDir(filepath.Join(root, "snapshots")); err == nil {
		for _, e := range entradas {
			if e.IsDir() {
				m.overlayDoradoSinJailer(filepath.Join(root, "snapshots", e.Name()))
			}
		}
	}
	restringirRaiz(root, priv)
	m.inicioDaemon = time.Now()
	// El almacén de discos, si existe, se monta ANTES de readoptar: las
	// instancias con su overlay dentro lo necesitan para descongelarse. Y los
	// binds que un daemon anterior dejó en jails de máquinas ya borradas.
	m.alm = nuevoAlmacen(root, priv)
	if m.alm != nil {
		m.alm.viva = func(id string) bool {
			m.mu.RLock()
			defer m.mu.RUnlock()
			_, ok := m.byID[id]
			return ok
		}
		m.alm.montarSiExiste(context.Background())
	}
	m.barrerBindsJail()
	// Las copias de volumen a medias de un daemon anterior (ver
	// volume_snapshot.go). Aquí y no en el vigilante: en marcha, un .tmp puede
	// ser una copia en curso.
	m.barrerTmpVolumenes()
	// Un commit -replace que un daemon anterior dejó a medias: el dorado
	// viejo sigue apartado al lado del nuevo (ver apartarAnterior).
	m.recuperarReemplazos()
	cerrarVolcadosExistentes(root)
	m.load()
	for _, mc := range m.byID {
		if mc.NetIndex > m.netCursor {
			m.netCursor = mc.NetIndex
		}
	}
	// El registro de auditoría del proxy, fuera del alcance del VMM (Linux),
	// antes de que reconcile vuelva a levantar los proxies (ver credaudit.go).
	m.prepararAuditoria()
	// Los grafos antes de reconciliar: reconcile rehace los proxies de enlace
	// de los nodos vivos, y para eso necesita sus aristas.
	m.cargarGrafos()
	// El broker antes de readoptar: los kling-vz que siguen vivos piden por
	// él sus aristas en cuanto el invitado conecta.
	m.iniciarBroker()
	m.reconcile()
	m.barrerAlmacen()
	// Tras readoptar: una máquina que arrancaba cuando murió el daemon anterior
	// se quedó con el techo de arranque (ver arranque_cpu.go).
	m.reaplicarTopesCPU()
	// Relleno inicial: DiskBytes no se persiste —es dato derivado— así que sin
	// esto todas las máquinas cargadas del estado saldrían a 0 en `kling ps`
	// hasta el primer tic del vigilante.
	m.refreshDiskUsage()
	return m, nil
}

func (m *Manager) dir(id string) string { return filepath.Join(m.root, "machines", id) }
func (m *Manager) statePath() string    { return filepath.Join(m.root, "state.json") }

// KernelPath es el vmlinux compartido por todas las microVMs.
func (m *Manager) KernelPath() string { return filepath.Join(m.root, "images", "vmlinux") }

func (m *Manager) imagePath(image string) string {
	return filepath.Join(m.root, "images", image+".ext4")
}

// ── persistencia ──────────────────────────────────────────────────────────────
// Los snapshots viven en disco, así que las máquinas warm deben sobrevivir a un
// reinicio del daemon: si no, perderíamos la vista de lo que sigue congelado.

// load carga state.json.
//
// UN ESTADO ILEGIBLE NO ES UN ESTADO VACÍO. Antes, si state.json no se podía
// leer o no era JSON válido, load volvía sin decir nada con byID vacío, y lo
// que venía después lo tomaba al pie de la letra: reconcile mataba todos los
// VMM "huérfanos", el barrido mandaba a la papelera el directorio de cada
// máquina (overlays, mem.file de las warm) y persist escribía un state.json
// vacío encima del roto. Un fichero truncado por un disco lleno se llevaba por
// delante todas las máquinas del host.
//
// Ahora el fichero ilegible se aparta a state.json.corrupt-<ns> (nunca se
// borra ni se pisa) y el daemon arranca en modo protegido: no borra ni mata
// nada que no conozca (ver barridoBloqueado). El modo dura mientras quede un
// state.json.corrupt-* en la raíz: es el operador quien decide qué recuperar
// y, al retirarlo, vuelve la recogida de basura.
func (m *Manager) load() {
	defer m.detectarCuarentena()
	b, err := os.ReadFile(m.statePath())
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var list []*api.Machine
	var version int
	if err == nil {
		version, err = esquema.Comprobar(m.statePath(), b, versionEstadoMax)
	}
	if esquema.EsMasNuevo(err) {
		// Lo dejó un kling más nuevo: no está roto, así que no se aparta, y
		// no se escribe encima (perdería lo que este binario no conoce).
		// NewManager ya se niega a arrancar con él; esto es la red de debajo.
		m.estadoIntocable = true
		m.estadoIlegible = err.Error()
		log.Printf("state: %v. Protected mode, and the state will NOT be written", err)
		return
	}
	if err == nil {
		list, err = decodificarEstado(b, version)
	}
	if err != nil {
		m.ponerEnCuarentena(err)
		return
	}
	if version < versionEstado {
		// La primera escritura lo pasará al formato nuevo: antes, la copia
		// del original, que es lo que permite volver al binario anterior.
		if err := esquema.Respaldar(m.statePath(), version); err != nil {
			m.estadoIntocable = true
			m.estadoIlegible = fmt.Sprintf("%s (schema %d) could not be backed up before migrating it (%v)",
				m.statePath(), version, err)
			log.Printf("state: %s. Protected mode, and the state will NOT be written", m.estadoIlegible)
		}
	}
	// No se toca el estado aquí: reconcile() decide comparando con la realidad
	// del host, porque una microVM SÍ puede sobrevivir al daemon.
	for _, mc := range list {
		if mc == nil || mc.ID == "" {
			continue
		}
		m.byID[mc.ID] = mc
	}
}

// versionEstado es la versión del formato de state.json que escribe este
// binario (ver pkg/esquema). Súbela cuando un kling anterior fuera a leer mal
// lo que se escribe, y añade el caso a decodificarEstado.
//
//	0: un array JSON de máquinas, sin campo (hasta v0.17).
//	1: {"schema": 1, "machines": [...]}.
//	2: el mismo formato, con alguna copia congelada en diferencial
//	   (diff_volcado.go). Un kling anterior cargaría su mem.file disperso como
//	   si fuera la RAM entera y la copia despertaría con la memoria rota: con
//	   el 2 se niega a arrancar. Sin copias en diferencial se sigue escribiendo
//	   el 1 y volver atrás es posible.
const versionEstado = 1

// versionEstadoDiff es el esquema que se escribe si alguna máquina tiene
// DiffBase; versionEstadoMax, el más nuevo que este binario sabe leer.
const (
	versionEstadoDiff = 2
	versionEstadoMax  = versionEstadoDiff
)

// esquemaParaEscribir es versionEstado, o versionEstadoDiff si alguna
// máquina depende de un diferencial.
func esquemaParaEscribir(list []api.Machine) int {
	for i := range list {
		if list[i].DiffBase != "" {
			return versionEstadoDiff
		}
	}
	return versionEstado
}

// ficheroEstado es state.json desde la versión 1.
type ficheroEstado struct {
	esquema.Cabecera
	Machines []*api.Machine `json:"machines"`
}

// decodificarEstado lee state.json en cualquier versión conocida.
func decodificarEstado(b []byte, version int) ([]*api.Machine, error) {
	switch version {
	case 0:
		var list []*api.Machine
		err := json.Unmarshal(b, &list)
		return list, err
	case 1, 2:
		var f ficheroEstado
		err := json.Unmarshal(b, &f)
		return f.Machines, err
	}
	return nil, fmt.Errorf("state.json: unknown schema %d", version)
}

// comprobarVersionEstado se niega a arrancar con un state.json de un kling más
// nuevo. Es lo primero que hace NewManager, antes de montar, barrer o
// readoptar nada: un daemon viejo sobre un estado nuevo no debe operar, ni
// siquiera en modo protegido, porque lo que haría con él no lo sabe nadie.
func comprobarVersionEstado(root string) error {
	ruta := filepath.Join(root, "state.json")
	b, err := os.ReadFile(ruta)
	if err != nil {
		return nil // que no exista es la primera arrancada; ilegible lo trata load
	}
	if _, err := esquema.Comprobar(ruta, b, versionEstadoMax); esquema.EsMasNuevo(err) {
		return err
	}
	return nil
}

// sufijoCuarentena es lo que lleva el nombre de un state.json apartado.
const sufijoCuarentena = ".corrupt-"

// ponerEnCuarentena aparta el state.json que no se pudo leer. Si ni siquiera
// se puede renombrar, se bloquea además su escritura: persist lo pisaría con
// un estado vacío, y ese fichero es lo único que queda de las máquinas.
func (m *Manager) ponerEnCuarentena(causa error) {
	destino := m.statePath() + sufijoCuarentena + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := os.Rename(m.statePath(), destino); err != nil {
		m.estadoIntocable = true
		m.estadoIlegible = fmt.Sprintf("%s is unreadable (%v) and could not be set aside (%v)",
			m.statePath(), causa, err)
		log.Printf("state: %s. Protected mode: nothing will be deleted or killed, and the state "+
			"will NOT be written until the file is fixed or moved away by hand", m.estadoIlegible)
		return
	}
	log.Printf("state: %s is unreadable (%v); moved to %s. Protected mode: machine directories, "+
		"VMMs and snapshots the daemon doesn't know about will NOT be deleted or killed until "+
		"that file is recovered or removed", m.statePath(), causa, destino)
}

// detectarCuarentena activa el modo protegido si queda algún state.json
// apartado: en esta arrancada o en una anterior (tras apartarlo, el daemon
// escribe un state.json nuevo con lo que cree, que es menos que la verdad).
func (m *Manager) detectarCuarentena() {
	if m.estadoIlegible != "" {
		return
	}
	apartados, _ := filepath.Glob(m.statePath() + sufijoCuarentena + "*")
	if len(apartados) > 0 {
		m.estadoIlegible = fmt.Sprintf("an unreadable state was set aside at %s", apartados[0])
		log.Printf("state: %s. Protected mode: nothing unknown to the daemon will be deleted or "+
			"killed until it is recovered or removed", m.estadoIlegible)
	}
}

// barridoBloqueado dice si el daemon está en modo protegido (ver load): con un
// estado que no se pudo leer, "no lo conozco" no significa "es basura", y
// nada que decida por ausencia en byID puede borrar ni matar.
func (m *Manager) barridoBloqueado() bool { return m.estadoIlegible != "" }

// persist encola una escritura del estado. Se llama SIEMPRE con m.mu tomado por
// quien la invoca — los doce sitios que la usan están dentro de una operación de
// ciclo de vida.
//
// Antes serializaba y escribía aquí mismo, con el lock global cogido: cada
// transición pagaba la latencia del disco y, mientras tanto, ninguna otra
// máquina podía arrancar, congelarse ni descongelarse. Ahora solo toma la foto y
// se va; escribirla es cosa de persistLoop.
//
// La foto es una copia PROFUNDA (Clone), no el puntero ni una copia por valor.
// Es la diferencia entre esto y una carrera de datos: si se guardaran los
// *api.Machine, json.Marshal los leería fuera del lock mientras otra goroutine
// les cambia State, PID o los punteros StartedAt/FrozenAt, y el fichero podría
// acabar describiendo un estado que nunca existió (una máquina "warm" con PID
// vivo, por ejemplo). Y una copia por valor no basta: comparte con la viva los
// arrays de Volumes, Shares o AllowDomains y los mapas de Labels y Forwards, y
// cualquier escritura en su sitio sobre ellos corre con el Marshal (M-03).
func (m *Manager) persist() {
	list := make([]api.Machine, 0, len(m.byID))
	for _, mc := range m.byID {
		list = append(list, *mc.Clone())
	}

	m.stateMu.Lock()
	m.pendGen++
	m.pending, m.hasPend = list, true
	m.stateMu.Unlock()

	// Aviso no bloqueante: si ya hay uno sin atender, este sobra. Las ráfagas
	// coalescen solas porque cada foto pisa a la anterior, y la última es la
	// que describe el estado actual.
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

const (
	// persistDebounce agrupa las transiciones que llegan juntas.
	persistDebounce = 50 * time.Millisecond
	// persistMaxDelay acota lo que puede posponerse una escritura. Sin este
	// tope, un debounce que se reinicia con cada aviso deja el estado sin
	// escribir indefinidamente mientras haya actividad — justo cuando más
	// importa que esté en disco.
	persistMaxDelay = 250 * time.Millisecond
)

// persistLoop materializa las escrituras fuera del lock.
func (m *Manager) persistLoop() {
	defer m.persistWG.Done()

	var (
		timer *time.Timer
		tC    <-chan time.Time
		first time.Time
	)
	for {
		select {
		case <-m.wake:
			if tC == nil {
				first = time.Now()
				timer = time.NewTimer(persistDebounce)
				tC = timer.C
				continue
			}
			if time.Since(first) >= persistMaxDelay {
				// Ya se aplazó bastante: que dispare el temporizador vigente.
				continue
			}
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(persistDebounce)

		case <-tC:
			tC, timer = nil, nil
			// Contenido: si volcar el estado entrara en panico, el proceso
			// entero moriria y las microVM quedarian huerfanas. Perder UNA
			// escritura es recuperable; perder el daemon, no.
			panico.Contener("persistLoop", m.writePending)

		case <-m.quit:
			if timer != nil {
				timer.Stop()
			}
			panico.Contener("persistLoop (cierre)", m.writePending)
			return
		}
	}
}

// writePending vuelca la última foto, si hay alguna.
func (m *Manager) writePending() {
	m.stateMu.Lock()
	if !m.hasPend {
		m.stateMu.Unlock()
		return
	}
	list, gen := m.pending, m.pendGen
	m.pending, m.hasPend = nil, false
	m.stateMu.Unlock()

	m.escrituraMu.Lock()
	defer m.escrituraMu.Unlock()
	if m.estadoIntocable {
		return // ver ponerEnCuarentena
	}
	if gen <= m.escritoGen {
		return // ya se escribió una foto más nueva
	}

	f := struct {
		esquema.Cabecera
		Machines []api.Machine `json:"machines"`
	}{esquema.Cabecera{Schema: esquemaParaEscribir(list)}, list}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		log.Printf("state: could not serialize it: %v", err)
		return
	}

	// El detalle del temporal, los dos fsync y el rename vive en un solo sitio
	// (internal/durable): tenerlo escrito dos veces era tenerlo bien en uno y
	// mal en el otro, que es lo que pasaba con links.json.
	if err := durable.Escribir(m.statePath(), b, 0o644); err != nil {
		log.Printf("state: could not persist it: %v", err)
		return
	}
	m.escritoGen = gen
}

// persistirYa escribe el estado AHORA, sin esperar al debounce. Se llama SIN m.mu.
//
// Solo antes de lanzar un VMM nuevo. El debounce dejaba hasta 250 ms en los que
// un firecracker ya vivía sin su máquina en disco; si el daemon moría en esa
// ventana, al volver reconcile veía un VMM que nadie conocía y lo mataba —lo
// correcto para un huérfano, lo incorrecto para una máquina que alguien acababa
// de crear—. Con esto, un VMM vivo siempre tiene su registro escrito, y el que
// no lo tiene es un huérfano de verdad.
func (m *Manager) persistirYa() {
	m.mu.RLock()
	m.persist()
	m.mu.RUnlock()
	m.writePending()
}

// Close para la escritura de estado tras volcar lo que quede pendiente.
//
// Lo llama el daemon en su camino de apagado, y tiene que esperarse: si el
// proceso sale antes, se pierde la última transición y el arranque siguiente
// reconstruye un estado que ya no es el real.
func (m *Manager) Close() {
	m.quitOnce.Do(func() {
		// Antes que nada, las carpetas vivas: su cierre ordenado necesita al
		// agente del invitado, no el estado.
		m.drainShares(shareDrainWait)
		close(m.quit)
		m.cerrarBroker()
		m.persistWG.Wait()
	})
}

// ── consultas ─────────────────────────────────────────────────────────────────

func (m *Manager) List() []*api.Machine {
	m.mu.RLock()
	out := make([]*api.Machine, 0, len(m.byID))
	for _, mc := range m.byID {
		// DiskBytes viene de la caché: recorrer el directorio de cada máquina
		// aquí era un walk por máquina en CADA `kling ps`, y encima con el
		// candado global cogido, así que contar bytes bloqueaba arranques y
		// congelaciones. Lo refresca el vigilante cada pocos segundos.
		c := *mc
		c.Transition = m.transicion[mc.ID]
		c.CPUBoostPct = m.impulsoVigente(c.ID, c.State)
		out = append(out, &c)
	}
	m.mu.RUnlock()
	// Fuera del candado: el estado de las carpetas vivas tiene el suyo.
	for _, c := range out {
		m.decorarShares(c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

// allocatedBytes devuelve los bytes realmente ocupados por un fichero, que con
// ficheros dispersos no tiene nada que ver con su tamaño lógico.
func allocatedBytes(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}

// diskUsage suma bloques asignados, no tamaños lógicos: es la única cifra
// honesta cuando los ficheros son dispersos.
// touchDisk recalcula el disco de UNA máquina. Se usa tras las operaciones que
// mueven ficheros, para que la respuesta de esa llamada ya lleve el dato bueno
// en vez del de la última pasada del vigilante.
//
// El walk va fuera del lock; solo la escritura del campo lo toma.
func (m *Manager) touchDisk(id string) int64 {
	n := diskUsage(m.dir(id))
	m.mu.Lock()
	if mc := m.byID[id]; mc != nil {
		mc.DiskBytes = n
	}
	m.mu.Unlock()
	return n
}

// refreshDiskUsage recalcula el disco de todas las máquinas.
//
// Lo llama el vigilante, que ya pasa cada pocos segundos. El overlay es un
// fichero disperso que CRECE mientras el invitado escribe, así que un valor
// que solo se actualizara al arrancar o congelar sería justo el que no sirve:
// el número con el que se detecta a un invitado llenando su disco.
func (m *Manager) refreshDiskUsage() {
	m.mu.RLock()
	ids := make([]string, 0, len(m.byID))
	for id := range m.byID {
		ids = append(ids, id)
	}
	m.mu.RUnlock()

	sizes := make(map[string]int64, len(ids))
	for _, id := range ids {
		sizes[id] = diskUsage(m.dir(id))
	}

	m.mu.Lock()
	for id, n := range sizes {
		if mc := m.byID[id]; mc != nil {
			mc.DiskBytes = n
		}
	}
	m.mu.Unlock()
}

func diskUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				total += st.Blocks * 512
			}
		}
		return nil
	})
	return total
}

// Get resuelve por ID completo, prefijo de ID o nombre, como hace docker.
func (m *Manager) Get(ref string) (*api.Machine, bool) {
	c, ok := m.get(ref)
	if ok {
		// Fuera del candado, como en List.
		m.decorarShares(c)
	}
	return c, ok
}

// get resuelve ref en este orden: ID exacto, nombre exacto, prefijo de ID (4
// caracteres o más). Un nombre o un prefijo que casa con más de una máquina no
// resuelve a ninguna: antes se devolvía la primera del mapa, al azar, y un `rm`
// o la autorización por nombre (internal/daemon/authz.go) podían caer sobre
// otra máquina que se llamaba igual. Los nombres nuevos son únicos
// (reservarNombre); la ambigüedad solo queda para estados de antes.
func (m *Manager) get(ref string) (*api.Machine, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mc, ok := m.byID[ref]; ok {
		c := *mc
		c.Transition = m.transicion[mc.ID]
		c.CPUBoostPct = m.impulsoVigente(c.ID, c.State)
		return &c, true
	}
	var porNombre, porPrefijo []*api.Machine
	for _, mc := range m.byID {
		switch {
		case mc.Name == ref:
			porNombre = append(porNombre, mc)
		case len(ref) >= 4 && strings.HasPrefix(mc.ID, ref):
			porPrefijo = append(porPrefijo, mc)
		}
	}
	for _, l := range [][]*api.Machine{porNombre, porPrefijo} {
		switch len(l) {
		case 0:
			continue
		case 1:
			c := *l[0]
			c.Transition = m.transicion[c.ID]
			c.CPUBoostPct = m.impulsoVigente(c.ID, c.State)
			return &c, true
		}
		return nil, false
	}
	return nil, false
}

// ErrNameTaken: ya hay una máquina (o una que está naciendo) con ese nombre.
var ErrNameTaken = errors.New("machine name is taken")

// reservarNombre aparta nombre para una máquina que va a nacer: falla si ya lo
// lleva otra, si otra está naciendo con él, o si es el ID de otra. La reserva
// se suelta al volver de run; para entonces la máquina ya está en byID, que la
// cubre. Un nombre vacío (el que run genera, con parte del ID) no se reserva.
func (m *Manager) reservarNombre(nombre string) (func(), error) {
	if nombre == "" {
		return func() {}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ocupado := m.nombresNaciendo[nombre] || m.byID[nombre] != nil
	for _, mc := range m.byID {
		if mc.Name == nombre {
			ocupado = true
			break
		}
	}
	if ocupado {
		return nil, fmt.Errorf("%w: %q (kling rm it, or pick another -name)", ErrNameTaken, nombre)
	}
	if m.nombresNaciendo == nil {
		m.nombresNaciendo = map[string]bool{}
	}
	m.nombresNaciendo[nombre] = true
	return func() {
		m.mu.Lock()
		delete(m.nombresNaciendo, nombre)
		m.mu.Unlock()
	}, nil
}

// marcarTransicion apunta que la máquina id está en medio de que (un
// api.Transition*) y devuelve la función que lo retira. La llama quien tiene
// el cerrojo de ciclo de vida de id, justo cuando ya sabe que va a seguir.
//
// Sin esto, un freeze de segundos se veía desde fuera como una máquina
// "running" corriente: el planificador del gateway la adoptaba y enrutaba a
// ella sesiones que morían con el volcado.
func (m *Manager) marcarTransicion(id, que string) func() {
	m.mu.Lock()
	if m.transicion == nil {
		m.transicion = map[string]string{}
	}
	m.transicion[id] = que
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if m.transicion[id] == que {
			delete(m.transicion, id)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}

// netIndexSpace es el rango de /30 disponibles en 172.30.0.0/16.
const netIndexSpace = 16000

// asignarRed asigna la red de una máquina nueva: el siguiente índice libre EN
// ROTACIÓN, no el menor, y libre también en el host (knet.Asignar), con su
// reserva hecha; montarRed la suelta.
//
// Reutilizar el índice más bajo parece más ordenado, pero con máquinas efímeras
// —que nacen y mueren en cientos de milisegundos— significa que la siguiente
// recibe la IP que acaba de liberar la anterior. El host conserva entradas de
// conntrack de la conexión previa y la nueva microVM se come un "connection
// reset by peer".
//
// Rotando, una IP tarda 16.000 máquinas en repetirse. Y mirando el host, no se
// repite la de una máquina de otro daemon (ver internal/net/subredes.go).
func (m *Manager) asignarRed(id string) (*knet.Net, error) {
	m.mu.RLock()
	used := make(map[int]bool, len(m.byID))
	for _, mc := range m.byID {
		used[mc.NetIndex] = true
	}
	m.mu.RUnlock()

	m.netMu.Lock()
	defer m.netMu.Unlock()
	return knet.Asignar(&m.netCursor, netIndexSpace, func(i int) bool { return used[i] }, id)
}

// ── ciclo de vida ─────────────────────────────────────────────────────────────

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Run crea una microVM y la arranca en frío.
func (m *Manager) Run(ctx context.Context, req api.RunRequest) (*api.Machine, error) {
	mc, err := m.run(ctx, req)
	if err != nil || !req.WaitReady {
		return mc, err
	}
	// -wait-ready: la máquina se devuelve igual si no llega; Ready dice cómo
	// quedó y quien pidió decide (el CLI sale con error).
	plazo := time.Duration(min(req.ReadyTimeoutSeconds, api.ReadyMaxWaitSeconds)) * time.Second
	res, werr := m.WaitReady(ctx, mc.ID, OpcionesListo{Plazo: plazo})
	mc.Ready = res.Ready
	if werr != nil && res.Ready == api.ReadyUnknown {
		mc.Ready = api.ReadyWaiting
	}
	return mc, nil
}

func (m *Manager) run(ctx context.Context, req api.RunRequest) (*api.Machine, error) {
	// Jailer bloqueado: lo PRIMERO, antes de reservar ni publicar nada. Más
	// tarde el abort pasaba por fail() y cada intento dejaba una entrada
	// fallida en byID que contaba para checkMachineLimit. El check de boot()
	// se queda como red.
	if m.JailerBlocked != "" {
		return nil, errors.New(m.JailerBlocked)
	}
	// El tope va ANTES de bifurcar: restaurar desde un snapshot es tan capaz de
	// agotar el host como arrancar en frío, y es el camino que más rápido crea
	// —gateway, fondo, efímero—. Comprobarlo solo en el arranque en frío lo
	// dejaba sin freno justo donde más falta hace.
	if err := m.checkMachineLimit(); err != nil {
		return nil, err
	}
	switch req.OnTTL {
	case "", api.OnTTLFreeze:
		req.OnTTL = ""
	case api.OnTTLRemove:
	default:
		return nil, fmt.Errorf("invalid on_ttl %q: use %q or %q", req.OnTTL, api.OnTTLFreeze, api.OnTTLRemove)
	}
	// El entorno de la máquina (entorno.go): validado antes de reservar
	// nada. Desde aquí solo viaja en env, nunca en la máquina.
	env, err := entornoDePeticion(req)
	if err != nil {
		return nil, err
	}
	req.Env = nil
	// El nombre es único: autorizar, borrar o entrar por nombre tiene que
	// llevar siempre a la misma máquina (docs/authz.md).
	soltarNombre, err := m.reservarNombre(req.Name)
	if err != nil {
		return nil, err
	}
	defer soltarNombre()

	// Instanciar desde un snapshot dorado es un camino distinto: no se arranca
	// nada en frío, se restaura.
	if req.From != "" {
		// Las carpetas se deciden al arrancar en frío: una copia es un disco,
		// y a una máquina restaurada no se le añaden discos.
		if len(req.Shares) > 0 {
			return nil, fmt.Errorf("%w: shared folders need a cold boot; they cannot be added to a machine restored from a snapshot", ErrShareRequest)
		}
		return m.runFrom(ctx, req)
	}
	if req.Image == "" {
		req.Image = "default"
	}
	if req.VCPUs <= 0 {
		req.VCPUs = 1
	}
	// El disco escribible: disperso, así que un tamaño grande no cuesta nada
	// hasta que se escribe, pero uno diminuto no monta ni el agente.
	if tope := maxDiskMiB(); req.DiskMiB < 0 || (req.DiskMiB > 0 && req.DiskMiB < minOverlayMiB) || req.DiskMiB > tope {
		return nil, fmt.Errorf("disk_mib %d: the machine's writable disk goes from %d MiB to %d MiB (0 = %d; KLING_MAX_DISK_MIB sets the maximum)",
			req.DiskMiB, minOverlayMiB, tope, defaultOverlayMiB)
	}
	// Disperso, pero el invitado puede llenarlo: uno más grande que el de
	// siempre tiene que caber en lo que queda libre.
	if req.DiskMiB > defaultOverlayMiB {
		if err := m.checkDiskParaOverlay(req.DiskMiB); err != nil {
			return nil, err
		}
	}
	if req.MemMiB <= 0 {
		req.MemMiB = 256
	}
	if req.MemMaxMiB != 0 && req.MemMaxMiB < req.MemMiB {
		return nil, fmt.Errorf("mem_max_mib (%d) can't be below mem_mib (%d)", req.MemMaxMiB, req.MemMiB)
	}
	if req.MemMaxMiB == req.MemMiB {
		req.MemMaxMiB = 0 // un techo igual a la memoria es memoria fija
	}
	// El techo de CPU: el flag > la receta de la imagen > el valor por defecto
	// de quien pide > el del daemon (este último, tras arrancar).
	// Un flag explícito, además, no lleva impulso de arranque (arranque_cpu.go).
	cpuFijo := req.CPUPct > 0
	if req.CPUPct <= 0 {
		req.CPUPct = m.techoCPUPorDefecto(req.Image, req.VCPUs, req.CPUPctDefault)
	}

	if err := m.checkMachineLimit(); err != nil {
		return nil, err
	}

	// El id se genera AQUI y no mas abajo: es la llave de la reserva, y la
	// reserva tiene que existir antes de que empiece nada lento.
	id := newID()
	if req.Name == "" {
		req.Name = req.Image + "-" + id[:6]
	}

	vols, err := m.reservarVolumenes(req, id, req.Name)
	// Se suelta pase lo que pase: si la maquina llego a byID, byID ya la cubre;
	// si no llego, la reserva no puede quedarse bloqueando el volumen.
	defer m.soltarReservas(id)
	if err != nil {
		return nil, err
	}
	shares, err := m.resolveShares(ctx, req, vols)
	if err != nil {
		return nil, err
	}
	for _, v := range vols {
		// Solo los que vamos a montar en ESCRITURA. e2fsck escribe, y un volumen
		// compartido puede tenerlo abierto otra microVM ahora mismo: repararlo
		// por debajo sería justo la corrupción que el modo lectura evita.
		if !v.readOnly {
			repairVolume(ctx, v.path)
		}
	}

	// src es el rootfs que va en vda; layer, el delta del servicio si la imagen
	// va por capas (vacío en las monolíticas de siempre).
	src, layer, err := m.imageLayer(req.Image)
	if err != nil {
		return nil, err
	}
	// Una base anterior a las capas ignoraría kling.layer y arrancaría el
	// invitado sin el servicio dentro. Se comprueba aquí, donde se puede explicar,
	// y no allí, donde se ve como un pánico del kernel.
	if layer != "" {
		switch ok, cerr := m.baseSupportsLayers(ctx, src); {
		case errors.Is(cerr, ErrNoDebugfs):
			// Un Mac sin e2fsprogs de Homebrew: se sabe desde el arranque del
			// daemon (ya lo avisa) y repetirlo en cada run solo ensucia el log.
		case cerr != nil:
			// Sin poder comprobarlo se sigue: convertir una comprobación de
			// diagnóstico en una dependencia de arranque sería peor que el problema.
			log.Printf("warning: could not check whether base %s understands layers: %v", src, cerr)
		case !ok:
			return nil, fmt.Errorf("image %q is layered, but its base (%s) has an overlay-init "+
				"from before layers existed: it would ignore %s and boot the guest without the "+
				"service inside.\nRebuild the base (scripts/70-build-minimal-image.sh) or "+
				"repackage this service as a monolithic image",
				req.Image, src, api.LayerBootParam)
		}
	}
	// Antes de comprometer nada: una microVM que no cabe no falla al arrancar,
	// arranca — y luego el OOM killer del anfitrión mata procesos al azar.
	//
	// reserveMemory y no un check suelto: reserva la memoria durante todo el
	// arranque para que dos microVMs concurrentes no vean la misma memoria libre
	// y pasen las dos. La reserva se libera al salir —el defer cubre todos los
	// returns—: en un fallo, la memoria nunca se ocupó; en el éxito, ya la ocupa
	// el proceso, así que "pendiente" deja de tener sentido.
	if err := m.admitir(); err != nil {
		return nil, err
	}
	releaseMem, err := m.reserveMemoryMakingRoom(ctx, req.MemMiB, "", "")
	if err != nil {
		return nil, err
	}
	defer releaseMem()
	if _, err := os.Stat(m.KernelPath()); err != nil {
		return nil, fmt.Errorf("missing kernel at %s", m.KernelPath())
	}

	// Sin agente de invitado no hay quien monte los volúmenes.
	//
	// El agente (kling-guest, o el puente de kindling-mcp que lo embebe) es lo
	// único que lee kling.volume y monta dentro del invitado.
	// Una imagen en modo HTTP nativo no lo lleva, así que el disco se
	// engancharía, el snapshot lo registraría y `volume ls` diría "en uso"…
	// mientras dentro nadie monta nada y todo lo escrito muere con la máquina.
	// Se comprueba ANTES de crear el directorio, para no tener que limpiarlo.
	if len(vols) > 0 || len(shares) > 0 {
		switch has, herr := m.imageHasBridgeCached(ctx, src, layer); {
		case herr != nil:
			// Sin poder comprobarlo se sigue, dejando constancia: convertir una
			// herramienta de diagnóstico en una dependencia de arranque sería
			// peor que el problema.
			log.Printf("warning: could not check whether %q has a bridge: %v", req.Image, herr)
		case !has:
			if len(vols) == 0 {
				return nil, fmt.Errorf("%w: image %q has no guest agent (kling-guest or kling-bridge), and the agent "+
					"is what mounts shared folders inside the guest.\n"+
					"Use an image with an agent (kling images toolchain, or kling images build -builder base)",
					ErrShareRequest, req.Image)
			}
			return nil, fmt.Errorf("image %q has no guest agent (kling-guest or kling-bridge), and the agent "+
				"is what mounts the volumes inside the guest.\n"+
				"With this image the disk would be attached but nobody would mount it, and everything written "+
				"to %s would die with the machine, without a single error.\n"+
				"Rebuild it with an agent (kling images build -builder base, or kindling-mcp in stdio mode), or remove the volume",
				req.Image, vols[0].mount)
		}
	}

	// El directorio nace reservado: la ventana hasta byID es más corta que en
	// runFrom, pero newOverlay sigue formateando un ext4 en medio. Ver
	// makeMachineDir.
	dir, unreserve, err := m.makeMachineDir(id)
	if err != nil {
		return nil, err
	}
	defer unreserve()

	// La imagen base no se copia: se comparte en solo lectura. Lo único propio de
	// esta microVM es su overlay escribible, que nace prácticamente vacío.
	overlay := filepath.Join(dir, "overlay.ext4")
	if err := m.newOverlay(ctx, overlay, req.DiskMiB); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	// Las copias: su ext4 pasa de la subida al directorio de la máquina y se
	// engancha como un disco de solo lectura DETRÁS de los volúmenes. Para el
	// invitado es un volumen de solo lectura más (kling.volume=…:ro), así que lo
	// monta incluso un agente anterior a las carpetas compartidas.
	copies, err := m.placeCopies(dir, shares)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	var shareAtts []api.ShareAttachment
	for _, s := range shares {
		shareAtts = append(shareAtts, s.att)
	}

	creada := time.Now()
	mc := &api.Machine{
		ID: id, Name: req.Name, Image: req.Image, State: api.StateCreated,
		VCPUs: req.VCPUs, MemMiB: req.MemMiB, MemMaxMiB: req.MemMaxMiB, DiskMiB: req.DiskMiB, CreatedAt: creada,
		TTLSeconds: req.TTLSeconds, CPUPct: req.CPUPct, CPUPctFixed: cpuFijo, Labels: req.Labels,
		Volumes:   attachments(vols),
		Shares:    shareAtts,
		AllowExec: req.AllowExec, OnTTL: req.OnTTL,
		TTLAt:   &creada,
		EnvKeys: api.MachineEnvKeys(env),
	}
	// El cerrojo de ciclo de vida, desde ANTES de publicarla hasta que queda
	// running o fallida. Sin él, un Remove (un `rm`, el TTL con on_ttl=remove,
	// gcFailed) veía la máquina en created con PID 0, no mataba nada, borraba
	// su directorio y su entrada… mientras boot() lanzaba el firecracker: un
	// VMM huérfano reteniendo RAM, invisible para `kling ps`, y un 201 para
	// una máquina que ya no existía (M-06). Ahora esperan a que termine.
	//
	// Justo antes de publicar y no nada más generar el id: hasta aquí nadie
	// puede nombrarla, y lo de arriba (checkMachineLimit → gcFailed) sí puede
	// esperar el cerrojo de OTRA máquina. Ver doc.go.
	soltarCiclo := m.lockUnaVez(id)
	defer soltarCiclo()
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvCreated, ID: id, Name: mc.Name})

	// abandonar deshace lo publicado: la máquina ya está en byID, y dejarla ahí
	// tras un fallo crea un fantasma que no se puede arrancar (no hay `start`) y
	// que RETIENE su volumen en exclusiva, porque volumeUsers cuenta todo lo que
	// no esté stopped o failed. Nadie podría montarlo hasta un `rm` a mano.
	abandonar := func(err error) (*api.Machine, error) {
		m.mu.Lock()
		delete(m.byID, id)
		delete(m.socket, id)
		m.persist()
		m.mu.Unlock()
		os.RemoveAll(dir)
		return nil, err
	}
	if prueba := m.pruebaTrasPublicar; prueba != nil {
		if err := prueba(id); err != nil {
			return abandonar(err)
		}
	}

	egress, err := knet.ParseEgress(req.Egress)
	if err != nil {
		return abandonar(err)
	}
	netcfg, err := m.asignarRed(id)
	if err != nil {
		return abandonar(err)
	}
	if err := m.montarRed(netcfg, id, egress, req.AllowDomains); err != nil {
		return abandonar(fmt.Errorf("mounting the network: %w", err))
	}
	// Bajo el candado: mc ya está en byID, y List()/Get()/persist() la copian
	// desde otras goroutines. Es la misma regla por la que boot() devuelve el PID
	// en vez de escribirlo él.
	m.mu.Lock()
	mc.IP, mc.NetIndex, mc.Egress = netcfg.NSIP, netcfg.Index, string(egress)
	mc.AllowDomains = req.AllowDomains
	m.mu.Unlock()

	// El VMM solo puede escribir en lo suyo: su directorio y su overlay.
	if err := m.priv.Own(dir, overlay); err != nil {
		m.desmontarRed(netcfg, id)
		return abandonar(err)
	}

	start := time.Now()
	m.persistirYa()
	pid, err := m.boot(ctx, mc.ID, mc.VCPUs, mc.MemMiB, mc.MemMaxMiB, src, layer, overlay, netcfg, append(vols, copies...), req.AllowExec, m.ipv6DeReceta(mc.Image), env)
	if err != nil {
		// boot() devuelve el PID aunque falle DESPUÉS de lanzar el proceso, y
		// hay que matarlo aquí: m.fail() llama a kill(), que lee el PID de la
		// máquina — y ahí todavía es 0. Sin esto queda un firecracker vivo para
		// siempre esperando en un socket que ya nadie usa.
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		m.desmontarRed(netcfg, id)
		m.fail(mc, err)
		return nil, err
	}
	if mc.CPUPct <= 0 {
		m.mu.Lock()
		mc.CPUPct = techoDelDaemon(mc.VCPUs)
		m.mu.Unlock()
	}
	// El impulso de arranque mientras arranca el invitado, que es lo que viene
	// ahora (boot() vuelve tras Start); el techo configurado, en cuanto pasa su
	// sonda de listo (o contesta su agente, si no declara sonda). El defer lo
	// baja en cualquier salida de aquí en adelante, salvo la de éxito, que se
	// lo entrega a quien espera el fin del arranque. Ver arranque_cpu.go.
	impulso := m.nuevoImpulso(mc.ID, mc.CPUPct, mc.VCPUs, mc.CPUPctFixed)
	defer impulso.fin()
	if warn := m.limitCPU(mc.ID, pid, impulso.tope); warn != "" {
		log.Printf("warning: %s: %s", mc.Name, warn)
	}

	m.mu.Lock()
	now := time.Now()
	mc.PID = pid
	mc.State = api.StateRunning
	mc.StartedAt = &now
	mc.BootMS = time.Since(start).Milliseconds()
	m.persist()
	out := *mc
	m.mu.Unlock()

	// El walk va DESPUÉS de soltar el lock, y su resultado entra en la
	// respuesta: si se dejara solo al vigilante, esta llamada devolvería 0 y
	// quien la hizo vería una máquina sin disco.
	out.DiskBytes = m.touchDisk(id)

	// Las carpetas vivas: se conectan ahora y se espera al primer attach, para
	// que quien arranca la máquina la reciba con la carpeta ya montada. Si el
	// agente no sabe (imagen vieja) o no puede (kernel sin FUSE), la máquina
	// no sirve para lo que se pidió: se destruye y se dice por qué.
	if hasLiveShares(&out) {
		m.startShares(id)
		if err := m.waitShares(ctx, id, shareAttachWait); err != nil {
			// Remove toma el cerrojo de la máquina, que es nuestro: soltarlo
			// antes o se esperaría a sí mismo para siempre.
			soltarCiclo()
			_ = m.Remove(id)
			return nil, err
		}
		m.decorarShares(&out)
	}

	// Desde aquí baja el techo la goroutine que espera el fin del arranque:
	// con carpetas vivas el agente ya contestó (waitShares).
	impulso.entregar()
	// Para `kling ps`: si la imagen declara una sonda, cuándo termina de
	// arrancar (listo.go). En segundo plano; -wait-ready espera aparte.
	m.vigilarListo(id, nil)
	// Y qué agente lleva (agente.go). Un arranque en frío puede estrenar
	// imagen reconstruida: lo que se supiera de antes no vale.
	m.olvidarAgente(id)
	m.conocerAgente(id)
	if len(env) > 0 {
		m.retirarEntornoMMDS(id, mc.Name, env)
	}
	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: id, Name: mc.Name,
		Message: fmt.Sprintf("cold started in %d ms", out.BootMS)})
	return &out, nil
}

// shareAttachWait es cuánto se espera al primer attach de una carpeta viva al
// arrancar: lo que tarda el agente en escuchar en un host cargado, con margen.
const shareAttachWait = 2 * time.Minute

// createOverlay crea el disco escribible de una microVM: un fichero disperso con
// ext4 encima. Sin journal a propósito — es almacenamiento efímero y el journal
// costaría varios MB de suelo en cada máquina sin aportar nada aquí.
// overlayTemplatePath es un overlay ya formateado que se copia para cada
// microVM nueva, en vez de correr mkfs.ext4 una vez por máquina.
func (m *Manager) overlayTemplatePath() string {
	return filepath.Join(m.root, "images", "overlay-template.ext4")
}

// ensureOverlayTemplate la construye si falta.
//
// Se formatea en un .tmp y se renombra con durable.Renombrar, para que EXISTIR
// IMPLIQUE ESTAR COMPLETA. Comprobar solo la existencia sobre el nombre
// definitivo sería una trampa: si el daemon muere entre el Truncate y el mkfs
// queda medio giga de ceros sin sistema de ficheros, y a partir de ahí TODAS
// las microVMs arrancan con un /dev/vdb que no monta — un fallo que aparece
// dentro del invitado y no en el log del daemon. Un rename sin fsync previo
// del temporal ni del directorio deja la misma trampa abierta tras un corte
// de luz: el rename puede haber quedado solo en la cache.
func (m *Manager) ensureOverlayTemplate(ctx context.Context) error {
	m.templateMu.Lock()
	defer m.templateMu.Unlock()

	path := m.overlayTemplatePath()
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := createOverlay(ctx, tmp, defaultOverlayMiB); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := durable.Renombrar(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// newOverlay deja listo el disco escribible de una microVM, de sizeMiB (0: el
// de siempre, defaultOverlayMiB).
//
// Copiar la plantilla ahorra el mkfs.ext4 por máquina, que son decenas de
// milisegundos sobre un arranque que aspira a estar en el orden de los 30 ms.
// Si la plantilla no se puede construir se formatea directamente: más lento,
// pero nadie se queda sin arrancar por una optimización. Un tamaño distinto
// del de la plantilla se formatea directamente también: es la excepción (una
// imagen de Docker que escribe GiB en su propio disco) y esos milisegundos
// no cuentan frente a lo que esa máquina va a hacer.
func (m *Manager) newOverlay(ctx context.Context, dst string, sizeMiB int) error {
	if sizeMiB > 0 && sizeMiB != defaultOverlayMiB {
		return createOverlay(ctx, dst, sizeMiB)
	}
	if err := m.ensureOverlayTemplate(ctx); err != nil {
		log.Printf("overlay template not available (%v): formatting directly", err)
		return createOverlay(ctx, dst, defaultOverlayMiB)
	}
	// Sin perder la dispersión (ver copiarDisco): el overlay es disperso y
	// copiarlo denso destruiría lo que hace que una máquina cueste ~8 MB en vez
	// de 512.
	out, err := m.copiarOverlay(ctx, m.overlayTemplatePath(), dst)
	if err != nil {
		return fmt.Errorf("copying overlay template: %v: %s", err, out)
	}
	return nil
}

func createOverlay(ctx context.Context, path string, sizeMiB int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := f.Truncate(int64(sizeMiB) << 20); err != nil {
		f.Close()
		return err
	}
	f.Close()

	// -E nodiscard evita que mke2fs escriba ceros y destruya la dispersión.
	out, err := e2fsCmd(ctx, "mkfs.ext4",
		"-q", "-F", "-O", "^has_journal", "-E", "nodiscard", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("formatting overlay: %v: %s", err, out)
	}
	return nil
}

// boot lanza el proceso firecracker y configura la microVM por su API.
//
// Dos discos: la imagen base compartida en solo lectura y el overlay propio. Con
// una imagen por capas hay un tercero de solo lectura, la capa del servicio, que
// se engancha DETRÁS de los volúmenes para no correrles la letra.
//
// Devuelve el PID en vez de escribirlo en la estructura: quien llama lo asigna
// bajo el mutex. Escribirlo aquí sería una carrera con List(), que copia las
// máquinas concurrentemente.
func (m *Manager) boot(ctx context.Context, id string, vcpus, memMiB, memMaxMiB int, base, layer, overlay string, n *knet.Net, vols []resolvedVolume, allowExec, ipv6Stack bool, env map[string]string) (int, error) {
	// Puerta de arranque: el encendido en frío crea los vCPU y los pone a correr
	// en KVM (c.Start más abajo). Que no lo hagan doce a la vez, o el kernel del
	// host se cuelga bajo anidamiento. boot() devuelve justo tras Start, así que el
	// defer suelta el hueco en cuanto la microVM está encendida y el siguiente pasa.
	release, glErr := m.enterLaunch(ctx)
	if glErr != nil {
		return 0, glErr
	}
	defer release()

	// El device de la capa se calcula ANTES de lanzar nada: si no cabe, mejor no
	// haber encendido un firecracker que luego hay que matar.
	var layerDev string
	if layer != "" {
		var err error
		if layerDev, err = layerDevice(len(vols)); err != nil {
			return 0, err
		}
	}

	var sock string
	var pid int
	var err error
	if m.JailerBlocked != "" {
		return 0, errors.New(m.JailerBlocked)
	}
	if m.jailerJailed {
		// Arranque en frío dentro del jail. A diferencia de la restauración, aquí
		// firecracker abre el KERNEL (SetBootSource) y los discos por su API, así
		// que también hay que replicar el kernel dentro del chroot.
		pid, sock, _, err = m.spawnJailed(id, n, nil)
		if err != nil {
			return 0, err
		}
		// layer va con cadena vacía si la imagen es monolítica; prepareJail las
		// ignora.
		toLink := []string{m.KernelPath(), base, layer, overlay}
		for _, v := range vols {
			toLink = append(toLink, v.path)
		}
		if err := m.prepareJail(id, toLink...); err != nil {
			return pid, err
		}
	} else {
		sock = filepath.Join(m.dir(id), "fc.sock")
		_ = os.Remove(sock)
		pid, _, err = m.spawn(id, sock, n, nil)
		if err != nil {
			return 0, err
		}
	}

	c := fc.New(sock)
	if err := waitSocket(ctx, c); err != nil {
		return pid, err
	}
	// El entorno de la máquina va por MMDS, que cuelga de la red.
	if len(env) > 0 && n == nil {
		return pid, fmt.Errorf("%w: it travels via MMDS, and this machine has no network", ErrEnvRequest)
	}
	args := bootArgs(attachments(vols), allowExec, layerDev, ipv6Stack) + envBootArg(len(env) > 0)
	if err := c.SetBootSource(ctx, fc.BootSource{KernelImagePath: m.KernelPath(), BootArgs: args}); err != nil {
		return pid, err
	}
	// vda: base compartida. is_read_only es lo que hace segura la compartición.
	if err := c.SetDrive(ctx, fc.Drive{
		DriveID: "rootfs", PathOnHost: base, IsRootDevice: true, IsReadOnly: true,
		RateLimiter: fc.Limit(diskBytesPerSec),
	}); err != nil {
		return pid, err
	}
	// vdb: capa escribible propia de esta microVM. Muere con ella.
	if err := c.SetDrive(ctx, fc.Drive{
		DriveID: "overlay", PathOnHost: overlay, IsRootDevice: false, IsReadOnly: false,
		RateLimiter: fc.Limit(diskBytesPerSec),
	}); err != nil {
		return pid, err
	}
	// vdc en adelante: los volúmenes, que son lo único que SOBREVIVE a la
	// máquina. El host no los monta mientras estén aquí: dos sistemas
	// escribiendo el mismo ext4 es corrupción segura.
	//
	// El orden de este bucle es el orden de las letras de disco, y el mismo en
	// que viajan los puntos de montaje al invitado. No es un detalle de estilo:
	// alterarlo montaría cada volumen en el sitio de otro.
	for i, v := range vols {
		if err := c.SetDrive(ctx, fc.Drive{
			DriveID: volumeDriveID(i), PathOnHost: v.path, IsRootDevice: false,
			// De solo lectura para el propio VMM, no solo en el mount: la
			// barrera no puede depender de que el invitado se porte bien.
			IsReadOnly:  v.readOnly,
			RateLimiter: fc.Limit(diskBytesPerSec),
		}); err != nil {
			return pid, err
		}
	}
	// La capa del servicio, EL ÚLTIMO. Va aquí y no junto a la base porque las
	// letras de disco salen del orden de enganche: colarla antes correría los
	// volúmenes, y el puente los cuenta desde vdc por posición. Su device viaja
	// en kling.layer=, que ya se calculó arriba a partir de este mismo orden.
	//
	// De solo lectura como la base: es compartida por todas las microVMs del
	// servicio, y lo que las hace seguras de compartir es que nadie escriba.
	if layer != "" {
		if err := c.SetDrive(ctx, fc.Drive{
			DriveID: layerDriveID, PathOnHost: layer, IsRootDevice: false, IsReadOnly: true,
			RateLimiter: fc.Limit(diskBytesPerSec),
		}); err != nil {
			return pid, err
		}
	}
	if n != nil {
		// tap0 se llama igual en todos los namespaces: por eso el snapshot vale
		// para cualquier instancia.
		if err := c.SetNetwork(ctx, fc.NetworkInterface{
			IfaceID: "eth0", HostDevName: knet.TapName, GuestMAC: knet.GuestMAC,
			RxLimiter: fc.Limit(netBytesPerSec), TxLimiter: fc.Limit(netBytesPerSec),
		}); err != nil {
			return pid, err
		}
		// MMDS por eth0: habilita el metadata service para poder INYECTAR secretos
		// de sesión (un token, una credencial) en la microVM ya viva, sin hornearlos
		// en la imagen compartida ni en el snapshot dorado. Va tras la red (necesita
		// una interfaz) y antes de Start (Firecracker no lo admite en caliente), así
		// que queda grabado en el snapshot dorado y las copias lo heredan.
		//
		// No es fatal, como el balloon: si la versión de Firecracker lo rechaza o el
		// invitado no trae ruta a 169.254.169.254, la microVM arranca igual y solo se
		// pierde la inyección de secretos por MMDS.
		if err := c.SetMMDS(ctx, []string{"eth0"}); err != nil {
			// Sin MMDS el entorno de la máquina no llega: el agente no
			// arrancaría el servicio. Mejor no arrancar.
			if len(env) > 0 {
				return pid, fmt.Errorf("configuring MMDS, which carries the machine environment: %w", err)
			}
			log.Printf("warning: could not configure MMDS on %s: %v (no session secrets available)", id, err)
		} else if len(env) > 0 {
			if err := ponerEntornoMMDS(ctx, c, env); err != nil {
				return pid, err
			}
		}
	}
	// virtio-rng: sin esto, las instancias de un mismo snapshot clonarían el
	// estado del generador de aleatoriedad y podrían producir las mismas claves.
	if err := c.SetEntropy(ctx); err != nil {
		return pid, fmt.Errorf("adding entropy: %w", err)
	}
	// Con techo, el VMM arranca con el techo y el globo retiene la diferencia:
	// el invitado dispone de memMiB, y subirlas o bajarlas es mover el globo
	// (Resize). Firecracker no admite añadir memoria en caliente, así que el
	// techo se fija aquí y queda en el snapshot.
	tamano, globo := memMiB, 0
	if memMaxMiB > memMiB {
		tamano, globo = memMaxMiB, memMaxMiB-memMiB
	}
	if err := c.SetMachineConfig(ctx, fc.MachineConfig{VCPUCount: vcpus, MemSizeMiB: tamano}); err != nil {
		return pid, err
	}
	// virtio-balloon a 0: no reclama nada al arrancar, pero deja el dispositivo
	// listo para que un "squeeze" posterior devuelva RAM al host entre sesiones.
	// Va ANTES de Start (Firecracker no lo admite en caliente) y así queda dentro
	// del snapshot dorado, heredado por las copias. No es fatal: si el kernel
	// invitado no trae el driver o la versión de Firecracker lo rechaza, la
	// microVM arranca igual y solo se pierde el squeeze.
	if err := c.SetBalloon(ctx, globo, true, balloonStatsPollSec); err != nil {
		if globo > 0 {
			// Sin globo no hay forma de retener la diferencia: el invitado
			// vería el techo entero, que no es lo que se pidió.
			return pid, fmt.Errorf("mem_max_mib needs the balloon, and it could not be configured: %w", err)
		}
		log.Printf("warning: could not configure balloon on %s: %v (squeeze will not be available)", id, err)
	}
	// En macOS, la política de salida se le da al ayudante aquí, antes de crear
	// la VM; en Linux ya la aplicó el namespace y esto no hace nada.
	if err := m.redAntesDeArrancar(ctx, c, id); err != nil {
		return pid, err
	}
	if err := c.Start(ctx); err != nil {
		return pid, err
	}
	// Y los reenvíos de puertos, sin los que en macOS el host no llega al
	// invitado. No hacen falta en Linux.
	if err := m.abrirReenvios(ctx, c, id); err != nil {
		return pid, err
	}
	m.mu.Lock()
	m.socket[id] = sock
	m.mu.Unlock()
	return pid, nil
}

// spawn arranca firecracker desacoplado del daemon y devuelve su PID.
//
// cg, si no es nil, es el cgroup en el que nace el proceso (cgroupParaLanzar);
// enCg dice si de verdad nació dentro. Si el kernel no lo admite, se lanza
// fuera y quien llama lo mete con limitCPU.
func (m *Manager) spawn(id, sock string, n *knet.Net, cg *os.File) (pid int, enCg bool, err error) {
	logf, err := abrirConsola(m.dir(id))
	if err != nil {
		return 0, false, err
	}
	// Firecracker corre DENTRO del namespace de la microVM: es donde vive su tap0.
	// Orden: primero el namespace (necesita privilegios), después soltarlos.
	argv := m.priv.Wrap(append([]string{m.fcBin, "--api-sock", sock}, argsVMM()...))
	if n != nil {
		argv = n.Wrap(argv[0], argv[1:]...)
	}
	cmd, enCg, err := arrancarEnCgroup(argv, logf, cg, m.entornoVMM()...)
	if err != nil {
		logf.Close()
		return 0, false, fmt.Errorf("launching firecracker: %w", err)
	}
	// Sin Wait() el proceso quedaría zombi al terminar.
	go func() { _ = cmd.Wait(); logf.Close() }()
	return cmd.Process.Pid, enCg, nil
}

// arrancarEnCgroup lanza argv desacoplado (setsid) con su salida en logf,
// dentro del cgroup cg si se puede. Si el kernel rechaza nacer en el cgroup
// (sin CLONE_INTO_CGROUP, o el cgroup no admite procesos), lo lanza fuera y lo
// dice: el proceso importa más que ahorrarse la migración.
func arrancarEnCgroup(argv []string, logf *os.File, cg *os.File, entorno ...string) (*exec.Cmd, bool, error) {
	nuevo := func() *exec.Cmd {
		cmd := exec.Command(argv[0], argv[1:]...)
		if len(entorno) > 0 {
			cmd.Env = append(os.Environ(), entorno...)
		}
		cmd.Stdout, cmd.Stderr = logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return cmd
	}
	if cg != nil {
		cmd := nuevo()
		enCgroup(cmd.SysProcAttr, cg)
		if err := cmd.Start(); err == nil {
			return cmd, true, nil
		}
	}
	cmd := nuevo()
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	return cmd, false, nil
}

// cacheTibiaMaxBytes es hasta qué tamaño (bloques asignados) se deja en la caché
// de página el mem.file de una máquina recién congelada. Una tarea Chispa pesa
// ~70 MiB; un VON, más de un GiB, y ese sí se suelta al congelar.
//
// 256 y no 128 MiB por las copias de kling db branch: una copia del golden de
// Postgres (1 GiB configurado) deja ~211 MiB asignados, y cada `git checkout`
// descongela una. Medido en fc-test con la misma copia: despertar en 12,7 ms
// con el mem.file en caché frente a 69-129 ms sin él (casi todo en el resync,
// con el invitado leyendo sus páginas del disco). La caché es recuperable y se
// suelta con la red a los redDormidaMax.
const cacheTibiaMaxBytes = 256 << 20

// precargaMaxBytes es hasta qué tamaño (bloques asignados) se precarga el
// mem.file de una máquina al descongelarla.
const precargaMaxBytes = 512 << 20

// waitSocket espera a que el socket de la API de un VMM recién lanzado
// conteste. El VMM tarda unos pocos milisegundos en abrirlo: se sondea cada
// milisegundo los primeros 200 ms (un Ping a un socket Unix cuesta decenas de
// µs) y cada 10 ms después. Con un paso fijo de 10 ms cada thaw pagaba un paso
// entero de espera (10,7 ms medidos) para un socket listo en ~1 ms.
func waitSocket(ctx context.Context, c *fc.Client) error {
	start := time.Now()
	deadline := start.Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Ping(ctx) == nil {
			return nil
		}
		paso := time.Millisecond
		if time.Since(start) > 200*time.Millisecond {
			paso = 10 * time.Millisecond
		}
		time.Sleep(paso)
	}
	return fmt.Errorf("firecracker socket did not respond within 5s")
}

// errYaNoToca es la respuesta de freezeSi y removeSi cuando, con el cerrojo
// tomado, la máquina ya no cumple lo que justificaba la operación.
var errYaNoToca = errors.New("the machine no longer qualifies for this operation")

// Freeze pausa la microVM, la vuelca a disco y libera su RAM y su proceso.
func (m *Manager) Freeze(ctx context.Context, ref string) (*api.Machine, error) {
	return m.freezeSi(ctx, ref, nil)
}

// freezeSi es Freeze, pero solo si sigue valiendo sigue(máquina) con el
// cerrojo de ciclo de vida ya tomado; si no, errYaNoToca y no se toca nada.
// sigue nil es congelar siempre.
//
// Es para quien decide congelar mirando una foto (el TTL): entre la foto y el
// cerrojo pudo llegar un renew, y congelar entonces era congelar una máquina
// que su dueño acababa de pedir conservar.
func (m *Manager) freezeSi(ctx context.Context, ref string, sigue func(*api.Machine) bool) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()

	// Se vuelve a leer con el cerrojo tomado: mientras lo esperábamos pudo
	// congelarla otro, borrarla, o terminar de arrancar (Run lo tiene desde que
	// la publica hasta que queda running). Seguir con la copia de antes era
	// rechazar una máquina recién arrancada por estar "created".
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	if cur.State == api.StateWarm {
		return cur, nil
	}
	if sigue != nil && !sigue(cur) {
		return nil, errYaNoToca
	}
	mc = cur
	// Una pausada se reanuda antes: hay que vaciar sus volúmenes y preguntar
	// por su agente, y un invitado pausado no contesta a nada.
	if mc.State == api.StatePaused {
		r, err := m.reanudarLocked(ctx, mc, nil)
		if err != nil {
			return nil, err
		}
		mc = r
	}
	if mc.State != api.StateRunning {
		return nil, fmt.Errorf("only a running machine can be frozen (it is %s)", mc.State)
	}

	// Negativa deliberada: una máquina con secretos inyectados por MMDS NO se
	// congela. El secreto vive en la RAM del invitado, y Freeze vuelca esa RAM a
	// mem.file; si esta máquina se promociona luego a snapshot dorado (o ya lo es
	// una copia suya), ese mem.file se COMPARTE con todas las instancias, y el
	// secreto de una sesión acabaría legible por las demás. Es justo lo que el
	// modelo MMDS existe para impedir, así que preferimos fallar claro a filtrar.
	// Quien quiera liberar RAM de una máquina con secretos tiene `squeeze` (no
	// vuelca nada a disco) o `stop`/`rm`.
	if mc.HasSecrets {
		return nil, fmt.Errorf("machine %s has session secrets injected via MMDS and "+
			"cannot be frozen: the RAM dump would end up in mem.file, which is shared if it is or "+
			"becomes a golden snapshot. Use squeeze (does not dump to disk) or stop/rm; or, if its image "+
			"has post-restore hooks that consume the secret, run them (kling machine hooks -wait) and "+
			"empty the store (echo '{}' | kling machine secret), which lifts the mark", mc.ID[:12])
	}

	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}
	// Antes de pausar nada: si el volcado no cabe, se dice ahora y la máquina
	// sigue corriendo como si nada (ver reservarDiscoParaVolcado).
	// Diferencial (diff_volcado.go): una copia con seguimiento de páginas
	// sucias vuelca solo lo escrito desde el dorado. Lo que ocupa no se sabe
	// hasta volcarlo; un cuarto de la RAM es más de lo medido (~100 MiB de 3
	// GiB) y mucho menos que exigir la RAM entera a cada copia dormida.
	enDiff := mc.DiffBase != "" && congelarEnDiff() && existe(mc.DiffBase)
	if enDiff && !huecosFiables(filepath.Dir(m.dir(mc.ID))) {
		// Sin huecos fiables un diff no se puede aplicar bien (huecos_fiables.go).
		log.Printf("warning: %s: %s does not tell holes from zero pages; freezing the whole memory",
			mc.Name, filepath.Dir(m.dir(mc.ID)))
		m.olvidarDiffBase(mc.ID)
		enDiff = false
	}
	necesario := max(mc.MemMiB, mc.MemMaxMiB)
	if enDiff {
		necesario /= 4
	}
	soltarDisco, err := m.reservarDiscoParaVolcado(necesario, "freeze")
	if err != nil {
		return nil, err
	}
	defer soltarDisco()
	defer m.marcarTransicion(mc.ID, api.TransitionFreezing)()

	dir := m.dir(mc.ID)
	// El diferencial se vuelca aparte (mem.diff) y se funde después sobre el
	// mem.file que ya hubiera; el completo es el mem.file directamente.
	memName := "mem.file"
	diffAlmacen := ""
	if enDiff {
		memName = memDiff
		// Si puede, el diff va al almacén (cow_memoria.go): ahí despertar
		// es clonar sus extents sobre el espejo del dorado, no copiarlos.
		diffAlmacen = m.diffEnAlmacen(ctx, mc.ID, max(mc.MemMiB, mc.MemMaxMiB))
		if diffAlmacen != "" && !huecosFiables(m.alm.dir) {
			m.borrarDiffParcialAlmacen(mc.ID)
			diffAlmacen = ""
		}
		if diffAlmacen == "" {
			if fi, err := os.Lstat(filepath.Join(dir, "mem.file")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
				// El acumulado vive en el almacén pero el almacén ya no
				// está en uso: no se mezclan los dos sitios. Entero, y la
				// próxima vez diferencial desde cero.
				m.borrarAcumuladoDiff(mc.ID, dir)
				m.olvidarDiffBase(mc.ID)
				enDiff, memName = false, "mem.file"
			}
		}
	}
	if !enDiff {
		// Un volcado completo pisa machines/<id>/mem.file: lo que hubiera
		// acumulado de diferenciales anteriores (y su enlace al almacén, que
		// mandaría la RAM entera dentro del almacén) sobra.
		m.borrarAcumuladoDiff(mc.ID, dir)
	}
	snapPath, memPath := filepath.Join(dir, "snap.file"), filepath.Join(dir, memName)
	if diffAlmacen != "" {
		memPath = diffAlmacen
	}
	if enDiff && diffAlmacen == "" {
		// Un mem.diff viejo (un crash a mitad de otro freeze) no se reutiliza:
		// Firecracker escribe encima solo las páginas sucias y las demás
		// serían de entonces.
		_ = os.Remove(filepath.Join(dir, memDiff))
	}

	// Una instancia jailed corre chrooteada: no puede escribir en el dir del
	// host. Se le pide el volcado en la raíz de su chroot y luego se recupera al
	// dir real. Mismo filesystem, así que el traslado es un rename atómico y el
	// mem.file conserva su inodo —y su caché de páginas—. El diff del almacén
	// no: su ruta absoluta resuelve dentro del chroot al bind del directorio
	// de la instancia (prepararBindsJail).
	jailed := m.jailerJailed && strings.HasPrefix(sock, m.jailRoot(mc.ID))
	if jailed {
		snapPath = "/snap.file"
		if diffAlmacen == "" {
			memPath = "/" + memName
		}
	}

	c := fc.New(sock)

	// El vaciado va ANTES de pausar, y no dentro de kill() como en los demás
	// caminos. Un invitado pausado no atiende HTTP: la petición se comía su
	// plazo entero en CADA congelación, y lo que quedara sin vaciar solo vivía
	// en mem.file — si luego se elimina la máquina warm, esas escrituras
	// desaparecen sin dejar rastro.
	m.flushVolume(mc)

	// Las carpetas vivas se desconectan ANTES de pausar: la conexión muere con
	// el VMM, y si no se cortara aquí el daemon no se enteraría hasta que
	// venciera el keepalive, con la sesión colgada. En orden (drainSharesOf):
	// lo que el invitado tenga en vuelo contesta y lo nuevo lo repite al
	// descongelar, en vez de EIO. hold impide que el vigilante las reconecte
	// mientras dura el volcado; al salir, si la máquina sigue corriendo (el
	// volcado falló), releaseShares las relanza.
	if hasLiveShares(mc) {
		m.holdShares(mc.ID)
		defer m.releaseShares(mc.ID)
		m.drainSharesOf(mc.ID)
	}

	// ¿Hay agente al que resincronizar al descongelarla? Se pregunta ahora,
	// con el invitado en marcha, porque tras restaurar un invitado sin nadie en
	// el puerto puede tardar segundos en contestar (ver resyncSinAgenteTTL).
	sinAgente := !m.agenteEscucha(ctx, mc.ID)

	// Con el invitado aún en marcha: que suelte lo que no usa, y el volcado
	// lleve solo lo que está en uso (apreton_volcado.go).
	// No antes de un diferencial: las páginas que el invitado soltara se
	// vuelcan igual (están sucias) y costaría hasta 2 s y su caché.
	apretado := 0
	if !enDiff {
		apretado = apretarAntesDeVolcar(ctx, c, mc)
	}

	// Desde aquí, lo que haya en disco deja de valer hasta el sello final: si el
	// daemon muere a mitad del volcado, reconcile y Thaw lo sabrán (volcado.go).
	if err := volcadoEnCurso(dir); err != nil {
		return nil, fmt.Errorf("marking the freeze as in progress: %w", err)
	}

	start := time.Now()
	if err := c.Pause(ctx); err != nil {
		return nil, err
	}
	// Con plazo propio: el de 30 s del cliente no alcanza para volcar varios
	// GiB, y cortarlo no para a Firecracker (F-01, ver plazoVolcado).
	lento := c.ConPlazo(plazoVolcado(max(mc.MemMiB, mc.MemMaxMiB)))
	var verr error
	if enDiff {
		verr = lento.SnapshotDiff(ctx, snapPath, memPath)
	} else {
		verr = lento.Snapshot(ctx, snapPath, memPath)
	}
	if err := verr; err != nil {
		// Lo que Firecracker llegó a escribir no vale: un mem.file a medias
		// (típico: se acabó el disco) del tamaño de la RAM que nadie borraría,
		// porque la máquina sigue running y reconcile solo mira las warm.
		m.borrarVolcadoParcial(mc.ID, jailed, dir)
		m.borrarDiffParcialAlmacen(mc.ID)
		// Y un diff a medias pudo dejar a cero el mapa de sucias: lo que se
		// escriba desde aquí ya no se distinguiría. El siguiente vuelca entero,
		// así que el acumulado de antes tampoco vale.
		m.borrarAcumuladoDiff(mc.ID, dir)
		m.olvidarDiffBase(mc.ID)
		// Reanudar antes de rendirse. Sin esto la máquina se quedaba PAUSADA
		// para siempre figurando como running: el vigilante no la detecta
		// porque el proceso vive, el gateway le sigue enrutando peticiones, y
		// ninguna responde jamás.
		//
		// Y hay que devolverle sus volúmenes, que se soltaron arriba para poder
		// congelar: si no, seguiría corriendo escribiendo en su overlay.
		if rerr := c.Resume(context.WithoutCancel(ctx)); rerr != nil {
			// Si tampoco se puede reanudar, la máquina no es recuperable y
			// dejarla como running sería mentir. Se marca fallida.
			m.fail(mc, fmt.Errorf("freeze failed (%v) and could not resume it either: %w", err, rerr))
			return nil, err
		}
		_ = m.acquireVolumes(mc)
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()

	if jailed {
		// Recuperar el volcado del chroot al dir del host: es donde Thaw y
		// reconcile lo buscan. Rename dentro del mismo filesystem.
		// Sin seguir enlaces, y solo si lo que hay es el fichero que escribió
		// el VMM: un enlace o un hardlink plantado en su chroot llevaría al
		// daemon a perforar, precargar y ceder un fichero del host (ver
		// recuperarDelJail).
		recuperar := []string{"snap.file"}
		if diffAlmacen == "" {
			recuperar = append(recuperar, memName)
		}
		if err := recuperarDelJail(m.jailRoot(mc.ID), "/", dir, m.uidJail(), recuperar...); err != nil {
			// La máquina sigue PAUSADA: devolver el error sin más la dejaba
			// figurando como running, sin contestar a nada, y sin que el
			// vigilante la viera, porque el proceso existe. Mismo trato que
			// un fallo del propio snapshot, más arriba.
			m.borrarVolcadoParcial(mc.ID, jailed, dir)
			m.borrarDiffParcialAlmacen(mc.ID)
			m.borrarAcumuladoDiff(mc.ID, dir)
			m.olvidarDiffBase(mc.ID)
			if rerr := c.Resume(context.WithoutCancel(ctx)); rerr != nil {
				m.fail(mc, fmt.Errorf("freeze failed (%v) and could not resume it either: %w", err, rerr))
				return nil, err
			}
			_ = m.acquireVolumes(mc)
			return nil, err
		}
		snapPath = filepath.Join(dir, "snap.file")
		if diffAlmacen == "" {
			memPath = filepath.Join(dir, memName)
		}
	}
	if enDiff {
		// Todo lo escrito desde el dorado, en un solo mem.file
		// (diff_volcado.go). Si no se puede, la máquina sigue como estaba —
		// pausada, con su VMM— y se reanuda como tras un volcado fallido; su
		// siguiente freeze vuelca entero, porque el mapa de sucias ya se
		// reinició con este diff.
		acum := filepath.Join(dir, "mem.file")
		if diffAlmacen != "" {
			acum = m.alm.acumuladoDiff(mc.ID)
		}
		if err := fusionarDiff(ctx, dir, memPath, acum); err != nil {
			m.borrarVolcadoParcial(mc.ID, false, dir)
			m.borrarDiffParcialAlmacen(mc.ID)
			m.borrarAcumuladoDiff(mc.ID, dir)
			m.olvidarDiffBase(mc.ID)
			if rerr := c.Resume(context.WithoutCancel(ctx)); rerr != nil {
				m.fail(mc, fmt.Errorf("freeze failed (%v) and could not resume it either: %w", err, rerr))
				return nil, err
			}
			_ = m.acquireVolumes(mc)
			return nil, err
		}
		memPath = filepath.Join(dir, "mem.file")
	}

	// Con el snapshot en disco el proceso sobra: aquí es donde se libera la RAM.
	//
	// killPaused y no kill: el invitado lleva pausado desde antes del snapshot y
	// no puede contestar. Los volúmenes ya se vaciaron arriba, con la máquina
	// aún corriendo, que era el único momento posible.
	m.killPaused(mc.ID)
	// Si despertó de un diferencial, el VMM mapeaba mem.full (base + diff):
	// muerto el VMM no lo lee nadie. Un freeze diferencial ya lo retiró al
	// fundir; uno completo (tras un commit, con el diff apagado o sin base) lo
	// dejaría ahí, y son GiB por copia dormida en ext4.
	_ = os.Remove(filepath.Join(dir, memFull))
	m.borrarMemoriaAlmacen(mc.ID)
	// El chroot del jail ya no sirve: se borra aquí, en segundo plano del
	// despertar, y no al principio del siguiente thaw (3,3 ms medidos ahí).
	if jailed {
		if err := m.borrarJail(mc.ID); err != nil {
			log.Printf("warning: %v", err)
		}
	}
	// La red se queda montada: Thaw la reutiliza (ver red.go), y el vigilante
	// la desmonta si la máquina pasa mucho tiempo congelada. El cgroup no hace
	// nada sin proceso; Thaw lo recrea.
	m.releaseCPU(mc.ID)

	// Firecracker vuelca la memoria entera, pero en una microVM recién arrancada
	// la mayor parte son páginas a cero. Perforarlas deja el fichero disperso: el
	// kernel devuelve ceros al leer un agujero, que es exactamente lo que había,
	// así que la restauración no se entera. Mide ~3x menos en disco.
	//
	// Un diferencial NO se perfora: sus ceros son páginas que el invitado puso
	// a cero, y un hueco diría "como en el dorado" (diff_volcado.go).
	if !enDiff {
		if out, err := perforarHuecos(ctx, memPath); err != nil {
			log.Printf("warning: could not punch holes in %s: %v: %s", memPath, err, out)
		}
	}

	// El fichero de memoria queda entero en caché tras escribirlo y releerlo para
	// perforarlo, y no se volverá a tocar hasta que alguien descongele ESTA
	// máquina. Se suelta: es la principal fuente de caché acumulada del host.
	//
	// Salvo si es pequeño: entonces se deja ENTERO en la caché mientras dure
	// la red conservada (redDormidaMax; soltarRedesDormidas lo suelta con
	// ella), y el thaw no lee nada del disco. Hay que leerlo aquí: tras el
	// volcado y el perforado casi nada queda en caché (medido: 832 KiB de 71
	// MiB). Y con la red conservada no queda tiempo para que la precarga del
	// thaw termine antes de que el invitado despierte (medido: 19 ms de resync
	// con la caché fría frente a 5 ms con ella caliente). Esto va en el
	// congelado, que corre en segundo plano, y no en el despertar.
	if allocatedBytes(memPath) > cacheTibiaMaxBytes {
		dropCache(memPath)
	} else {
		precargar(memPath)
	}

	size := allocatedBytes(memPath) + allocatedBytes(snapPath)

	// El sello va cuando los ficheros están completos y en su sitio (tras
	// sacarlos de la jaula y perforarlos): es lo que dice que este volcado vale.
	// Con el kernel instalado (K2): el Thaw lo compara, igual que runFrom con
	// el de un dorado. Sin él (no se pudo hashear), el sello vale igual.
	kernelSHA, kerr := m.kernelHash()
	if kerr != nil {
		log.Printf("warning: %s: could not hash the kernel for the seal: %v", mc.Name, kerr)
	}
	cerrarVolcado(dir)
	base := ""
	if enDiff {
		base = mc.DiffBase
	}
	if err := sellarVolcado(dir, kernelSHA, m.origenActual().vmm, base); err != nil {
		log.Printf("warning: %s: could not seal the frozen state: %v", mc.Name, err)
	}

	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		// La eliminaron mientras congelabamos. Los ficheros del snapshot estan
		// escritos, pero ya no hay maquina a la que pertenezcan. Antes esto era
		// un deref nil, y un deref nil en el daemon no se lleva esta operacion:
		// se lleva el proceso, y con el TODAS las microVM quedan huerfanas.
		delete(m.socket, mc.ID)
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %q was removed while it was being frozen", mc.Name)
	}
	now := time.Now()
	live.State = api.StateWarm
	live.FrozenAt = &now
	live.FreezeMS = elapsed
	live.SnapSize = size
	live.PID = 0
	// Los reenvíos (macOS) eran del proceso que acaba de morir; los de la
	// próxima descongelación serán otros.
	live.Forwards = nil
	delete(m.socket, mc.ID)
	m.persist()
	out := *live
	m.mu.Unlock()
	// Una copia de kling db congelada no atiende: se cortan las sesiones de
	// los agentes que llegaban a ella por su proxy (copias_db.go).
	m.invalidarSesiones(mc.ID, "frozen")

	out.DiskBytes = m.touchDisk(mc.ID)
	if sinAgente {
		m.resyncSinAgente.Store(claveThaw(mc.ID), time.Time{})
	} else {
		m.resyncSinAgente.Delete(claveThaw(mc.ID))
	}

	msg := fmt.Sprintf("frozen in %d ms (%d MiB on disk)", elapsed, size>>20)
	if apretado > 0 {
		msg = fmt.Sprintf("frozen in %d ms (%d MiB on disk; the guest handed back ~%d MiB before the dump)", elapsed, size>>20, apretado)
	}
	m.bus.Publish(api.Event{Time: now, Type: api.EvFrozen, ID: mc.ID, Name: mc.Name, Message: msg})
	return &out, nil
}

// balloonStatsPollSec es cada cuánto reporta el globo sus estadísticas. Hace
// falta >0 para poder leer la memoria libre del invitado antes de apretarlo; 1 s
// es barato (el hilo vive dentro del invitado) y suficientemente fresco.
const balloonStatsPollSec = 1

// balloonSqueezeMarginMiB es el colchón que se le deja al invitado al apretar:
// se reclama su memoria libre MENOS este margen, para no dejarlo pegado al borde
// del OOM justo después.
const balloonSqueezeMarginMiB = 128

// squeezeMinRetenerMiB: por debajo de esto, en macOS, el apretón no ha devuelto
// nada que valga la pena retener y el globo vuelve a la línea base.
const squeezeMinRetenerMiB = 16

// Squeeze aprieta el globo de una instancia running para devolver al host la RAM
// que el invitado tiene LIBRE, sin congelarla.
//
// Es el estado intermedio que faltaba entre "running" (faultea su mem_mib entero
// y node nunca lo devuelve al SO, de ahí la densidad 3-8 bajo carga) y "warm"
// (congelada; descongelar cuesta más que un simple globo). Infla el globo hasta
// dejar al invitado con un margen mínimo, espera a que su driver entregue las
// páginas —Firecracker las suelta con madvise(DONTNEED), y ahí cae el RSS del
// host—, y desinfla a 0 para que el invitado pueda volver a crecer: la RAM ya
// está reclamada y solo reentra si de verdad se necesita.
func (m *Manager) Squeeze(ctx context.Context, ref string) (*api.SqueezeResult, error) {
	return m.SqueezeWith(ctx, ref, false)
}

// ErrSqueezeShared es un squeeze que se niega porque la máquina comparte su
// RAM con otras copias del mismo dorado (Machine.MemShared).
var ErrSqueezeShared = errors.New("squeezing a copy that shares memory with its template")

// SqueezeWith es Squeeze; force aprieta aunque la máquina comparta memoria con
// un dorado (ver squeezeLocked).
func (m *Manager) SqueezeWith(ctx context.Context, ref string, force bool) (*api.SqueezeResult, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()
	return m.squeezeLocked(ctx, mc.ID, ref, force)
}

// squeezeLocked es el apretón propiamente dicho, con el cerrojo de la máquina ya
// tomado por quien llama (Squeeze espera por él; makeRoom lo intenta y se salta
// las ocupadas).
func (m *Manager) squeezeLocked(ctx context.Context, id, ref string, force bool) (*api.SqueezeResult, error) {
	// Pudo cambiar de estado mientras esperábamos el lock.
	cur, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	if cur.State != api.StateRunning {
		return nil, fmt.Errorf("only a running machine can be squeezed (it is %s)", cur.State)
	}
	// Una copia de un dorado en Firecracker mapea su mem.file MAP_PRIVATE: la
	// caché de páginas del invitado son páginas LIMPIAS y COMPARTIDAS con las
	// demás copias. Al inflar el globo el invitado las suelta, y en cuanto
	// vuelve a leer esos ficheros los trae del disco virtual a páginas
	// PRIVADAS. Cada copia "devuelve" algo y el total sube: medido con 24
	// teléfonos Android, Σ PSS de 4533 a 5072 MiB, la swap llena y el host
	// colgado (prototypes/android/docs/proxmox.md).
	if cur.MemShared && !force {
		return nil, fmt.Errorf("%w: %s was restored from template %s and shares its memory pages with the "+
			"other copies; the balloon would make its guest drop shared page cache and read it back as "+
			"private memory, so the host would end up using MORE memory, not less. "+
			"Freeze it instead, or use -force if it is the only copy", ErrSqueezeShared, cur.Name, cur.From)
	}

	m.mu.RLock()
	sock := m.socket[id]
	pid := cur.PID
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", id)
	}
	c := fc.New(sock)

	stats, err := c.BalloonStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("the balloon is not available on this instance "+
			"(reimport the image to record it in the snapshot): %w", err)
	}
	freeMiB := int(stats.FreeMemory >> 20)
	rssBefore := rssVMM(pid, sock)

	// Se reclama la memoria DISPONIBLE, no solo la LIBRE. Al inflar el globo, el
	// invitado suelta también su caché de página limpia para cedérnosla, así que
	// esas páginas vuelven al host igual que las libres. AvailableMemory es la
	// estimación del kernel invitado de lo reclamable sin entrar en swap; el
	// colchón y el deflate_on_oom configurado en boot cubren que se quede corta.
	// Si el invitado no reporta AvailableMemory (0 en guests que no exponen ese
	// stat), se cae a la memoria libre, que siempre es segura de reclamar.
	reclaimMiB := freeMiB
	if avail := int(stats.AvailableMemory >> 20); avail > reclaimMiB {
		reclaimMiB = avail
	}

	target := stats.ActualMiB + reclaimMiB - balloonSqueezeMarginMiB
	sinEstadisticas := globoSinEstadisticas && estadisticasDesconocidas(stats)
	if sinEstadisticas {
		// macOS: el framework no dice cuánta memoria tiene libre el invitado
		// (los tres campos llegan a 0), así que la cuenta de arriba no reclama
		// nada. Se pregunta al agente (GET /meminfo) y, si no lo sabe, se
		// aprieta hasta un suelo fijo; ver objetivoSinEstadisticas.
		mi, err := m.memoriaInvitado(ctx, id)
		if err == nil {
			target = objetivoConMeminfo(cur, stats.ActualMiB, mi)
			freeMiB = mi.AvailableMiB
		} else {
			target = objetivoSinEstadisticas(cur)
		}
	}
	if target <= stats.ActualMiB {
		// El invitado no tiene holgura que reclamar.
		return &api.SqueezeResult{ID: id, ReclaimedMiB: 0, GuestFreeMiB: freeMiB, RSSMiB: rssBefore}, nil
	}

	if err := c.PatchBalloon(ctx, target); err != nil {
		return nil, fmt.Errorf("inflating the balloon: %w", err)
	}
	// El inflado es asíncrono: el driver del invitado va entregando páginas.
	// Esperamos a que se acerque al objetivo (o a un plazo corto) antes de medir.
	waitBalloon(ctx, c, target)
	if sinEstadisticas {
		// Sin estadísticas, actual_mib es lo pedido y waitBalloon vuelve al
		// instante; lo que dice cuándo ha soltado el invitado es la huella del
		// VMM en el host, que deja de bajar.
		esperarHuellaEstable(ctx, pid, sock)
	}
	rssAfter := rssVMM(pid, sock)
	reclaimed := rssBefore - rssAfter
	if reclaimed < 0 {
		reclaimed = 0
	}

	// Desinflar: la RAM ya se reclamó al inflar; esto solo devuelve el presupuesto
	// al invitado. Con contexto sin cancelar para que no se quede inflado si el
	// cliente abandonó.
	// A la línea base, no a 0: en una máquina con techo el globo retiene la
	// diferencia entre el techo y su memoria, y desinflarlo del todo le daría
	// el techo entero.
	//
	// En macOS NO se desinfla si el apretón devolvió algo: al desinflar,
	// Virtualization.framework vuelve a poblar las páginas y la huella regresa
	// entera en ~3 s (medido: 1123 -> 620 -> 1132 MiB), así que "apretar y
	// soltar" no deja nada. El globo se queda inflado; deflate_on_oom (que se
	// configura al arrancar) deja al invitado recuperarlo si de verdad lo
	// necesita, y `kling resize` o el siguiente squeeze lo recolocan. Si no
	// devolvió nada (una máquina arrancada en frío: ahí el framework no suelta
	// las páginas del globo) retenerlo solo le quitaría memoria al invitado.
	if !sinEstadisticas || reclaimed < squeezeMinRetenerMiB {
		if err := c.PatchBalloon(context.WithoutCancel(ctx), globoBase(cur)); err != nil {
			log.Printf("warning: could not deflate the balloon for %s: %v", id, err)
		}
	}

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvFrozen, ID: id, Name: cur.Name,
		Message: fmt.Sprintf("squeezed: ~%d MiB returned to host (RSS %d→%d MiB)", reclaimed, rssBefore, rssAfter)})

	return &api.SqueezeResult{ID: id, ReclaimedMiB: reclaimed, GuestFreeMiB: freeMiB, RSSMiB: rssAfter}, nil
}

// waitBalloon espera a que el globo alcance ~el objetivo. El inflado lo hace el
// driver del invitado a su ritmo; sin esta espera mediríamos el RSS antes de que
// soltara nada. Plazo corto: si el invitado no coopera, no bloqueamos.
func waitBalloon(ctx context.Context, c *fc.Client, targetMiB int) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s, err := c.BalloonStats(ctx)
		if err == nil && s.ActualMiB >= targetMiB-16 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// procRSSMiB lee el RSS del proceso (VmRSS de /proc/<pid>/status) en MiB. Es la
// cifra del host que cae cuando el globo devuelve páginas. Devuelve 0 si no se
// puede leer (macOS de desarrollo, o proceso ya muerto).
func procRSSMiB(pid int) int {
	if pid <= 0 {
		return 0
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "VmRSS:")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			return 0
		}
		kb, err := strconv.Atoi(f[0])
		if err != nil {
			return 0
		}
		return kb / 1024
	}
	return 0
}

// PutMMDS inyecta el store MMDS en una microVM VIVA: es la entrega de secretos
// (un token, una credencial) a una instancia ya arrancada, sin haberlos
// horneado en la imagen compartida ni en el snapshot dorado.
//
// Resuelve la máquina, coge su socket de Firecracker —igual que Squeeze— y hace
// PUT /mmds con el documento JSON tal cual. El esquema del store lo entiende el
// bridge de dentro: un objeto con "env", comunes a TODAS las sesiones de la
// máquina ("sessions" se retiró: ver pkg/guest/mmds.go).
//
// Al inyectar marca la máquina con HasSecrets: a partir de aquí Freeze se niega a
// congelarla, para que el secreto no acabe en un mem.file compartido.
func (m *Manager) PutMMDS(ctx context.Context, ref string, data any) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()

	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	if cur.State != api.StateRunning {
		return nil, fmt.Errorf("MMDS can only be injected into a running machine (it is %s)", cur.State)
	}

	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}

	// El PUT sustituye el almacén ENTERO, y en él están los marcadores de sus
	// credenciales (ponerMarcadoresMMDS): sin volver a ponerlos, el invitado
	// se quedaba sin la variable que el proxy sabe cambiar por la clave. Van
	// en el mismo documento, no en un PATCH después: así no hay un instante
	// en que una sesión nueva lea el almacén sin ellos.
	vacio := almacenVacio(data)
	doc, err := m.conMarcadores(mc.ID, data)
	if err != nil {
		return nil, err
	}
	if doc, err = m.conEntornoPendiente(mc.ID, doc); err != nil {
		return nil, err
	}
	// Con los marcadores y el entorno pendiente puede pasarse del almacén
	// del VMM: se dice aquí, con el tope, y no con el error crudo del VMM.
	if b, err := json.Marshal(doc); err != nil {
		return nil, err
	} else if len(b) > api.MaxMMDSBytes {
		return nil, fmt.Errorf("the MMDS store would be %d bytes with the machine's credential placeholders, "+
			"over the limit of %d", len(b), api.MaxMMDSBytes)
	}
	c := fc.New(sock)
	if err := c.PutMMDSData(ctx, doc); err != nil {
		return nil, fmt.Errorf("injecting MMDS: %w", err)
	}

	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %q no longer exists", ref)
	}
	if m.secretos == nil {
		m.secretos = map[string]*estadoSecreto{}
	}
	st := m.secretos[mc.ID]
	mensaje := "session secrets injected via MMDS (can no longer be frozen)"
	levantada := false
	switch {
	case !vacio:
		if st == nil {
			st = &estadoSecreto{}
			m.secretos[mc.ID] = st
		}
		st.gen++
		live.HasSecrets = true
	case !live.HasSecrets:
		mensaje = "MMDS store emptied"
	case st.confirmado(): // st != nil: lo garantiza confirmado()
		live.HasSecrets = false
		delete(m.secretos, mc.ID)
		levantada = true
		mensaje = "MMDS store emptied after its post-restore hooks consumed the secret: it can be frozen again"
	default:
		mensaje = "MMDS store emptied, but still marked with secrets: no post-restore hook confirmed it " +
			"consumed them (kling machine hooks -wait) after the last injection"
	}
	m.persist()
	out := *live
	m.mu.Unlock()

	if levantada {
		log.Printf("%s: %s", mc.Name, mensaje)
	}
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStarted, ID: mc.ID, Name: mc.Name, Message: mensaje})
	return &out, nil
}

// LEVANTAR LA MARCA DE SECRETOS.
//
// Una máquina que recibió un secreto por MMDS no se congela (HasSecrets): la
// RAM del invitado lo lleva, y un volcado lo repartiría. Pero el caso de la
// identidad por copia (un teléfono: android_id y nombre por MMDS, que un
// gancho aplica) dejaba la máquina marcada para siempre aunque el almacén se
// vaciara, y ya no se podía ni congelar para recuperarla.
//
// Nadie puede demostrar desde fuera que la RAM del invitado ya no contiene el
// secreto. Lo que el daemon SÍ sabe es esto, y es lo que se exige:
//
//  1. después de la ÚLTIMA inyección no vacía corrió una tanda de ganchos de
//     la imagen (POST /machines/{ref}/hooks) y terminó con éxito, sin otra
//     inyección en medio;
//  2. y el almacén se vació después (PUT con {} o null).
//
// La garantía es la de la imagen: su gancho declara, al salir con 0, que
// aplicó el secreto y no dejó copia (en un Android, la identidad aplicada ES
// del teléfono y no es secreto; lo que no debe quedar es el documento). Una
// imagen sin ganchos, o un servidor MCP que guardó el secreto en el entorno de
// sus procesos, no confirma nada y la marca se queda. No sobrevive a reiniciar
// el daemon: sin el registro, la marca también se queda.

// estadoSecreto es lo que el daemon sabe de los secretos de una máquina.
type estadoSecreto struct {
	gen        uint64 // inyecciones no vacías
	consumidos uint64 // la gen que una tanda de ganchos confirmó
}

func (e *estadoSecreto) confirmado() bool {
	return e != nil && e.gen > 0 && e.consumidos == e.gen
}

// almacenVacio: {} o null (con espacios), que es como se vacía el almacén.
func almacenVacio(data any) bool {
	b, err := json.Marshal(data)
	if err != nil {
		return false
	}
	switch strings.Join(strings.Fields(string(b)), "") {
	case "{}", "null":
		return true
	}
	return false
}

// genSecreto es la inyección vigente de la máquina id (0 = ninguna).
func (m *Manager) genSecreto(id string) uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if st := m.secretos[id]; st != nil {
		return st.gen
	}
	return 0
}

// confirmarSecreto apunta que los ganchos terminaron bien tras la inyección
// gen, si no hubo otra entre medias.
func (m *Manager) confirmarSecreto(id string, gen uint64) {
	if gen == 0 {
		return
	}
	m.mu.Lock()
	if st := m.secretos[id]; st != nil && st.gen == gen {
		st.consumidos = gen
	}
	m.mu.Unlock()
}

// SetCredentials entrega credenciales al proxy de credenciales de una máquina
// viva (pkg/credproxy, servido por internal/net en Linux y por su kling-vz en
// macOS). La clave real se queda en el host —en memoria del daemon (o de
// kling-vz) y cifrada en el directorio de la máquina (ver credenciales.go)—; al
// invitado le llega por MMDS (env) un marcador en la variable que pide cada
// credencial, y el proxy lo cambia por la clave solo en peticiones a su
// dominio. Así un servidor comprometido no puede leer la clave, sacarla a otro
// dominio ni dejarla en un snapshot.
//
// Exige egress allowlist: el desvío del dominio al proxy lo hace el resolver
// propio de la máquina, que solo existe en ese modo.
//
// Se FUSIONA con lo que la máquina ya tuviera, por variable: repetir la misma
// -env con otra clave la rota sin que el proceso del invitado se entere, porque
// conserva su marcador (el entorno de un proceso no cambia después de exec).
//
// A diferencia de PutMMDS, NO marca HasSecrets: lo que hay en la RAM del
// invitado es el marcador, y el marcador no es un secreto (ver la documentación de
// pkg/credproxy). La máquina se puede congelar, y al descongelarla Thaw vuelve
// a entregar las credenciales desde su almacén.
func (m *Manager) SetCredentials(ctx context.Context, ref string, specs []api.CredentialSpec) (*api.Machine, error) {
	if len(specs) == 0 {
		return nil, errors.New("no credentials given")
	}
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()

	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	if cur.State != api.StateRunning {
		return nil, fmt.Errorf("credentials can only be given to a running machine (it is %s)", cur.State)
	}
	if cur.Egress != string(knet.EgressAllowlist) {
		return nil, fmt.Errorf("credentials need -egress allowlist (the machine has %q)", cur.Egress)
	}
	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}

	creds, nuevas, err := m.entregarCredenciales(ctx, mc.ID, knet.Plan(cur.NetIndex, cur.ID), fc.New(sock), specs)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %q no longer exists", ref)
	}
	live.CredentialDomains = dominiosDe(creds)
	live.CredentialAnyDatabase = anyDatabaseDe(creds)
	m.persist()
	out := live.Clone()
	m.mu.Unlock()

	dominios := make([]string, 0, len(specs))
	for _, s := range specs {
		dominios = append(dominios, s.Domain)
	}
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStarted, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("credentials handed to the credential proxy for %s (%d new, %d rotated; the guest only sees placeholders)",
			strings.Join(dominios, ", "), nuevas, len(specs)-nuevas)})
	return out, nil
}

// Thaw restaura una máquina warm. Es la operación rápida del proyecto.
func (m *Manager) Thaw(ctx context.Context, ref string) (*api.Machine, error) {
	crono := nuevoCrono("frozen")
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()
	crono.marca(&crono.p.WaitMS)

	// Otra llamada pudo descongelarla mientras esperábamos el candado, o
	// borrarla: seguir con la copia de antes lanzaba un VMM sobre un
	// directorio que ya no existe (como Freeze y Commit, se relee).
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	if cur.State == api.StateRunning {
		return cur, nil
	}
	// Pausada: solo reanudar (ver pausa.go).
	if cur.State == api.StatePaused {
		crono.p.Tier = "paused"
		// Pausada por el almacén lleno (cow_vigilante.go): reanudarla sin
		// sitio la devolvería a escribir en un almacén que da EIO.
		if err := m.comprobarAlmacenPara(mc.ID); err != nil {
			return nil, fmt.Errorf("machine %q can't be resumed: %w", cur.Name, err)
		}
		out, err := m.reanudarLocked(ctx, cur, crono)
		if err != nil {
			return nil, err
		}
		fases := crono.cerrar()
		out.Wake = fases
		m.mu.Lock()
		if l := m.byID[mc.ID]; l != nil {
			l.Wake = fases
		}
		m.mu.Unlock()
		m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvThawed, ID: mc.ID, Name: mc.Name,
			Message: "resumed" + notaFases(fases)})
		return out, nil
	}
	mc = cur
	if mc.State != api.StateWarm {
		return nil, fmt.Errorf("only a warm or paused machine can be thawed (it is %s)", mc.State)
	}
	// Su disco vive en el almacén y no queda sitio: se dice ahora, antes de
	// que el invitado lo descubra con un EIO al escribir.
	if err := m.comprobarAlmacenPara(mc.ID); err != nil {
		return nil, fmt.Errorf("machine %q can't be thawed: %w", mc.Name, err)
	}

	dir := m.dir(mc.ID)
	snapPath, memPath := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
	sock := filepath.Join(dir, "fc.sock")

	// Antes de borrar el socket y arrancar: ¿hay ya un firecracker vivo que sea
	// de ESTA máquina?
	//
	// Con el estado obsoleto —un thaw anterior cuya escritura no llegó a disco,
	// por ejemplo— la máquina figura warm mientras corre de verdad. Seguir
	// adelante arrancaría un SEGUNDO firecracker sobre el mismo overlay.ext4:
	// dos VMMs escribiendo el mismo sistema de ficheros es corrupción, y del
	// tipo que no se nota hasta mucho después.
	live := m.liveVMs()
	crono.marca(&crono.p.CheckMS)
	if live[mc.ID] > 0 {
		pid := live[mc.ID]
		log.Printf("thaw: %s (%s) was already running (pid %d); re-adopting it instead of starting another",
			mc.Name, mc.ID[:8], pid)
		// Con su socket real: si corre en jail, el del chroot (ver socketDe).
		sock := m.socketDe(mc.ID, pid)
		// El techo de CPU, el configurado (no el impulso de arranque: este
		// invitado ya arrancó). Su cgroup pudo irse con un reinicio del daemon
		// (sweepCgroups) o no haberse aplicado nunca si el thaw que lo lanzó
		// murió antes de llegar a limitCPU: sin esto corría sin techo.
		techo := mc.CPUPct
		if techo <= 0 {
			techo = techoDelDaemon(mc.VCPUs)
		}
		if warn := m.limitCPU(mc.ID, pid, techo); warn != "" {
			log.Printf("warning: %s: %s", mc.Name, warn)
		}
		m.mu.Lock()
		cur := m.byID[mc.ID]
		if cur == nil {
			// Con el cerrojo tomado nadie debería poder borrarla; si aun así
			// no está, seguir lanzaría un SEGUNDO VMM sobre su overlay, que es
			// justo lo que esta readopción evita. El que corre no es de nadie.
			m.mu.Unlock()
			_ = syscall.Kill(pid, syscall.SIGKILL)
			return nil, fmt.Errorf("machine %q was removed while it was being thawed", mc.Name)
		}
		now := time.Now()
		cur.State = api.StateRunning
		cur.PID = pid
		cur.StartedAt = &now
		cur.FrozenAt = nil
		cur.CPUPct = techo
		m.socket[mc.ID] = sock
		m.persist()
		out := *cur
		m.mu.Unlock()
		m.startShares(mc.ID)
		return &out, nil
	}

	// Jailer bloqueado: antes de enterLaunch, la red y el cgroup (ver Run). No
	// antes: reanudar una pausada o readoptar un VMM que ya corre no lanza
	// ningún firecracker nuevo, y las máquinas vivas siguen funcionando.
	if m.JailerBlocked != "" {
		return nil, errors.New(m.JailerBlocked)
	}
	// Un volcado a medias no se carga: fallaría con un error de Firecracker que
	// no señala a ninguna parte, o peor, arrancaría un invitado corrupto.
	if err := volcadoValido(dir); err != nil {
		return nil, fmt.Errorf("machine %q can't be thawed: %w. Remove it (kling rm %s) and start it again",
			mc.Name, err, mc.Name)
	}
	// VMM: congelada con uno que el de ahora no sabe cargar (otro VMM u otra
	// MAJOR.MINOR de Firecracker, ver causaObsoleto). Se dice aquí, antes de
	// lanzar nada: el error crudo del VMM al cargar no señala la causa.
	if causa := causaObsoleto(vmmDelVolcado(dir), m.origenActual().vmm); causa != "" {
		return nil, fmt.Errorf("machine %q can't be thawed: it was frozen %s, and a memory dump "+
			"can't be loaded by another VMM version. Go back to that VMM to thaw it, or remove it "+
			"(kling rm %s) and start it again", mc.Name, strings.Replace(causa, "made with", "with", 1), mc.Name)
	}
	// KERNEL (K2), como runFrom con los dorados: solo se avisa, porque
	// descongelar no usa vmlinux (ver avisoKernel).
	if aviso := m.avisoKernel(kernelDelVolcado(dir), fmt.Sprintf("machine %q", mc.Name)); aviso != "" {
		log.Print(aviso)
	}
	// Admisión de memoria, como Run y runFrom: descongelar devuelve al host
	// toda la RAM del invitado, y una tormenta de thaws (el gateway
	// despertando a la vez lo que congeló) lo dejaba sin memoria sin que nada
	// dijera que no. El techo entero (MemMaxMiB): es lo que el VMM mapea al
	// cargar. Sin la del disco (checkDisk): despertar no crea disco —el
	// overlay y el volcado ya existen—, y el diferencial sin reflink ya mide
	// el suyo (prepararMemoriaDesdeDiff). Reanudar una pausada (arriba) y
	// readoptar un VMM vivo no pasan por aquí: su memoria ya está ocupada.
	if err := m.admitirMemoria(); err != nil {
		return nil, err
	}
	releaseMem, merr := m.reserveMemoryMakingRoom(ctx, max(mc.MemMiB, mc.MemMaxMiB), "", mc.ID)
	if merr != nil {
		return nil, merr
	}
	defer releaseMem()
	// Congelada en diferencial (diff_volcado.go): el VMM carga de un solo
	// fichero, base + diff, que se construye aquí y vive mientras corra.
	base := leerSello(dir).DiffBase
	if base != "" {
		if err := m.baseDiffValida(mc, base); err != nil {
			return nil, fmt.Errorf("machine %q can't be thawed: %w", mc.Name, err)
		}
		full, err := m.prepararMemoriaDesdeDiff(ctx, mc, dir, base)
		if err != nil {
			return nil, fmt.Errorf("machine %q can't be thawed: %w", mc.Name, err)
		}
		memPath = full
	}
	// El mem.full de este despertar, si algo falla antes de abortar (que lo
	// retira igual): GiB por copia en ext4 hasta el siguiente thaw.
	soltarFull := func() {
		if base != "" {
			_ = os.Remove(filepath.Join(dir, memFull))
			m.borrarMemoriaAlmacen(mc.ID)
		}
	}
	// La memoria, a la caché ya: la E/S corre mientras se monta la red y se
	// lanza el VMM (ver precargar). Solo las pequeñas: en una grande el
	// invitado no toca todo al despertar, y leerla entera competiría con el
	// resto del host por el disco.
	if allocatedBytes(memPath) <= precargaMaxBytes {
		go precargar(memPath)
	}

	// Puerta de arranque: descongelar es cargar un snapshot en KVM —mapear su
	// memoria y reanudar los vCPU—, tan intensivo como un arranque en frío. Es,
	// además, la mayoría del ciclo, así que es aquí donde una tormenta simultánea
	// hace más daño. Va tras la readopción de arriba: readoptar no enciende nada,
	// no tiene por qué esperar turno.
	release, glErr := m.enterLaunch(ctx)
	if glErr != nil {
		soltarFull()
		return nil, glErr
	}
	defer release()
	crono.marca(&crono.p.WaitMS)

	_ = os.Remove(sock)

	// El namespace pudo desaparecer con un reinicio del host; lo rehacemos con el
	// mismo índice para que la máquina conserve su IP.
	// La red suele seguir montada desde el Freeze (ver red.go); si no —un
	// reinicio del daemon o del host, o el vigilante la soltó—, se rehace con
	// el mismo índice para que la máquina conserve su IP.
	egress, _ := knet.ParseEgress(mc.Egress)
	netcfg := knet.Plan(mc.NetIndex, mc.ID)
	if !m.redLista(netcfg, mc.ID) {
		n, err := m.redParaRehacer(mc)
		if err != nil {
			soltarFull()
			return nil, fmt.Errorf("rebuilding the network: %w", err)
		}
		netcfg = n
		if err := m.montarRed(netcfg, mc.ID, egress, mc.AllowDomains); err != nil {
			soltarFull()
			return nil, fmt.Errorf("rebuilding the network: %w", err)
		}
	}
	// Las aristas del nodo, si es de un grafo: si la red se rehízo, sus
	// proxies de enlace y su resolver se fueron con ella. Idempotente. Un
	// fallo no tumba el thaw: las aristas fallan cerradas, y se dice.
	if spec, ok := m.especRedGrafo(mc); ok {
		if err := montarRedGrafo(netcfg, spec); err != nil {
			log.Printf("thaw: %s woke up without its graph edges: %v", mc.Name, err)
		}
	}
	crono.marca(&crono.p.NetMS)
	var pid int
	var c *fc.Client
	var err error

	// El VMM nace ya en su cgroup con techo de CPU (ver cgroupParaLanzar): el
	// de arranque, que se baja al configurado en cuanto termina de arrancar
	// (al final, tras los ganchos) o, en cualquier otra salida, en el defer.
	// Ver arranque_cpu.go.
	if mc.CPUPct <= 0 {
		mc.CPUPct = techoDelDaemon(mc.VCPUs)
	}
	impulso := m.nuevoImpulso(mc.ID, mc.CPUPct, mc.VCPUs, mc.CPUPctFixed)
	defer impulso.fin()
	cg := m.cgroupParaLanzar(mc.ID, impulso.tope)
	if cg != nil {
		defer cg.Close()
	}
	var enCg bool

	// abortar es la única salida de error a partir de aquí.
	//
	// Sin esto, un fallo tras el spawn —el caso real es el TSC invalidado al
	// reiniciar el host, que hace fallar LoadSnapshot— dejaba tres cosas: un
	// firecracker vivo SIN snapshot cargado, su namespace de red montado, y la
	// máquina figurando como warm. El siguiente thaw veía ese proceso en
	// liveVMs, lo readoptaba como sano y marcaba la máquina running: el gateway
	// enrutaba a una microVM vacía que no contesta nunca, y sweep() no lo
	// detectaba porque el proceso existe de verdad.
	abortar := func(err error) (*api.Machine, error) {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		m.desmontarRed(netcfg, mc.ID)
		if base != "" {
			// El mem.full de este despertar (diff_volcado.go): el siguiente
			// lo rehace, y mientras tanto ocuparía lo que el diff.
			_ = os.Remove(filepath.Join(dir, memFull))
			m.borrarMemoriaAlmacen(mc.ID)
		}
		return nil, err
	}

	if m.JailerBlocked != "" {
		return abortar(errors.New(m.JailerBlocked))
	}
	if m.jailerJailed {
		// El warm también se descongela dentro de un jail, o el aislamiento se
		// perdería justo en las descongelaciones —que son la mayoría del ciclo—.
		// Mismo patrón que runFrom: poblar el chroot antes de cargar.
		pid, sock, enCg, err = m.spawnJailed(mc.ID, netcfg, cg)
		if err != nil {
			return abortar(err)
		}
		crono.marca(&crono.p.SpawnMS)
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			return abortar(err)
		}
		crono.marca(&crono.p.SocketMS)
		// La imagen se resuelve igual que en runFrom: una imagen por capas no
		// tiene $NAME.ext4, tiene su capa y la base de su familia. Enlazar la
		// ruta monolítica fallaba con "no such file" en cuanto la imagen era por
		// capas, y como jailer era opcional nadie lo había visto.
		imgBase, imgLayer, ierr := m.imageLayer(mc.Image)
		if ierr != nil {
			return abortar(ierr)
		}
		toLink := []string{snapPath, memPath, imgBase, imgLayer, filepath.Join(dir, "overlay.ext4")}
		for _, v := range mc.Volumes {
			toLink = append(toLink, m.volumePath(v.Name))
		}
		toLink = append(toLink, m.copyImages(mc)...)
		if err := m.prepareJail(mc.ID, toLink...); err != nil {
			return abortar(err)
		}
		crono.marca(&crono.p.SpawnMS)
	} else {
		pid, enCg, err = m.spawn(mc.ID, sock, netcfg, cg)
		if err != nil {
			return abortar(err)
		}
		crono.marca(&crono.p.SpawnMS)
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			return abortar(err)
		}
		crono.marca(&crono.p.SocketMS)
	}

	// macOS: la política de salida viaja con la instancia, no con el snapshot.
	if err := m.redAntesDeArrancar(ctx, c, mc.ID); err != nil {
		return abortar(err)
	}
	crono.marca(&crono.p.NetMS)
	start := time.Now()
	// Con seguimiento otra vez si venía en diferencial: el siguiente freeze
	// vuelca solo lo escrito desde ahora y lo funde con el diff que ya hay.
	if err := c.ConPlazo(plazoVolcado(max(mc.MemMiB, mc.MemMaxMiB))).LoadSnapshotTracking(ctx, snapPath, memPath, true, base != ""); err != nil {
		// Mismo motivo que en runFrom: si la causa es el TSC de un host
		// reiniciado, el error crudo de Firecracker no le dice a nadie qué
		// hacer, y este texto es lo que verá quien despierte la máquina.
		return abortar(explainRestoreErr(err, fmt.Sprintf("machine %q", mc.Name),
			"  kling rm "+mc.Name+"\n"+
				"  and start it again (kling run -from <snapshot>, or a cold boot from its image)"))
	}
	elapsed := time.Since(start).Milliseconds()
	crono.marca(&crono.p.LoadMS)
	if err := m.abrirReenvios(ctx, c, mc.ID); err != nil {
		return abortar(err)
	}
	// Sus credenciales (credenciales.go): el proxy y el resolver se rehicieron
	// con la red si esta se había desmontado, y el VMM es nuevo, así que los
	// marcadores vuelven a MMDS. Un fallo no tumba el thaw: la máquina sirve y
	// el dominio con credencial falla cerrado, y queda dicho en el log.
	if _, err := m.reentregarCredenciales(ctx, mc, c); err != nil {
		log.Printf("thaw: %s woke up without its credentials: %v", mc.Name, err)
	}
	crono.marca(&crono.p.ForwardsMS)
	// El invitado despierta con el reloj del momento en que se congeló, y si
	// otras máquinas salieron del mismo estado, con su mismo CSPRNG. Se corrige
	// antes de devolverla como running (ver resync.go).
	var resyncT time.Duration
	var resyncOK bool
	var listo *api.GuestReady
	if _, sinAgente := m.resyncSinAgente.LoadAndDelete(claveThaw(mc.ID)); !sinAgente {
		resyncT, resyncOK, listo = m.resyncGuest(ctx, mc.ID, "", api.ResyncThaw)
	}
	crono.marca(&crono.p.ResyncMS)

	// Reaplicar el techo de CPU: el firecracker de una máquina descongelada es un
	// proceso NUEVO (spawn), así que su pertenencia al cgroup no sobrevive al ciclo
	// freeze→thaw. Sin esto una microVM descongelada corría en el cgroup del daemon,
	// SIN límite —hueco de aislamiento— y, además, se saltaba el cpu_pct que viaja
	// con el snapshot justo en el camino de thaw, que es el habitual del gateway.
	// Mismo patrón que Run (boot) y runFrom. Si ya nació dentro, no hay nada
	// que mover.
	if !enCg {
		if warn := m.limitCPU(mc.ID, pid, impulso.tope); warn != "" {
			log.Printf("warning: %s: %s", mc.Name, warn)
		}
	}
	crono.marca(&crono.p.CgroupMS)

	m.mu.Lock()
	cur = m.byID[mc.ID]
	if cur == nil {
		delete(m.socket, mc.ID)
		m.mu.Unlock()
		// Acabamos de arrancar un VMM para una maquina que ya no existe. Hay que
		// matarlo AQUI: dejarlo seria un firecracker huerfano reteniendo la RAM
		// de su invitado, que no aparece en `kling ps` y que nadie contabiliza al
		// decidir si cabe la siguiente microVM.
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		m.desmontarRed(netcfg, mc.ID)
		return nil, fmt.Errorf("machine %q was removed while it was being thawed", mc.Name)
	}
	now := time.Now()
	cur.State = api.StateRunning
	cur.StartedAt = &now
	cur.FrozenAt = nil
	cur.Hold = ""
	cur.ThawMS = elapsed
	cur.PID = pid
	// El techo por defecto se decidió sobre la copia (arriba); se anota en la
	// viva para que state.json y `kling ps` digan el que de verdad se aplicó.
	cur.CPUPct = mc.CPUPct
	// Descongelada de SU mem.file: ya no comparte páginas con un dorado.
	cur.MemShared = false
	// Pero si venía en diferencial, sigue midiéndose contra él.
	cur.DiffBase = base
	// Lo que reentregarCredenciales anotó en la copia.
	cur.CredentialAnyDatabase = mc.CredentialAnyDatabase
	m.socket[mc.ID] = sock
	m.persist()
	out := *cur
	m.mu.Unlock()

	out.DiskBytes = m.touchDisk(mc.ID)

	// Las carpetas vivas vuelven a conectarse: el agente reconoce cada montaje
	// por su tag y sigue con el mismo. En segundo plano; lo que el invitado
	// pida mientras tanto espera a la sesión.
	m.startShares(mc.ID)
	// Los ganchos de la imagen, con credenciales y red ya en su sitio.
	m.trasRestaurar(ctx, mc.ID, api.ResyncThaw, listo)
	// Fin del impulso de arranque: ya, si su memoria trae el "listo" (lo
	// normal) o no declara sonda; si no, cuando la pase.
	impulso.entregarRestaurada(listo)

	fases := crono.cerrar()
	out.Wake = fases
	m.mu.Lock()
	if cur := m.byID[mc.ID]; cur != nil {
		cur.Wake = fases
	}
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: now, Type: api.EvThawed, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("thawed in %d ms%s%s", elapsed, resyncNota(resyncT, resyncOK), notaFases(fases))})
	return &out, nil
}

// SetLabels reetiqueta una máquina viva.
func (m *Manager) SetLabels(ref string, labels map[string]string) error {
	// Las de grafo las pone el daemon y no cambian: son la mitad de la
	// comprobación de cada arista (grafo_red.go).
	if err := api.ValidateNoGraphLabels(labels); err != nil {
		return err
	}
	mc, ok := m.Get(ref)
	if !ok {
		return fmt.Errorf("machine %q doesn't exist", ref)
	}
	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		m.mu.Unlock()
		// La eliminaron entre el Get y el candado. Etiquetar algo que ya no
		// existe no es un error del que llama, pero deref nil sí tumbaba el
		// daemon entero.
		return fmt.Errorf("machine %q no longer exists", ref)
	}
	cambia := cambianEtiquetasDB(live.Labels, labels)
	live.Labels = api.MergeLabels(live.Labels, labels)
	m.persist()
	id, plan := live.ID, knet.Plan(live.NetIndex, live.ID)
	m.mu.Unlock()
	if cambia {
		// Lo que el proxy comprueba en cada conexión (copias_db.go) acaba de
		// cambiar: una copia que deja de estar lista o cambia de dueño no
		// conserva las sesiones abiertas, ni un agente que cambia de dueño
		// las suyas. Después del cambio y fuera del candado.
		m.invalidarSesiones(id, "kling db labels changed")
		invalidarAgente(plan)
		invalidarOrigen(id)
	}
	return nil
}

// Stop termina la microVM sin borrar su directorio.
func (m *Manager) Stop(ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	// El cerrojo de ciclo de vida, que aqui faltaba y lo tienen Freeze, Thaw,
	// Squeeze, PutMMDS y Remove. Sin el, un Stop concurrente a un Thaw desmonta
	// el namespace de red POR DEBAJO del thaw: la maquina queda "running" y sin
	// red, y el gateway ve timeouts que no apuntan a nada.
	defer m.lock(mc.ID)()
	// Lo que vio Get antes de esperar el cerrojo puede no valer ya: si la
	// máquina estaba arrancando, su NetIndex era aún el de antes de montar la
	// red, y desmontar con él dejaba la red de verdad montada.
	if cur, ok := m.get(mc.ID); ok {
		mc = cur
	}
	defer m.marcarTransicion(mc.ID, api.TransitionStopping)()
	m.kill(mc.ID)
	// Una máquina parada no necesita namespace ni cgroup: se recrean al arrancar.
	m.desmontarRed(knet.Plan(mc.NetIndex, mc.ID), mc.ID)
	m.releaseCPU(mc.ID)

	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		// La eliminó alguien mientras parábamos. No es un error: el resultado
		// que se pedía —que deje de correr— ya se cumplió. Antes esto era un
		// deref nil que se llevaba por delante el daemon entero.
		m.mu.Unlock()
		// mc es ya una COPIA (Get la devuelve por valor), así que ajustarla aquí
		// no toca el estado compartido.
		mc.State, mc.PID = api.StateStopped, 0
		return mc, nil
	}
	live.State = api.StateStopped
	live.PID = 0
	live.Forwards = nil
	delete(m.socket, mc.ID)
	m.persist()
	out := *live
	m.mu.Unlock()
	m.invalidarSesiones(mc.ID, "stopped")

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStopped, ID: mc.ID, Name: mc.Name})
	return &out, nil
}

// Remove para la máquina y borra su directorio, snapshot incluido.
func (m *Manager) Remove(ref string) error {
	return m.removeSi(ref, nil)
}

// removeSi es Remove, pero solo si sigue valiendo sigue(máquina) con el
// cerrojo de ciclo de vida ya tomado; si no, errYaNoToca y no se toca nada.
// sigue nil es borrar siempre.
//
// Lo usan los que deciden borrar mirando una foto sin cerrojo: el TTL con
// on_ttl=remove (un renew pudo llegar entre medias) y la recogida de disco
// (la congelada que eligió pudo despertarse con sesiones dentro).
func (m *Manager) removeSi(ref string, sigue func(*api.Machine) bool) error {
	mc, ok := m.Get(ref)
	if !ok {
		return fmt.Errorf("machine %q doesn't exist", ref)
	}
	// Sin retirar nada a mano: el registro lo hace solo cuando sale el ultimo.
	// Borrar la entrada desde aqui era justo lo que abria la ventana.
	defer m.lock(mc.ID)()
	// Releer con el cerrojo: si estaba arrancando, Run lo tenía y la copia de
	// Get es de antes de montar la red (NetIndex) y de lanzar el VMM.
	if cur, ok := m.get(mc.ID); ok {
		mc = cur
	} else if sigue != nil {
		return errYaNoToca
	}
	if sigue != nil && !sigue(mc) {
		return errYaNoToca
	}
	defer m.marcarTransicion(mc.ID, api.TransitionRemoving)()
	m.kill(mc.ID)
	m.desmontarRed(knet.Plan(mc.NetIndex, mc.ID), mc.ID)
	m.releaseCPU(mc.ID)
	// El chroot del jail vive aparte del directorio de la máquina: se limpia
	// también, o cada restauración jailed deja un árbol huérfano.
	if err := m.borrarJail(mc.ID); err != nil {
		log.Printf("warning: %v", err)
	}
	if err := os.RemoveAll(m.dir(mc.ID)); err != nil {
		return err
	}
	m.borrarAuditoria(mc.ID)
	// Su overlay en el almacén de discos, si lo tenía (ver cow.go).
	m.borrarOverlayAlmacen(mc.ID)
	// Su enlace corto en /tmp/kling-<uid> (macOS) ya no apunta a nada; el
	// vigilante lo barrería en la siguiente vuelta, pero así no queda ni ese rato.
	fc.BarrerEnlaces(m.root)
	m.mu.Lock()
	delete(m.byID, mc.ID)
	delete(m.socket, mc.ID)
	m.persist()
	m.mu.Unlock()
	m.invalidarSesiones(mc.ID, "removed")
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStopped, ID: mc.ID, Name: mc.Name, Message: "removed"})
	return nil
}

// kill para el VMM. SIGKILL y no SIGTERM: un invitado hostil puede ignorar una
// señal educada, y la garantía de que una microVM se para no puede depender de
// que colabore.
//
// Lo que sí se le concede antes es vaciar el volumen al disco: eso no es
// cortesía con el invitado, es que si no, el volumen persistente pierde lo
// último que se escribió en él.
func (m *Manager) kill(id string) { m.killMachine(id, true) }

// killPaused mata un VMM que YA está pausado, sin pedirle nada al invitado.
//
// Un invitado pausado no atiende HTTP: pedirle que vacíe sus volúmenes se come
// el plazo entero de la petición y acaba en un "no vació sus volúmenes antes de
// morir" que asusta y no significa nada. Lo usa Freeze, que ya vació ANTES de
// pausar, que es el único momento en que el invitado podía responder.
func (m *Manager) killPaused(id string) { m.killMachine(id, false) }

func (m *Manager) killMachine(id string, flush bool) {
	// Las carpetas vivas primero: sus sesiones con el invitado van a morir, y
	// es mejor cerrarlas que esperar a que el keepalive lo note.
	m.stopShares(id)
	// Dos kill de la misma máquina a la vez (un fail sin cerrojo de ciclo de
	// vida contra un Stop) leían los dos el mismo PID; el segundo mandaba su
	// SIGKILL cuando el primero ya lo había visto morir, y ese PID podía ser
	// ya de otro proceso. Se hacen de uno en uno, y el que mata deja el PID a
	// 0 (abajo): el siguiente ya no tiene a quién matar.
	defer m.muertes.tomar(id)()
	// Una COPIA tomada con el candado: la entrada viva la escriben otros
	// (anotarListo, touchTTL...) con m.mu, y leer su State o sus volúmenes
	// sin él era una carrera.
	m.mu.RLock()
	var mc *api.Machine
	if live := m.byID[id]; live != nil {
		mc = live.Clone()
	}
	m.mu.RUnlock()
	if mc == nil || mc.PID == 0 {
		return
	}
	pid := mc.PID
	// Una pausada no contesta: pedirle que vacíe se comería el plazo entero.
	if flush && mc.State != api.StatePaused {
		// El servicio primero: es quien escribe en los volúmenes.
		m.stopService(mc)
		m.flushVolume(mc)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	defer func() {
		m.mu.Lock()
		if live := m.byID[id]; live != nil && live.PID == pid {
			live.PID = 0
		}
		m.mu.Unlock()
	}()

	// Esperar a que MUERA de verdad, no solo a mandar la señal.
	//
	// SIGKILL es asíncrono: el proceso tarda en morir y el kernel en reclamar su
	// memoria —un gigabyte no se libera al instante—. Quien congela para hacer
	// sitio (el gateway, cuando el anfitrión está lleno) reintentaba antes de que
	// esa memoria estuviera disponible y volvía a chocar con "no cabe". Bloquear
	// aquí hace que Freeze/Stop/Remove no digan "hecho" mientras el VMM siga vivo
	// gastando RAM, que es lo que "hecho" debería significar.
	//
	// Con tope: un firecracker que ignorase el SIGKILL (imposible, pero) no debe
	// colgar el daemon. El proceso lo recoge la goroutine de cmd.Wait() del
	// arranque, así que Kill(pid, 0) devuelve ESRCH en cuanto desaparece.
	waitGone(pid, 5*time.Second)
}

// waitGone espera a que un pid desaparezca de la tabla de procesos.
//
// Kill(pid, 0) no manda señal: solo pregunta si el proceso existe. Devuelve
// ESRCH cuando ya no —ni vivo ni zombi—, que es justo cuando su memoria está
// reclamada. Sondea con pausas cortas porque morir tras un SIGKILL es cuestión
// de milisegundos en el caso normal.
func waitGone(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// fail da una máquina por perdida: mata su VMM, desmonta su red y su cgroup y
// la marca failed con el error.
//
// mc puede ser el puntero vivo o una COPIA (lo que devuelve Get): se escribe
// siempre en la entrada viva de byID. Antes se escribía en mc, y con una copia
// —Freeze, al no poder reanudar tras un volcado fallido— el VMM moría pero la
// máquina seguía "running" con un PID muerto: el gateway le enrutaba hasta que
// el vigilante, 10 s después, la relabelaba con "the microVM process
// disappeared", perdiendo el error real (M-01). Solo si ya no está registrada
// (la borraron, o Run la abandonó) se escribe en mc, que entonces no ve nadie.
func (m *Manager) fail(mc *api.Machine, err error) {
	id := mc.ID
	m.kill(id)

	// El índice de red, de la viva: el de una copia tomada antes de esperar el
	// cerrojo puede ser anterior a montar la red.
	m.mu.RLock()
	netIndex := mc.NetIndex
	if live := m.byID[id]; live != nil {
		netIndex = live.NetIndex
	}
	m.mu.RUnlock()
	m.desmontarRed(knet.Plan(netIndex, id), id)
	m.releaseCPU(id)

	m.mu.Lock()
	destino := m.byID[id]
	if destino == nil {
		destino = mc
	}
	now := time.Now()
	destino.State = api.StateFailed
	destino.LastErr = err.Error()
	destino.Forwards = nil
	// La hora del fallo es lo que permite recogerla luego: una failed sin fecha
	// se quedaba en la lista para siempre (ver gcFailed).
	destino.FailedAt = &now
	name := destino.Name
	m.persist()
	m.mu.Unlock()
	m.invalidarSesiones(id, "failed")
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvFailed, ID: id, Name: name, Message: err.Error()})
}
