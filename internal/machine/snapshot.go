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

	"log"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
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

	// Quien escribe el snapshot es Firecracker, que corre sin privilegios: el
	// directorio tiene que ser suyo antes de pedírselo.
	if err := m.priv.Own(dir); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	ownOverlay := filepath.Join(m.dir(mc.ID), "overlay.ext4")

	c := fc.Get(sock)
	if err := c.Pause(ctx); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	abort := func(err error) (*api.Snapshot, error) {
		os.RemoveAll(dir)
		_ = c.PatchDrive(ctx, "overlay", ownOverlay)
		_ = c.Resume(ctx)
		return nil, err
	}

	// El overlay se copia con la máquina pausada, para que sea coherente con la
	// memoria que se va a volcar.
	if out, err := exec.CommandContext(ctx, "cp", "--sparse=always",
		ownOverlay, goldOverlay).CombinedOutput(); err != nil {
		return abort(fmt.Errorf("copiando el overlay: %v: %s", err, out))
	}
	// La copia la crea el daemon (root) pero quien va a abrirla es el VMM, que
	// corre sin privilegios. Sin ceder el fichero, el reapuntado falla con
	// "Permission denied".
	if err := m.priv.Own(goldOverlay); err != nil {
		return abort(err)
	}

	// CLAVE: se reapunta el disco a la copia dorada ANTES de volcar, para que el
	// snapshot grabe esa ruta y no la de esta máquina.
	//
	// Sin esto, el snapshot queda atado al directorio de la plantilla: en cuanto
	// se elimina la plantilla, restaurar falla con "No such file or directory"
	// sobre un overlay que ya no existe. El snapshot dorado tiene que ser
	// autocontenido, porque su razón de ser es sobrevivir a la máquina que lo creó.
	if err := c.PatchDrive(ctx, "overlay", goldOverlay); err != nil {
		return abort(fmt.Errorf("reapuntando el overlay a la copia dorada: %w", err))
	}
	if err := c.Snapshot(ctx, snapPath, memPath); err != nil {
		return abort(err)
	}
	// Se devuelve el disco propio: la plantilla sigue viva y no debe escribir en
	// el overlay dorado, que a partir de ahora es plantilla de otras instancias.
	if err := c.PatchDrive(ctx, "overlay", ownOverlay); err != nil {
		return nil, fmt.Errorf("devolviendo el overlay a la máquina: %w", err)
	}
	if err := c.Resume(ctx); err != nil {
		return nil, err
	}

	if out, err := exec.CommandContext(ctx, "fallocate", "--dig-holes", memPath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("perforando el fichero de memoria: %v: %s", err, out)
	}

	snap := &api.Snapshot{
		Name: name, Image: mc.Image, CreatedAt: time.Now(),
		VCPUs: mc.VCPUs, MemMiB: mc.MemMiB, Labels: mc.Labels,
		MemBytes:  allocatedBytes(memPath),
		DiskBytes: diskUsage(dir),
	}
	m.priv.EnsureReadable(dir)

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
		// DiskBytes se lee de meta.json — se calculó al hacer Commit().
		// Recalcularlo en cada Snapshots() era un filepath.WalkDir por snapshot.
		s.Instances = live[e.Name()]
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out
}

func (m *Manager) loadSnapshot(name string) (*api.Snapshot, error) {
	// El nombre llega de la URL. Sin validarlo, un "../../etc" saldría del
	// directorio de datos: recorrido de rutas de manual.
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("nombre de snapshot inválido: %q", name)
	}
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

// SetCatalog guarda las capacidades declaradas por el servidor MCP.
//
// Se captura una sola vez, al importar el servicio, y a partir de ahí el
// inventario se sirve desde disco sin tocar ninguna microVM.
func (m *Manager) SetCatalog(name string, tools []api.ToolSpec) (*api.Snapshot, error) {
	snap, err := m.loadSnapshot(name)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	snap.Tools, snap.ToolsAt = tools, &now

	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(m.snapDir(name), "meta.json"), b, 0o644); err != nil {
		return nil, err
	}
	m.priv.EnsureReadable(m.snapDir(name))

	m.bus.Publish(api.Event{Time: now, Type: api.EvCommitted, Name: name,
		Message: fmt.Sprintf("catálogo actualizado: %d herramienta(s)", len(tools))})
	return snap, nil
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

	// Namespace propio, pero con tap0 y la misma IP interna que tenía la máquina
	// al congelarse: es lo que permite que un solo snapshot sirva para N copias.
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
	if err := m.priv.Own(dir, overlay); err != nil {
		netcfg.Teardown()
		os.RemoveAll(dir)
		return nil, err
	}

	mc := &api.Machine{
		ID: id, Name: req.Name, Image: snap.Image, From: req.From,
		State: api.StateCreated, VCPUs: snap.VCPUs, MemMiB: snap.MemMiB,
		IP: netcfg.NSIP, NetIndex: netcfg.Index, Egress: string(egress),
		TTLSeconds: req.TTLSeconds, CPUPct: req.CPUPct,
		// Las etiquetas del snapshot se heredan; las de la petición mandan.
		Labels:    api.MergeLabels(snap.Labels, req.Labels),
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.byID[id] = mc
	m.schedulePersist()
	m.mu.Unlock()

	sock := filepath.Join(dir, "fc.sock")
	_ = os.Remove(sock)
	pid, err := m.spawn(id, sock, netcfg)
	if err != nil {
		netcfg.Teardown()
		m.fail(mc, err)
		return nil, err
	}
	c := fc.Get(sock)
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
	// Nada de SetEntropy aquí: tras cargar un snapshot no se pueden añadir
	// dispositivos. El virtio-rng ya viene dentro, porque la plantilla lo tenía
	// al congelarse; y CONFIG_VMGENID hace que el invitado resiembre su pool al
	// detectar que ha sido restaurado.
	if err := c.PatchDrive(ctx, "overlay", overlay); err != nil {
		m.fail(mc, fmt.Errorf("reapuntando el overlay: %w", err))
		return nil, err
	}
	if err := c.Resume(ctx); err != nil {
		m.fail(mc, err)
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()

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
	mc.ThawMS = elapsed
	m.socket[id] = sock
	m.schedulePersist()
	out := *mc
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: id, Name: mc.Name,
		Message: fmt.Sprintf("instanciada desde %s en %d ms", req.From, elapsed)})
	return &out, nil
}
