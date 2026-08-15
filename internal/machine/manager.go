// Package machine implementa el ciclo de vida de las microVMs.
package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
)

// La raíz se monta en SOLO LECTURA y el init es overlay-init, que superpone el
// disco propio de cada máquina (/dev/vdb) sobre la imagen base compartida
// (/dev/vda). Así N microVMs comparten una base de cientos de MB en vez de
// copiarla N veces.
const bootArgsBase = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/sbin/overlay-init"

// bootArgs arma la línea de comandos del kernel del invitado. La red la aplica
// el propio kernel con el parámetro ip=, sin herramientas dentro de la imagen.
//
// El punto de montaje del volumen viaja aquí y no dentro de la imagen: así el
// mismo snapshot dorado sirve con volúmenes distintos, o sin ninguno. Lo mismo
// vale para layerDev, el disco de la capa de servicio: vacío en las imágenes
// monolíticas, que siguen arrancando con la línea de siempre.
func bootArgs(vols []api.VolumeAttachment, allowExec bool, layerDev string) string {
	return bootArgsBase + " " + knet.BootArg() + volumeBootArg(vols) + execBootArg(allowExec) + layerBootArg(layerDev)
}

// defaultOverlayMiB es el tamaño lógico del disco escribible por máquina. Al ser
// disperso, el coste real en disco es solo lo que la microVM llegue a escribir.
const defaultOverlayMiB = 512

// Límites de seguridad. El código que corre dentro se considera hostil, así que
// una sola microVM no debe poder degradar el host ni a las demás.
const (
	// MaxMachines evita que un cliente comprometido agote el host creando
	// máquinas sin fin.
	MaxMachines = 256

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

	bus    *events.Bus
	mu     sync.RWMutex
	byID   map[string]*api.Machine
	socket map[string]string // id -> ruta del socket de firecracker

	// reserved son los ids cuyo directorio se está CONSTRUYENDO ahora mismo, aún
	// sin entrada en byID. Ver reserveDir: sin esto el barrido de huérfanos los
	// borra bajo los pies de quien los está llenando. Se toca bajo mu.
	reserved map[string]bool

	// netCursor rota los índices de red en vez de reutilizar el menor libre.
	// Ver allocNetIndex.
	netCursor int

	// layerOK memoriza qué bases traen un overlay-init que entiende las capas.
	// Es dato de un fichero que no cambia, y preguntarlo cuesta un debugfs en el
	// camino de arranque en frío. Ver baseSupportsLayers.
	layerOK sync.Map

	// pendingMiB es la memoria de las microVMs que están ARRANCANDO ahora mismo,
	// aún sin proceso que la ocupe. checkHostMemory la resta de lo disponible:
	// sin esto, dos arranques concurrentes ven los dos la misma memoria libre,
	// pasan los dos, y juntos no caben — el OOM del anfitrión que el check existe
	// para impedir. Se toca bajo mu.
	pendingMiB int

	// snapPending cuenta, por snapshot dorado, cuántas instancias están
	// arrancando de él ahora mismo. Sirve a reserveMemory para saber si el
	// mem.file ya está anclado y cobrar solo la fracción divergente a las copias.
	// Se toca bajo mu.
	snapPending map[string]int

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
	lifecycle sync.Map // id -> *sync.Mutex

	// Escritura del estado, fuera del lock. Ver persist().
	stateMu   sync.Mutex
	pending   []api.Machine // último snapshot sin escribir; el nuevo pisa al viejo
	hasPend   bool
	wake      chan struct{}
	quit      chan struct{}
	quitOnce  sync.Once
	persistWG sync.WaitGroup
}

// lock serializa las operaciones de ciclo de vida de una máquina concreta.
func (m *Manager) lock(id string) func() {
	v, _ := m.lifecycle.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func NewManager(root, fcBin, runAs string, bus *events.Bus) (*Manager, error) {
	for _, d := range []string{root, filepath.Join(root, "machines"), filepath.Join(root, "images")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	priv, warn := resolvePrivileges(runAs)
	m := &Manager{
		root: root, fcBin: fcBin, bus: bus, priv: priv, PrivWarning: warn,
		byID:        make(map[string]*api.Machine),
		socket:      make(map[string]string),
		wake:        make(chan struct{}, 1),
		quit:        make(chan struct{}),
		launchGate:  make(chan struct{}, maxParallelLaunch()),
		snapPending: make(map[string]int),
	}
	m.persistWG.Add(1)
	go m.persistLoop()
	if cg, err := ensureDelegation(); err != nil {
		m.CgroupWarning = err.Error()
	} else {
		m.cgroupRoot = cg
	}

	priv.EnsureReadable(filepath.Join(root, "images"))
	m.load()
	for _, mc := range m.byID {
		if mc.NetIndex > m.netCursor {
			m.netCursor = mc.NetIndex
		}
	}
	m.reconcile()
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

func (m *Manager) load() {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return
	}
	var list []*api.Machine
	if json.Unmarshal(b, &list) != nil {
		return
	}
	// No se toca el estado aquí: reconcile() decide comparando con la realidad
	// del host, porque una microVM SÍ puede sobrevivir al daemon.
	for _, mc := range list {
		m.byID[mc.ID] = mc
	}
}

// persist encola una escritura del estado. Se llama SIEMPRE con m.mu tomado por
// quien la invoca — los doce sitios que la usan están dentro de una operación de
// ciclo de vida.
//
// Antes serializaba y escribía aquí mismo, con el lock global cogido: cada
// transición pagaba la latencia del disco y, mientras tanto, ninguna otra
// máquina podía arrancar, congelarse ni descongelarse. Ahora solo toma la foto y
// se va; escribirla es cosa de persistLoop.
//
// La foto se copia POR VALOR, no por puntero. Es la diferencia entre esto y una
// carrera de datos: si se guardaran los *api.Machine, json.Marshal los leería
// fuera del lock mientras otra goroutine les cambia State, PID o los punteros
// StartedAt/FrozenAt, y el fichero podría acabar describiendo un estado que
// nunca existió (una máquina "warm" con PID vivo, por ejemplo).
func (m *Manager) persist() {
	list := make([]api.Machine, 0, len(m.byID))
	for _, mc := range m.byID {
		list = append(list, *mc)
	}

	m.stateMu.Lock()
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
			m.writePending()

		case <-m.quit:
			if timer != nil {
				timer.Stop()
			}
			m.writePending()
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
	list := m.pending
	m.pending, m.hasPend = nil, false
	m.stateMu.Unlock()

	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		log.Printf("state: could not serialize it: %v", err)
		return
	}

	tmp := m.statePath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("state: could not write %s: %v", tmp, err)
		return
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		log.Printf("state: incomplete write: %v", err)
		return
	}
	// fsync del fichero Y del directorio. Sin el segundo, el rename puede no
	// haber llegado al disco cuando se corta la luz, y el daemon arrancaría
	// leyendo el estado anterior — que es peor que no leer ninguno, porque se
	// da por bueno.
	if err := f.Sync(); err != nil {
		log.Printf("state: fsync failed: %v", err)
	}
	if err := f.Close(); err != nil {
		log.Printf("state: close failed: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath()); err != nil {
		log.Printf("state: rename failed: %v", err)
		return
	}
	if d, err := os.Open(m.root); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// Close para la escritura de estado tras volcar lo que quede pendiente.
//
// Lo llama el daemon en su camino de apagado, y tiene que esperarse: si el
// proceso sale antes, se pierde la última transición y el arranque siguiente
// reconstruye un estado que ya no es el real.
func (m *Manager) Close() {
	m.quitOnce.Do(func() {
		close(m.quit)
		m.persistWG.Wait()
	})
}

// ── consultas ─────────────────────────────────────────────────────────────────

func (m *Manager) List() []*api.Machine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*api.Machine, 0, len(m.byID))
	for _, mc := range m.byID {
		// DiskBytes viene de la caché: recorrer el directorio de cada máquina
		// aquí era un walk por máquina en CADA `kling ps`, y encima con el
		// candado global cogido, así que contar bytes bloqueaba arranques y
		// congelaciones. Lo refresca el vigilante cada pocos segundos.
		c := *mc
		out = append(out, &c)
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
	m.mu.RLock()
	defer m.mu.RUnlock()
	if mc, ok := m.byID[ref]; ok {
		c := *mc
		return &c, true
	}
	for _, mc := range m.byID {
		if mc.Name == ref || (len(ref) >= 4 && len(mc.ID) >= len(ref) && mc.ID[:len(ref)] == ref) {
			c := *mc
			return &c, true
		}
	}
	return nil, false
}

func (m *Manager) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.byID)
}

// netIndexSpace es el rango de /30 disponibles en 172.30.0.0/16.
const netIndexSpace = 16000

// allocNetIndex asigna el siguiente índice libre EN ROTACIÓN, no el menor.
//
// Reutilizar el índice más bajo parece más ordenado, pero con máquinas efímeras
// —que nacen y mueren en cientos de milisegundos— significa que la siguiente
// recibe la IP que acaba de liberar la anterior. El host conserva entradas de
// conntrack de la conexión previa y la nueva microVM se come un "connection
// reset by peer".
//
// Rotando, una IP tarda 16.000 máquinas en repetirse.
func (m *Manager) allocNetIndex() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	used := make(map[int]bool, len(m.byID))
	for _, mc := range m.byID {
		used[mc.NetIndex] = true
	}
	for i := 0; i < netIndexSpace; i++ {
		m.netCursor = m.netCursor%netIndexSpace + 1
		if !used[m.netCursor] {
			return m.netCursor
		}
	}
	return 1 // rango agotado: MaxMachines lo impide mucho antes
}

// ── ciclo de vida ─────────────────────────────────────────────────────────────

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Run crea una microVM y la arranca en frío.
func (m *Manager) Run(ctx context.Context, req api.RunRequest) (*api.Machine, error) {
	// El tope va ANTES de bifurcar: restaurar desde un snapshot es tan capaz de
	// agotar el host como arrancar en frío, y es el camino que más rápido crea
	// —gateway, fondo, efímero—. Comprobarlo solo en el arranque en frío lo
	// dejaba sin freno justo donde más falta hace.
	if n := m.Count(); n >= MaxMachines {
		return nil, fmt.Errorf("limit of %d machines reached (there are %d)", MaxMachines, n)
	}

	// Instanciar desde un snapshot dorado es un camino distinto: no se arranca
	// nada en frío, se restaura.
	if req.From != "" {
		return m.runFrom(ctx, req)
	}
	if req.Image == "" {
		req.Image = "default"
	}
	if req.VCPUs <= 0 {
		req.VCPUs = 1
	}
	if req.MemMiB <= 0 {
		req.MemMiB = 256
	}

	if n := m.Count(); n >= MaxMachines {
		return nil, fmt.Errorf("limit of %d machines reached (there are %d)", MaxMachines, n)
	}

	vols, err := m.resolveVolumes(req)
	for _, v := range vols {
		// Solo los que vamos a montar en ESCRITURA. e2fsck escribe, y un volumen
		// compartido puede tenerlo abierto otra microVM ahora mismo: repararlo
		// por debajo sería justo la corrupción que el modo lectura evita.
		if !v.readOnly {
			repairVolume(ctx, v.path)
		}
	}
	if err != nil {
		return nil, err
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
	releaseMem, err := m.reserveMemory(req.MemMiB, "")
	if err != nil {
		return nil, err
	}
	defer releaseMem()
	if _, err := os.Stat(m.KernelPath()); err != nil {
		return nil, fmt.Errorf("missing kernel at %s", m.KernelPath())
	}

	// Sin puente no hay quien monte los volúmenes.
	//
	// El puente es lo único que lee kling.volume y monta dentro del invitado.
	// Una imagen en modo HTTP nativo no lo lleva, así que el disco se
	// engancharía, el snapshot lo registraría y `volume ls` diría "en uso"…
	// mientras dentro nadie monta nada y todo lo escrito muere con la máquina.
	// Se comprueba ANTES de crear el directorio, para no tener que limpiarlo.
	if len(vols) > 0 {
		switch has, herr := imageHasBridge(ctx, src, layer); {
		case herr != nil:
			// Sin poder comprobarlo se sigue, dejando constancia: convertir una
			// herramienta de diagnóstico en una dependencia de arranque sería
			// peor que el problema.
			log.Printf("warning: could not check whether %q has a bridge: %v", req.Image, herr)
		case !has:
			return nil, fmt.Errorf("image %q does not have kling-bridge, and the bridge is what mounts "+
				"the volumes inside the guest.\n"+
				"With this image the disk would be attached but nobody would mount it, and everything written "+
				"to %s would die with the machine, without a single error.\n"+
				"Repackage it in stdio mode, or remove the volume",
				req.Image, vols[0].mount)
		}
	}

	id := newID()
	if req.Name == "" {
		req.Name = req.Image + "-" + id[:6]
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
	if err := m.newOverlay(ctx, overlay); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	mc := &api.Machine{
		ID: id, Name: req.Name, Image: req.Image, State: api.StateCreated,
		VCPUs: req.VCPUs, MemMiB: req.MemMiB, CreatedAt: time.Now(),
		TTLSeconds: req.TTLSeconds, CPUPct: req.CPUPct, Labels: req.Labels,
		Volumes: attachments(vols),
	}
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

	egress, err := knet.ParseEgress(req.Egress)
	if err != nil {
		return abandonar(err)
	}
	netcfg := knet.Plan(m.allocNetIndex(), id)
	if err := netcfg.Setup(egress, req.AllowDomains, m.priv.UID); err != nil {
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
		netcfg.Teardown()
		return abandonar(err)
	}

	start := time.Now()
	pid, err := m.boot(ctx, mc.ID, mc.VCPUs, mc.MemMiB, src, layer, overlay, netcfg, vols, req.AllowExec)
	if err != nil {
		// boot() devuelve el PID aunque falle DESPUÉS de lanzar el proceso, y
		// hay que matarlo aquí: m.fail() llama a kill(), que lee el PID de la
		// máquina — y ahí todavía es 0. Sin esto queda un firecracker vivo para
		// siempre esperando en un socket que ya nadie usa.
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		netcfg.Teardown()
		m.fail(mc, err)
		return nil, err
	}
	if mc.CPUPct <= 0 {
		m.mu.Lock()
		mc.CPUPct = defaultCPUPct
		m.mu.Unlock()
	}
	if warn := m.limitCPU(mc.ID, pid, mc.CPUPct); warn != "" {
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

	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: id, Name: mc.Name,
		Message: fmt.Sprintf("cold started in %d ms", out.BootMS)})
	return &out, nil
}

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
// Se formatea en un .tmp y se renombra, para que EXISTIR IMPLIQUE ESTAR
// COMPLETA. Comprobar solo la existencia sobre el nombre definitivo sería una
// trampa: si el daemon muere entre el Truncate y el mkfs queda medio giga de
// ceros sin sistema de ficheros, y a partir de ahí TODAS las microVMs arrancan
// con un /dev/vdb que no monta — un fallo que aparece dentro del invitado y no
// en el log del daemon.
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
	return os.Rename(tmp, path)
}

// newOverlay deja listo el disco escribible de una microVM.
//
// Copiar la plantilla ahorra el mkfs.ext4 por máquina, que son decenas de
// milisegundos sobre un arranque que aspira a estar en el orden de los 30 ms.
// Si la plantilla no se puede construir se formatea directamente: más lento,
// pero nadie se queda sin arrancar por una optimización.
func (m *Manager) newOverlay(ctx context.Context, dst string) error {
	if err := m.ensureOverlayTemplate(ctx); err != nil {
		log.Printf("overlay template not available (%v): formatting directly", err)
		return createOverlay(ctx, dst, defaultOverlayMiB)
	}
	// --sparse=always: el overlay es disperso y copiarlo denso destruiría lo
	// que hace que una máquina cueste ~8 MB en vez de 512.
	out, err := exec.CommandContext(ctx, "cp", "--sparse=always", m.overlayTemplatePath(), dst).CombinedOutput()
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
	out, err := exec.CommandContext(ctx, "mkfs.ext4",
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
func (m *Manager) boot(ctx context.Context, id string, vcpus, memMiB int, base, layer, overlay string, n *knet.Net, vols []resolvedVolume, allowExec bool) (int, error) {
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
	if jailerEnabled() {
		// Arranque en frío dentro del jail. A diferencia de la restauración, aquí
		// firecracker abre el KERNEL (SetBootSource) y los discos por su API, así
		// que también hay que replicar el kernel dentro del chroot.
		pid, sock, err = m.spawnJailed(id, n)
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
		pid, err = m.spawn(id, sock, n)
		if err != nil {
			return 0, err
		}
	}

	c := fc.New(sock)
	if err := waitSocket(ctx, c); err != nil {
		return pid, err
	}
	if err := c.SetBootSource(ctx, fc.BootSource{KernelImagePath: m.KernelPath(), BootArgs: bootArgs(attachments(vols), allowExec, layerDev)}); err != nil {
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
			log.Printf("warning: could not configure MMDS on %s: %v (no session secrets available)", id, err)
		}
	}
	// virtio-rng: sin esto, las instancias de un mismo snapshot clonarían el
	// estado del generador de aleatoriedad y podrían producir las mismas claves.
	if err := c.SetEntropy(ctx); err != nil {
		return pid, fmt.Errorf("adding entropy: %w", err)
	}
	if err := c.SetMachineConfig(ctx, fc.MachineConfig{VCPUCount: vcpus, MemSizeMiB: memMiB}); err != nil {
		return pid, err
	}
	// virtio-balloon a 0: no reclama nada al arrancar, pero deja el dispositivo
	// listo para que un "squeeze" posterior devuelva RAM al host entre sesiones.
	// Va ANTES de Start (Firecracker no lo admite en caliente) y así queda dentro
	// del snapshot dorado, heredado por las copias. No es fatal: si el kernel
	// invitado no trae el driver o la versión de Firecracker lo rechaza, la
	// microVM arranca igual y solo se pierde el squeeze.
	if err := c.SetBalloon(ctx, 0, true, balloonStatsPollSec); err != nil {
		log.Printf("warning: could not configure balloon on %s: %v (squeeze will not be available)", id, err)
	}
	if err := c.Start(ctx); err != nil {
		return pid, err
	}
	m.mu.Lock()
	m.socket[id] = sock
	m.mu.Unlock()
	return pid, nil
}

// spawn arranca firecracker desacoplado del daemon y devuelve su PID.
func (m *Manager) spawn(id, sock string, n *knet.Net) (int, error) {
	logf, err := os.Create(filepath.Join(m.dir(id), "firecracker.log"))
	if err != nil {
		return 0, err
	}
	// Firecracker corre DENTRO del namespace de la microVM: es donde vive su tap0.
	// Orden: primero el namespace (necesita privilegios), después soltarlos.
	argv := m.priv.Wrap([]string{m.fcBin, "--api-sock", sock})
	if n != nil {
		argv = n.Wrap(argv[0], argv[1:]...)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return 0, fmt.Errorf("launching firecracker: %w", err)
	}
	// Sin Wait() el proceso quedaría zombi al terminar.
	go func() { _ = cmd.Wait(); logf.Close() }()
	return cmd.Process.Pid, nil
}

func waitSocket(ctx context.Context, c *fc.Client) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.Ping(ctx) == nil {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("firecracker socket did not respond within 5s")
}

// Freeze pausa la microVM, la vuelca a disco y libera su RAM y su proceso.
func (m *Manager) Freeze(ctx context.Context, ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()

	// Pudo congelarla otro mientras esperábamos.
	if cur, ok := m.Get(mc.ID); ok && cur.State == api.StateWarm {
		return cur, nil
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
			"becomes a golden snapshot. Use squeeze (does not dump to disk) or stop/rm", mc.ID[:12])
	}

	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}

	dir := m.dir(mc.ID)
	snapPath, memPath := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")

	// Una instancia jailed corre chrooteada: no puede escribir en el dir del
	// host. Se le pide el volcado en la raíz de su chroot y luego se recupera al
	// dir real. Mismo filesystem, así que el traslado es un rename atómico y el
	// mem.file conserva su inodo —y su caché de páginas—.
	jailed := jailerEnabled() && strings.HasPrefix(sock, m.jailRoot(mc.ID))
	if jailed {
		snapPath, memPath = "/snap.file", "/mem.file"
	}

	c := fc.New(sock)

	// El vaciado va ANTES de pausar, y no dentro de kill() como en los demás
	// caminos. Un invitado pausado no atiende HTTP: la petición se comía su
	// plazo entero en CADA congelación, y lo que quedara sin vaciar solo vivía
	// en mem.file — si luego se elimina la máquina warm, esas escrituras
	// desaparecen sin dejar rastro.
	m.flushVolume(mc)

	start := time.Now()
	if err := c.Pause(ctx); err != nil {
		return nil, err
	}
	if err := c.Snapshot(ctx, snapPath, memPath); err != nil {
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
		root := m.jailRoot(mc.ID)
		for _, f := range []string{"snap.file", "mem.file"} {
			if err := os.Rename(filepath.Join(root, f), filepath.Join(dir, f)); err != nil {
				return nil, fmt.Errorf("recovering %s from jail: %w", f, err)
			}
		}
		snapPath, memPath = filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
	}

	// Con el snapshot en disco el proceso sobra: aquí es donde se libera la RAM.
	//
	// killPaused y no kill: el invitado lleva pausado desde antes del snapshot y
	// no puede contestar. Los volúmenes ya se vaciaron arriba, con la máquina
	// aún corriendo, que era el único momento posible.
	m.killPaused(mc.ID)
	// Y su namespace y su cgroup tampoco hacen nada mientras está congelada.
	// Thaw los recrea.
	knet.Plan(mc.NetIndex, mc.ID).Teardown()
	m.releaseCPU(mc.ID)

	// Firecracker vuelca la memoria entera, pero en una microVM recién arrancada
	// la mayor parte son páginas a cero. Perforarlas deja el fichero disperso: el
	// kernel devuelve ceros al leer un agujero, que es exactamente lo que había,
	// así que la restauración no se entera. Mide ~3x menos en disco.
	if out, err := exec.CommandContext(ctx, "fallocate", "--dig-holes", memPath).CombinedOutput(); err != nil {
		log.Printf("warning: could not punch holes in %s: %v: %s", memPath, err, out)
	}

	// El fichero de memoria queda entero en caché tras escribirlo y releerlo para
	// perforarlo, y no se volverá a tocar hasta que alguien descongele ESTA
	// máquina. Se suelta: es la principal fuente de caché acumulada del host.
	dropCache(memPath)

	size := allocatedBytes(memPath) + allocatedBytes(snapPath)

	m.mu.Lock()
	live := m.byID[mc.ID]
	now := time.Now()
	live.State = api.StateWarm
	live.FrozenAt = &now
	live.FreezeMS = elapsed
	live.SnapSize = size
	live.PID = 0
	delete(m.socket, mc.ID)
	m.persist()
	out := *live
	m.mu.Unlock()

	out.DiskBytes = m.touchDisk(mc.ID)

	m.bus.Publish(api.Event{Time: now, Type: api.EvFrozen, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("frozen in %d ms (%d MiB on disk)", elapsed, size>>20)})
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
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()

	// Pudo cambiar de estado mientras esperábamos el lock.
	cur, ok := m.Get(mc.ID)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	if cur.State != api.StateRunning {
		return nil, fmt.Errorf("only a running machine can be squeezed (it is %s)", cur.State)
	}

	m.mu.RLock()
	sock := m.socket[mc.ID]
	pid := cur.PID
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}
	c := fc.New(sock)

	stats, err := c.BalloonStats(ctx)
	if err != nil {
		return nil, fmt.Errorf("the balloon is not available on this instance "+
			"(reimport the image to record it in the snapshot): %w", err)
	}
	freeMiB := int(stats.FreeMemory >> 20)
	rssBefore := procRSSMiB(pid)

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
	if target <= stats.ActualMiB {
		// El invitado no tiene holgura que reclamar.
		return &api.SqueezeResult{ID: mc.ID, ReclaimedMiB: 0, GuestFreeMiB: freeMiB, RSSMiB: rssBefore}, nil
	}

	if err := c.PatchBalloon(ctx, target); err != nil {
		return nil, fmt.Errorf("inflating the balloon: %w", err)
	}
	// El inflado es asíncrono: el driver del invitado va entregando páginas.
	// Esperamos a que se acerque al objetivo (o a un plazo corto) antes de medir.
	waitBalloon(ctx, c, target)
	// Desinflar: la RAM ya se reclamó al inflar; esto solo devuelve el presupuesto
	// al invitado. Con contexto sin cancelar para que no se quede inflado si el
	// cliente abandonó.
	if err := c.PatchBalloon(context.WithoutCancel(ctx), 0); err != nil {
		log.Printf("warning: could not deflate the balloon for %s: %v", mc.ID, err)
	}

	rssAfter := procRSSMiB(pid)
	reclaimed := rssBefore - rssAfter
	if reclaimed < 0 {
		reclaimed = 0
	}

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvFrozen, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("squeezed: ~%d MiB returned to host (RSS %d→%d MiB)", reclaimed, rssBefore, rssAfter)})

	return &api.SqueezeResult{ID: mc.ID, ReclaimedMiB: reclaimed, GuestFreeMiB: freeMiB, RSSMiB: rssAfter}, nil
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

// PutMMDS inyecta el store MMDS en una microVM VIVA: es la entrega del secreto de
// sesión (un token, una credencial) a una instancia ya arrancada, sin haberlo
// horneado en la imagen compartida ni en el snapshot dorado.
//
// Resuelve la máquina, coge su socket de Firecracker —igual que Squeeze— y hace
// PUT /mmds con el documento JSON tal cual. El esquema del store lo entiende el
// bridge de dentro (objeto con "env" comunes y "sessions" por Mcp-Session-Id).
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

	c := fc.New(sock)
	if err := c.PutMMDSData(ctx, data); err != nil {
		return nil, fmt.Errorf("injecting MMDS: %w", err)
	}

	m.mu.Lock()
	live := m.byID[mc.ID]
	if live == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("machine %q no longer exists", ref)
	}
	live.HasSecrets = true
	m.persist()
	out := *live
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStarted, ID: mc.ID, Name: mc.Name,
		Message: "session secrets injected via MMDS (can no longer be frozen)"})
	return &out, nil
}

// Thaw restaura una máquina warm. Es la operación rápida del proyecto.
func (m *Manager) Thaw(ctx context.Context, ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()

	// Otra llamada pudo descongelarla mientras esperábamos el candado.
	if cur, ok := m.Get(mc.ID); ok && cur.State == api.StateRunning {
		return cur, nil
	}
	if mc.State != api.StateWarm {
		return nil, fmt.Errorf("only a warm machine can be thawed (it is %s)", mc.State)
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
	if live := m.liveVMs(); live[mc.ID] > 0 {
		pid := live[mc.ID]
		log.Printf("thaw: %s (%s) was already running (pid %d); re-adopting it instead of starting another",
			mc.Name, mc.ID[:8], pid)
		m.mu.Lock()
		if cur := m.byID[mc.ID]; cur != nil {
			now := time.Now()
			cur.State = api.StateRunning
			cur.PID = pid
			cur.StartedAt = &now
			cur.FrozenAt = nil
			m.socket[mc.ID] = sock
			m.persist()
			out := *cur
			m.mu.Unlock()
			return &out, nil
		}
		m.mu.Unlock()
	}

	// Puerta de arranque: descongelar es cargar un snapshot en KVM —mapear su
	// memoria y reanudar los vCPU—, tan intensivo como un arranque en frío. Es,
	// además, la mayoría del ciclo, así que es aquí donde una tormenta simultánea
	// hace más daño. Va tras la readopción de arriba: readoptar no enciende nada,
	// no tiene por qué esperar turno.
	release, glErr := m.enterLaunch(ctx)
	if glErr != nil {
		return nil, glErr
	}
	defer release()

	_ = os.Remove(sock)

	// El namespace pudo desaparecer con un reinicio del host; lo rehacemos con el
	// mismo índice para que la máquina conserve su IP.
	egress, _ := knet.ParseEgress(mc.Egress)
	netcfg := knet.Plan(mc.NetIndex, mc.ID)
	if err := netcfg.Setup(egress, mc.AllowDomains, m.priv.UID); err != nil {
		return nil, fmt.Errorf("rebuilding the network: %w", err)
	}
	var pid int
	var c *fc.Client
	var err error
	if jailerEnabled() {
		// El warm también se descongela dentro de un jail, o el aislamiento se
		// perdería justo en las descongelaciones —que son la mayoría del ciclo—.
		// Mismo patrón que runFrom: poblar el chroot antes de cargar.
		pid, sock, err = m.spawnJailed(mc.ID, netcfg)
		if err != nil {
			return nil, err
		}
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			return nil, err
		}
		toLink := []string{snapPath, memPath, m.imagePath(mc.Image), filepath.Join(dir, "overlay.ext4")}
		for _, v := range mc.Volumes {
			toLink = append(toLink, m.volumePath(v.Name))
		}
		if err := m.prepareJail(mc.ID, toLink...); err != nil {
			return nil, err
		}
	} else {
		pid, err = m.spawn(mc.ID, sock, netcfg)
		if err != nil {
			return nil, err
		}
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			return nil, err
		}
	}

	start := time.Now()
	if err := c.LoadSnapshot(ctx, snapPath, memPath, true); err != nil {
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()

	// Reaplicar el techo de CPU: el firecracker de una máquina descongelada es un
	// proceso NUEVO (spawn), así que su pertenencia al cgroup no sobrevive al ciclo
	// freeze→thaw. Sin esto una microVM descongelada corría en el cgroup del daemon,
	// SIN límite —hueco de aislamiento— y, además, se saltaba el cpu_pct que viaja
	// con el snapshot justo en el camino de thaw, que es el habitual del gateway.
	// Mismo patrón que Run (boot) y runFrom.
	if mc.CPUPct <= 0 {
		mc.CPUPct = defaultCPUPct
	}
	if warn := m.limitCPU(mc.ID, pid, mc.CPUPct); warn != "" {
		log.Printf("warning: %s: %s", mc.Name, warn)
	}

	m.mu.Lock()
	live := m.byID[mc.ID]
	now := time.Now()
	live.State = api.StateRunning
	live.StartedAt = &now
	live.FrozenAt = nil
	live.ThawMS = elapsed
	live.PID = pid
	m.socket[mc.ID] = sock
	m.persist()
	out := *live
	m.mu.Unlock()

	out.DiskBytes = m.touchDisk(mc.ID)

	m.bus.Publish(api.Event{Time: now, Type: api.EvThawed, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("thawed in %d ms", elapsed)})
	return &out, nil
}

// SetLabels reetiqueta una máquina viva.
func (m *Manager) SetLabels(ref string, labels map[string]string) error {
	mc, ok := m.Get(ref)
	if !ok {
		return fmt.Errorf("machine %q doesn't exist", ref)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	live := m.byID[mc.ID]
	if live == nil {
		// La eliminaron entre el Get y el candado. Etiquetar algo que ya no
		// existe no es un error del que llama, pero deref nil sí tumbaba el
		// daemon entero.
		return fmt.Errorf("machine %q no longer exists", ref)
	}
	live.Labels = api.MergeLabels(live.Labels, labels)
	m.persist()
	return nil
}

// Stop termina la microVM sin borrar su directorio.
func (m *Manager) Stop(ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q doesn't exist", ref)
	}
	m.kill(mc.ID)
	// Una máquina parada no necesita namespace ni cgroup: se recrean al arrancar.
	knet.Plan(mc.NetIndex, mc.ID).Teardown()
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
	delete(m.socket, mc.ID)
	m.persist()
	out := *live
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStopped, ID: mc.ID, Name: mc.Name})
	return &out, nil
}

// Remove para la máquina y borra su directorio, snapshot incluido.
func (m *Manager) Remove(ref string) error {
	mc, ok := m.Get(ref)
	if !ok {
		return fmt.Errorf("machine %q doesn't exist", ref)
	}
	defer m.lock(mc.ID)()
	m.kill(mc.ID)
	knet.Plan(mc.NetIndex, mc.ID).Teardown()
	m.releaseCPU(mc.ID)
	m.lifecycle.Delete(mc.ID)
	// El chroot del jail vive aparte del directorio de la máquina: se limpia
	// también, o cada restauración jailed deja un árbol huérfano.
	_ = os.RemoveAll(filepath.Join(m.jailBase(), "firecracker", mc.ID))
	if err := os.RemoveAll(m.dir(mc.ID)); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.byID, mc.ID)
	delete(m.socket, mc.ID)
	m.persist()
	m.mu.Unlock()
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
	m.mu.RLock()
	mc := m.byID[id]
	m.mu.RUnlock()
	if mc == nil || mc.PID == 0 {
		return
	}
	if flush {
		m.flushVolume(mc)
	}
	_ = syscall.Kill(mc.PID, syscall.SIGKILL)

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
	waitGone(mc.PID, 5*time.Second)
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

func (m *Manager) fail(mc *api.Machine, err error) {
	m.kill(mc.ID)
	knet.Plan(mc.NetIndex, mc.ID).Teardown()
	m.releaseCPU(mc.ID)
	m.mu.Lock()
	mc.State = api.StateFailed
	mc.LastErr = err.Error()
	m.persist()
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvFailed, ID: mc.ID, Name: mc.Name, Message: err.Error()})
}
