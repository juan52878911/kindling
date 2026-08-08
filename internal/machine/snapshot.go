package machine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/fc"
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

func (m *Manager) snapDir(name string) string {
	return filepath.Join(m.root, "snapshots", name)
}

// Commit congela una máquina en marcha como snapshot dorado reutilizable, y la
// deja corriendo.
//
// El snapshot guarda tres cosas: el estado de la VM, su memoria, y una copia del
// overlay tal y como estaba en ese instante. Las tres son necesarias: al
// restaurar, el invitado despierta con su estado de montaje en memoria, así que
// el disco que le demos debe tener exactamente el contenido que tenía al
// congelarse.
func (m *Manager) Commit(ctx context.Context, ref, name string) (*api.Snapshot, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("nombre de snapshot inválido: %q", name)
	}
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("no existe la máquina %q", ref)
	}
	if mc.State != api.StateRunning {
		return nil, fmt.Errorf("solo se puede commitear una máquina running (está %s)", mc.State)
	}

	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("sin socket para %s", mc.ID)
	}

	dir := m.snapDir(name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("el snapshot %q ya existe", name)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	snapPath := filepath.Join(dir, "snap.file")
	memPath := filepath.Join(dir, "mem.file")
	goldOverlay := filepath.Join(dir, "overlay.ext4")

	c := fc.New(sock)
	if err := c.Pause(ctx); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := c.Snapshot(ctx, snapPath, memPath); err != nil {
		os.RemoveAll(dir)
		_ = c.Resume(ctx)
		return nil, err
	}
	// El overlay se copia con la máquina pausada, para que sea coherente con la
	// memoria que acabamos de volcar.
	if out, err := exec.CommandContext(ctx, "cp", "--sparse=always",
		filepath.Join(m.dir(mc.ID), "overlay.ext4"), goldOverlay).CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		_ = c.Resume(ctx)
		return nil, fmt.Errorf("copiando el overlay: %v: %s", err, out)
	}
	// La máquina origen sigue su vida: commit no la mata.
	if err := c.Resume(ctx); err != nil {
		return nil, err
	}

	if out, err := exec.CommandContext(ctx, "fallocate", "--dig-holes", memPath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("perforando el fichero de memoria: %v: %s", err, out)
	}

	snap := &api.Snapshot{
		Name: name, Image: mc.Image, CreatedAt: time.Now(),
		VCPUs: mc.VCPUs, MemMiB: mc.MemMiB,
		MemBytes:  allocatedBytes(memPath),
		DiskBytes: diskUsage(dir),
	}
	b, _ := json.MarshalIndent(snap, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), b, 0o644); err != nil {
		return nil, err
	}

	m.bus.Publish(api.Event{Time: time.Now(), Type: api.EvCommitted, ID: mc.ID, Name: name,
		Message: fmt.Sprintf("snapshot dorado desde %s (%d MiB)", mc.Name, snap.MemBytes>>20)})
	return snap, nil
}

// Snapshots lista los snapshots dorados y cuántas instancias vivas tiene cada uno.
func (m *Manager) Snapshots() []*api.Snapshot {
	entries, err := os.ReadDir(filepath.Join(m.root, "snapshots"))
	if err != nil {
		return nil
	}

	live := map[string]int{}
	m.mu.RLock()
	for _, mc := range m.byID {
		if mc.From != "" && mc.State == api.StateRunning {
			live[mc.From]++
		}
	}
	m.mu.RUnlock()

	out := make([]*api.Snapshot, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := m.loadSnapshot(e.Name())
		if err != nil {
			continue
		}
		s.DiskBytes = diskUsage(m.snapDir(e.Name()))
		s.Instances = live[e.Name()]
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *Manager) loadSnapshot(name string) (*api.Snapshot, error) {
	b, err := os.ReadFile(filepath.Join(m.snapDir(name), "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("no existe el snapshot %q", name)
	}
	var s api.Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// RemoveSnapshot borra un snapshot dorado, salvo que tenga instancias vivas.
func (m *Manager) RemoveSnapshot(name string) error {
	if _, err := m.loadSnapshot(name); err != nil {
		return err
	}
	m.mu.RLock()
	var users []string
	for _, mc := range m.byID {
		if mc.From == name && mc.State != api.StateStopped {
			users = append(users, mc.Name)
		}
	}
	m.mu.RUnlock()
	if len(users) > 0 {
		return fmt.Errorf("el snapshot %q tiene %d instancias vivas (%v)", name, len(users), users)
	}
	return os.RemoveAll(m.snapDir(name))
}

// runFrom instancia una microVM desde un snapshot dorado.
//
// Todas las instancias mapean el MISMO fichero de memoria: Firecracker lo mapea
// en privado, así que comparten las páginas que no escriben y solo divergen las
// que tocan. La segunda instancia y las siguientes salen casi gratis en RAM.
func (m *Manager) runFrom(ctx context.Context, req api.RunRequest) (*api.Machine, error) {
	snap, err := m.loadSnapshot(req.From)
	if err != nil {
		return nil, err
	}

	id := newID()
	if req.Name == "" {
		req.Name = req.From + "-" + id[:6]
	}
	dir := m.dir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	// Copia del overlay dorado: mismo contenido, fichero propio. Compartirlo
	// haría que las instancias se pisaran el disco entre ellas.
	overlay := filepath.Join(dir, "overlay.ext4")
	if out, err := exec.CommandContext(ctx, "cp", "--sparse=always",
		filepath.Join(m.snapDir(req.From), "overlay.ext4"), overlay).CombinedOutput(); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("copiando el overlay dorado: %v: %s", err, out)
	}

	mc := &api.Machine{
		ID: id, Name: req.Name, Image: snap.Image, From: req.From,
		State: api.StateCreated, VCPUs: snap.VCPUs, MemMiB: snap.MemMiB,
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()

	sock := filepath.Join(dir, "fc.sock")
	_ = os.Remove(sock)
	pid, err := m.spawn(id, sock)
	if err != nil {
		m.fail(mc, err)
		return nil, err
	}
	c := fc.New(sock)
	if err := waitSocket(ctx, c); err != nil {
		m.fail(mc, err)
		return nil, err
	}

	start := time.Now()
	// Pausada: hay que reapuntar el overlay antes de dejarla correr.
	if err := c.LoadSnapshot(ctx,
		filepath.Join(m.snapDir(req.From), "snap.file"),
		filepath.Join(m.snapDir(req.From), "mem.file"), false); err != nil {
		m.fail(mc, err)
		return nil, err
	}
	if err := c.PatchDrive(ctx, "overlay", overlay); err != nil {
		m.fail(mc, fmt.Errorf("reapuntando el overlay: %w", err))
		return nil, err
	}
	if err := c.Resume(ctx); err != nil {
		m.fail(mc, err)
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()

	m.mu.Lock()
	now := time.Now()
	mc.PID = pid
	mc.State = api.StateRunning
	mc.StartedAt = &now
	mc.ThawMS = elapsed
	m.socket[id] = sock
	m.persist()
	out := *mc
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: id, Name: mc.Name,
		Message: fmt.Sprintf("instanciada desde %s en %d ms", req.From, elapsed)})
	return &out, nil
}
