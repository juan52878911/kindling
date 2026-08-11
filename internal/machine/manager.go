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
// mismo snapshot dorado sirve con volúmenes distintos, o sin ninguno.
func bootArgs(volMount string) string {
	return bootArgsBase + " " + knet.BootArg() + volumeBootArg(volMount)
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

	// netCursor rota los índices de red en vez de reutilizar el menor libre.
	// Ver allocNetIndex.
	netCursor int

	// templateMu serializa la construcción de la plantilla de overlay: dos
	// arranques a la vez sobre un host limpio la formatearían por duplicado.
	templateMu sync.Mutex

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
		byID:   make(map[string]*api.Machine),
		socket: make(map[string]string),
		wake:   make(chan struct{}, 1),
		quit:   make(chan struct{}),
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
		log.Printf("estado: no pude serializarlo: %v", err)
		return
	}

	tmp := m.statePath() + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		log.Printf("estado: no pude escribir %s: %v", tmp, err)
		return
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		log.Printf("estado: escritura incompleta: %v", err)
		return
	}
	// fsync del fichero Y del directorio. Sin el segundo, el rename puede no
	// haber llegado al disco cuando se corta la luz, y el daemon arrancaría
	// leyendo el estado anterior — que es peor que no leer ninguno, porque se
	// da por bueno.
	if err := f.Sync(); err != nil {
		log.Printf("estado: fsync falló: %v", err)
	}
	if err := f.Close(); err != nil {
		log.Printf("estado: cierre falló: %v", err)
		return
	}
	if err := os.Rename(tmp, m.statePath()); err != nil {
		log.Printf("estado: rename falló: %v", err)
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
		return nil, fmt.Errorf("límite de %d máquinas alcanzado (hay %d)", MaxMachines, n)
	}

	volPath, volMount, err := m.resolveVolume(req)
	if err != nil {
		return nil, err
	}

	src := m.imagePath(req.Image)
	if _, err := os.Stat(src); err != nil {
		return nil, fmt.Errorf("no encuentro la imagen %q en %s", req.Image, src)
	}
	if _, err := os.Stat(m.KernelPath()); err != nil {
		return nil, fmt.Errorf("falta el kernel en %s", m.KernelPath())
	}

	id := newID()
	if req.Name == "" {
		req.Name = req.Image + "-" + id[:6]
	}
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

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
		Volume: req.Volume, VolumeMount: volMount,
	}
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvCreated, ID: id, Name: mc.Name})

	egress, err := knet.ParseEgress(req.Egress)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	netcfg := knet.Plan(m.allocNetIndex(), id)
	if err := netcfg.Setup(egress, m.priv.UID); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("montando la red: %w", err)
	}
	mc.IP, mc.NetIndex, mc.Egress = netcfg.NSIP, netcfg.Index, string(egress)

	// El VMM solo puede escribir en lo suyo: su directorio y su overlay.
	if err := m.priv.Own(dir, overlay); err != nil {
		netcfg.Teardown()
		os.RemoveAll(dir)
		return nil, err
	}

	start := time.Now()
	pid, err := m.boot(ctx, mc.ID, mc.VCPUs, mc.MemMiB, src, overlay, netcfg, volPath, volMount)
	if err != nil {
		netcfg.Teardown()
		m.fail(mc, err)
		return nil, err
	}
	if mc.CPUPct <= 0 {
		mc.CPUPct = defaultCPUPct
	}
	if warn := m.limitCPU(mc.ID, pid, mc.CPUPct); warn != "" {
		log.Printf("aviso: %s: %s", mc.Name, warn)
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
		Message: fmt.Sprintf("arrancada en frío en %d ms", out.BootMS)})
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
		log.Printf("plantilla de overlay no disponible (%v): formateo directo", err)
		return createOverlay(ctx, dst, defaultOverlayMiB)
	}
	// --sparse=always: el overlay es disperso y copiarlo denso destruiría lo
	// que hace que una máquina cueste ~8 MB en vez de 512.
	out, err := exec.CommandContext(ctx, "cp", "--sparse=always", m.overlayTemplatePath(), dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("copiando la plantilla de overlay: %v: %s", err, out)
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
		return fmt.Errorf("formateando el overlay: %v: %s", err, out)
	}
	return nil
}

// boot lanza el proceso firecracker y configura la microVM por su API.
//
// Dos discos: la imagen base compartida en solo lectura y el overlay propio.
//
// Devuelve el PID en vez de escribirlo en la estructura: quien llama lo asigna
// bajo el mutex. Escribirlo aquí sería una carrera con List(), que copia las
// máquinas concurrentemente.
func (m *Manager) boot(ctx context.Context, id string, vcpus, memMiB int, base, overlay string, n *knet.Net, volPath, volMount string) (int, error) {
	sock := filepath.Join(m.dir(id), "fc.sock")
	_ = os.Remove(sock)

	pid, err := m.spawn(id, sock, n)
	if err != nil {
		return 0, err
	}

	c := fc.New(sock)
	if err := waitSocket(ctx, c); err != nil {
		return pid, err
	}
	if err := c.SetBootSource(ctx, fc.BootSource{KernelImagePath: m.KernelPath(), BootArgs: bootArgs(volMount)}); err != nil {
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
	// vdc: volumen persistente, si se pidió. Es lo único que SOBREVIVE a la
	// máquina. El host no lo monta mientras esté aquí: dos sistemas escribiendo
	// el mismo ext4 es corrupción segura.
	if volPath != "" {
		if err := c.SetDrive(ctx, fc.Drive{
			DriveID: "volume", PathOnHost: volPath, IsRootDevice: false, IsReadOnly: false,
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
	}
	// virtio-rng: sin esto, las instancias de un mismo snapshot clonarían el
	// estado del generador de aleatoriedad y podrían producir las mismas claves.
	if err := c.SetEntropy(ctx); err != nil {
		return pid, fmt.Errorf("añadiendo entropía: %w", err)
	}
	if err := c.SetMachineConfig(ctx, fc.MachineConfig{VCPUCount: vcpus, MemSizeMiB: memMiB}); err != nil {
		return pid, err
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
		return 0, fmt.Errorf("lanzando firecracker: %w", err)
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
	return fmt.Errorf("el socket de firecracker no respondió en 5s")
}

// Freeze pausa la microVM, la vuelca a disco y libera su RAM y su proceso.
func (m *Manager) Freeze(ctx context.Context, ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("no existe la máquina %q", ref)
	}
	defer m.lock(mc.ID)()

	// Pudo congelarla otro mientras esperábamos.
	if cur, ok := m.Get(mc.ID); ok && cur.State == api.StateWarm {
		return cur, nil
	}
	if mc.State != api.StateRunning {
		return nil, fmt.Errorf("solo se puede congelar una máquina running (está %s)", mc.State)
	}

	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("sin socket para %s", mc.ID)
	}

	dir := m.dir(mc.ID)
	snapPath, memPath := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")

	c := fc.New(sock)
	start := time.Now()
	if err := c.Pause(ctx); err != nil {
		return nil, err
	}
	if err := c.Snapshot(ctx, snapPath, memPath); err != nil {
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()

	// Con el snapshot en disco el proceso sobra: aquí es donde se libera la RAM.
	m.kill(mc.ID)
	// Y su namespace y su cgroup tampoco hacen nada mientras está congelada.
	// Thaw los recrea.
	knet.Plan(mc.NetIndex, mc.ID).Teardown()
	m.releaseCPU(mc.ID)

	// Firecracker vuelca la memoria entera, pero en una microVM recién arrancada
	// la mayor parte son páginas a cero. Perforarlas deja el fichero disperso: el
	// kernel devuelve ceros al leer un agujero, que es exactamente lo que había,
	// así que la restauración no se entera. Mide ~3x menos en disco.
	if out, err := exec.CommandContext(ctx, "fallocate", "--dig-holes", memPath).CombinedOutput(); err != nil {
		log.Printf("aviso: no pude perforar %s: %v: %s", memPath, err, out)
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
		Message: fmt.Sprintf("congelada en %d ms (%d MiB en disco)", elapsed, size>>20)})
	return &out, nil
}

// Thaw restaura una máquina warm. Es la operación rápida del proyecto.
func (m *Manager) Thaw(ctx context.Context, ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("no existe la máquina %q", ref)
	}
	defer m.lock(mc.ID)()

	// Otra llamada pudo descongelarla mientras esperábamos el candado.
	if cur, ok := m.Get(mc.ID); ok && cur.State == api.StateRunning {
		return cur, nil
	}
	if mc.State != api.StateWarm {
		return nil, fmt.Errorf("solo se puede descongelar una máquina warm (está %s)", mc.State)
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
		log.Printf("thaw: %s (%s) ya estaba corriendo (pid %d); la readopto en vez de arrancar otra",
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

	_ = os.Remove(sock)

	// El namespace pudo desaparecer con un reinicio del host; lo rehacemos con el
	// mismo índice para que la máquina conserve su IP.
	egress, _ := knet.ParseEgress(mc.Egress)
	netcfg := knet.Plan(mc.NetIndex, mc.ID)
	if err := netcfg.Setup(egress, m.priv.UID); err != nil {
		return nil, fmt.Errorf("rehaciendo la red: %w", err)
	}
	pid, err := m.spawn(mc.ID, sock, netcfg)
	if err != nil {
		return nil, err
	}
	c := fc.New(sock)
	if err := waitSocket(ctx, c); err != nil {
		return nil, err
	}

	start := time.Now()
	if err := c.LoadSnapshot(ctx, snapPath, memPath, true); err != nil {
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()

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
		Message: fmt.Sprintf("descongelada en %d ms", elapsed)})
	return &out, nil
}

// SetLabels reetiqueta una máquina viva.
func (m *Manager) SetLabels(ref string, labels map[string]string) error {
	mc, ok := m.Get(ref)
	if !ok {
		return fmt.Errorf("no existe la máquina %q", ref)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[mc.ID].Labels = api.MergeLabels(m.byID[mc.ID].Labels, labels)
	m.persist()
	return nil
}

// Stop termina la microVM sin borrar su directorio.
func (m *Manager) Stop(ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("no existe la máquina %q", ref)
	}
	m.kill(mc.ID)
	// Una máquina parada no necesita namespace ni cgroup: se recrean al arrancar.
	knet.Plan(mc.NetIndex, mc.ID).Teardown()
	m.releaseCPU(mc.ID)

	m.mu.Lock()
	live := m.byID[mc.ID]
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
		return fmt.Errorf("no existe la máquina %q", ref)
	}
	defer m.lock(mc.ID)()
	m.kill(mc.ID)
	knet.Plan(mc.NetIndex, mc.ID).Teardown()
	m.releaseCPU(mc.ID)
	m.lifecycle.Delete(mc.ID)
	if err := os.RemoveAll(m.dir(mc.ID)); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.byID, mc.ID)
	delete(m.socket, mc.ID)
	m.persist()
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvStopped, ID: mc.ID, Name: mc.Name, Message: "eliminada"})
	return nil
}

func (m *Manager) kill(id string) {
	m.mu.RLock()
	mc := m.byID[id]
	m.mu.RUnlock()
	if mc == nil || mc.PID == 0 {
		return
	}
	_ = syscall.Kill(mc.PID, syscall.SIGKILL)
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
