// Package machine implementa el ciclo de vida de las microVMs.
package machine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
)

const bootArgs = "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw"

// Manager gestiona todas las microVMs del daemon.
type Manager struct {
	root   string // /var/lib/kindling
	fcBin  string
	bus    *events.Bus
	mu     sync.RWMutex
	byID   map[string]*api.Machine
	socket map[string]string // id -> ruta del socket de firecracker
}

func NewManager(root, fcBin string, bus *events.Bus) (*Manager, error) {
	for _, d := range []string{root, filepath.Join(root, "machines"), filepath.Join(root, "images")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	m := &Manager{
		root: root, fcBin: fcBin, bus: bus,
		byID:   make(map[string]*api.Machine),
		socket: make(map[string]string),
	}
	m.load()
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
	for _, mc := range list {
		// Un proceso en marcha no sobrevive al daemon; solo warm y stopped son
		// estados fiables tras un reinicio.
		if mc.State == api.StateRunning {
			mc.State = api.StateStopped
			mc.PID = 0
		}
		m.byID[mc.ID] = mc
	}
}

func (m *Manager) persist() {
	list := make([]*api.Machine, 0, len(m.byID))
	for _, mc := range m.byID {
		list = append(list, mc)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := m.statePath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, m.statePath())
	}
}

// ── consultas ─────────────────────────────────────────────────────────────────

func (m *Manager) List() []*api.Machine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*api.Machine, 0, len(m.byID))
	for _, mc := range m.byID {
		c := *mc
		out = append(out, &c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
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

// ── ciclo de vida ─────────────────────────────────────────────────────────────

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Run crea una microVM y la arranca en frío.
func (m *Manager) Run(ctx context.Context, req api.RunRequest) (*api.Machine, error) {
	if req.Image == "" {
		req.Image = "default"
	}
	if req.VCPUs <= 0 {
		req.VCPUs = 1
	}
	if req.MemMiB <= 0 {
		req.MemMiB = 256
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

	// Sin capas: cada microVM necesita su propio rootfs escribible. --reflink=auto
	// usa copia-al-escribir si el filesystem la soporta, y copia entera si no.
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if out, err := exec.CommandContext(ctx, "cp", "--reflink=auto", src, rootfs).CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("copiando rootfs: %v: %s", err, out)
	}

	mc := &api.Machine{
		ID: id, Name: req.Name, Image: req.Image, State: api.StateCreated,
		VCPUs: req.VCPUs, MemMiB: req.MemMiB, CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvCreated, ID: id, Name: mc.Name})

	start := time.Now()
	if err := m.boot(ctx, mc, rootfs); err != nil {
		m.fail(mc, err)
		return nil, err
	}
	m.mu.Lock()
	now := time.Now()
	mc.State = api.StateRunning
	mc.StartedAt = &now
	mc.BootMS = time.Since(start).Milliseconds()
	m.persist()
	out := *mc
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: id, Name: mc.Name,
		Message: fmt.Sprintf("arrancada en frío en %d ms", out.BootMS)})
	return &out, nil
}

// boot lanza el proceso firecracker y configura la microVM por su API.
func (m *Manager) boot(ctx context.Context, mc *api.Machine, rootfs string) error {
	sock := filepath.Join(m.dir(mc.ID), "fc.sock")
	_ = os.Remove(sock)

	pid, err := m.spawn(mc.ID, sock)
	if err != nil {
		return err
	}
	mc.PID = pid

	c := fc.New(sock)
	if err := waitSocket(ctx, c); err != nil {
		return err
	}
	if err := c.SetBootSource(ctx, fc.BootSource{KernelImagePath: m.KernelPath(), BootArgs: bootArgs}); err != nil {
		return err
	}
	if err := c.SetDrive(ctx, fc.Drive{DriveID: "rootfs", PathOnHost: rootfs, IsRootDevice: true}); err != nil {
		return err
	}
	if err := c.SetMachineConfig(ctx, fc.MachineConfig{VCPUCount: mc.VCPUs, MemSizeMiB: mc.MemMiB}); err != nil {
		return err
	}
	if err := c.Start(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.socket[mc.ID] = sock
	m.mu.Unlock()
	return nil
}

// spawn arranca firecracker desacoplado del daemon y devuelve su PID.
func (m *Manager) spawn(id, sock string) (int, error) {
	logf, err := os.Create(filepath.Join(m.dir(id), "firecracker.log"))
	if err != nil {
		return 0, err
	}
	cmd := exec.Command(m.fcBin, "--api-sock", sock)
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

	var size int64
	if fi, err := os.Stat(memPath); err == nil {
		size = fi.Size()
	}

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
	if mc.State != api.StateWarm {
		return nil, fmt.Errorf("solo se puede descongelar una máquina warm (está %s)", mc.State)
	}

	dir := m.dir(mc.ID)
	snapPath, memPath := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
	sock := filepath.Join(dir, "fc.sock")
	_ = os.Remove(sock)

	pid, err := m.spawn(mc.ID, sock)
	if err != nil {
		return nil, err
	}
	c := fc.New(sock)
	if err := waitSocket(ctx, c); err != nil {
		return nil, err
	}

	start := time.Now()
	if err := c.LoadSnapshot(ctx, snapPath, memPath); err != nil {
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

	m.bus.Publish(api.Event{Time: now, Type: api.EvThawed, ID: mc.ID, Name: mc.Name,
		Message: fmt.Sprintf("descongelada en %d ms", elapsed)})
	return &out, nil
}

// Stop termina la microVM sin borrar su directorio.
func (m *Manager) Stop(ref string) (*api.Machine, error) {
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("no existe la máquina %q", ref)
	}
	m.kill(mc.ID)

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
	m.kill(mc.ID)
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
	m.mu.Lock()
	mc.State = api.StateFailed
	mc.LastErr = err.Error()
	m.persist()
	m.mu.Unlock()
	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvFailed, ID: mc.ID, Name: mc.Name, Message: err.Error()})
}
