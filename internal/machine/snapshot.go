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

	c := fc.New(sock)

	// Los volúmenes se DESMONTAN antes de congelar, y con la máquina aún
	// corriendo: un invitado pausado no atiende HTTP.
	//
	// Sin esto, la memoria volcada lleva dentro la caché de ext4 —superbloque,
	// mapas de bloques, posición del journal— de un disco que después seguirá
	// cambiando, porque el fichero del volumen NO se copia al snapshot. Cada
	// instancia restaurada arrancaría creyendo un estado que ya no existe.
	if err := m.releaseVolumes(mc); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("preparando los volúmenes para congelar: %w", err)
	}

	if err := c.Pause(ctx); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	abort := func(err error) (*api.Snapshot, error) {
		os.RemoveAll(dir)
		_ = c.PatchDrive(ctx, "overlay", ownOverlay)
		_ = c.Resume(ctx)
		// La plantilla sigue viva y se quedó sin volúmenes al soltarlos: hay que
		// devolvérselos, o seguirá corriendo escribiendo en su overlay.
		_ = m.acquireVolumes(mc)
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
	// La plantilla vuelve a montarlos: sigue viva hasta que la importación la
	// destruya, y con -keep puede quedarse.
	if err := m.acquireVolumes(mc); err != nil {
		log.Printf("aviso: la plantilla %s se quedó sin sus volúmenes tras congelar: %v", mc.Name, err)
	}

	if out, err := exec.CommandContext(ctx, "fallocate", "--dig-holes", memPath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("perforando el fichero de memoria: %v: %s", err, out)
	}

	snap := &api.Snapshot{
		Name: name, Image: mc.Image, CreatedAt: time.Now(),
		VCPUs: mc.VCPUs, MemMiB: mc.MemMiB, Labels: mc.Labels,
		Egress: mc.Egress,
		// El volumen se graba en el snapshot porque el conjunto de discos de una
		// microVM queda FIJADO al congelarla: a una restaurada no se le puede
		// añadir un disco que no tuviera. Sin esto, el gateway despierta el
		// servicio sin volumen y la herramienta escribe en un overlay que muere
		// con la máquina — sin un solo error por ningún lado.
		Volumes:   mc.Volumes,
		MemBytes:  allocatedBytes(memPath),
		DiskBytes: diskUsage(dir),
	}
	m.priv.EnsureReadable(dir)

	b, _ := json.MarshalIndent(snap, "", "  ")
	if err := writeMeta(dir, b); err != nil {
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
	if err := writeMeta(m.snapDir(name), b); err != nil {
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

	// HERENCIA desde el snapshot. RunRequest ya promete que "el resto de campos
	// se heredan del snapshot", y la política de red no era una excepción: sin
	// esto, un servicio importado con -egress internet despertaba SIEMPRE sin
	// red, porque quien lo instancia —el gateway, el fondo, el modo efímero—
	// solo conoce el nombre del snapshot. El síntoma era un "fetch failed"
	// dentro del invitado que no señalaba a ninguna parte.
	//
	// Lo que venga en la petición manda; el snapshot solo rellena el hueco.
	if req.Egress == "" {
		req.Egress = snap.Egress
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
	// El volumen se resuelve ANTES de arrancar nada: si se pide uno sobre un
	// snapshot que no lo lleva, hay que decirlo aquí y no tras el arranque.
	// El volumen se HEREDA del snapshot, igual que la política de salida: quien
	// despierta un servicio no tiene por qué saber con qué volumen se importó,
	// y el gateway desde luego no lo sabe.
	if len(req.VolumeSet()) == 0 {
		req.Volumes = snap.VolumeSet()
	}
	vols, verr := m.resolveVolumes(req)
	if verr != nil {
		os.RemoveAll(dir)
		return nil, verr
	}
	for _, v := range vols {
		if !v.readOnly {
			repairVolume(ctx, v.path)
		}
	}
	// El CONJUNTO de discos quedó fijado al congelar: Firecracker no admite
	// añadir ni quitar discos a una VM restaurada. Solo se puede reapuntar cada
	// uno a otro fichero, así que el número tiene que coincidir exactamente.
	grabados := snap.VolumeSet()
	if n, quiere := len(grabados), len(vols); n != quiere {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("el servicio %q se importó con %d volumen(es) y se le están dando %d.\n"+
			"A una microVM restaurada no se le pueden añadir ni quitar discos: reimpórtalo si quieres cambiarlo",
			req.From, n, quiere)
	}
	// El MODO tampoco se puede cambiar, y esto es lo que impedía verlo.
	//
	// PatchDrive solo reapunta el fichero: is_read_only quedó fijado al congelar,
	// igual que el conjunto de discos. Pedir :ro sobre un snapshot que se congeló
	// en escritura no monta nada en solo lectura — monta en ESCRITURA y encima
	// engaña a la contabilidad, que lo apunta como lector y deja entrar a más.
	// El resultado son varios ext4 montados en escritura sobre el mismo fichero,
	// que es exactamente lo que el resto de este fichero existe para impedir.
	//
	// El punto de montaje va por el mismo camino: viaja en la línea de comandos
	// del kernel, que se congeló con la memoria.
	for i, v := range vols {
		g := grabados[i]
		if v.readOnly != g.ReadOnly {
			os.RemoveAll(dir)
			modo := func(ro bool) string {
				if ro {
					return "solo lectura"
				}
				return "escritura"
			}
			return nil, fmt.Errorf("el volumen %q se congeló en %s y se está pidiendo en %s.\n"+
				"El modo queda GRABADO en el snapshot y no se puede cambiar al restaurar: "+
				"reimporta el servicio si quieres el otro",
				v.name, modo(g.ReadOnly), modo(v.readOnly))
		}
		if v.mount != g.Mount {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("el volumen %q se congeló montado en %s y se está pidiendo en %s.\n"+
				"El punto de montaje viaja en la línea de comandos del kernel, que se congeló "+
				"con la memoria: reimporta el servicio si quieres moverlo",
				v.name, g.Mount, v.mount)
		}
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
		Volumes: attachments(vols),
		// Las etiquetas del snapshot se heredan; las de la petición mandan.
		Labels:    api.MergeLabels(snap.Labels, req.Labels),
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()

	sock := filepath.Join(dir, "fc.sock")
	_ = os.Remove(sock)
	pid, err := m.spawn(id, sock, netcfg)
	if err != nil {
		netcfg.Teardown()
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
	// Nada de SetEntropy aquí: tras cargar un snapshot no se pueden añadir
	// dispositivos. El virtio-rng ya viene dentro, porque la plantilla lo tenía
	// al congelarse; y CONFIG_VMGENID hace que el invitado resiembre su pool al
	// detectar que ha sido restaurado.
	if err := c.PatchDrive(ctx, "overlay", overlay); err != nil {
		m.fail(mc, fmt.Errorf("reapuntando el overlay: %w", err))
		return nil, err
	}
	// Cada volumen se reapunta igual que el overlay: el dispositivo ya existe
	// dentro del snapshot, y aquí solo se le dice a qué fichero del host mira.
	// Es lo que permite que el mismo snapshot dorado sirva a volúmenes distintos.
	//
	// El identificador tiene que ser el que quedó GRABADO al congelar, que en
	// los snapshots de una sola unidad era "volume" a secas. Equivocarlo deja a
	// la microVM mirando el fichero de otra máquina, o ninguno.
	// El nombre del disco se LEE del snapshot, no se deduce.
	//
	// Antes se deducía del meta.json (¿lista de volúmenes o campos sueltos?), y
	// eso rompía la cadena: el meta cambia en cada commit, el nombre del disco
	// dentro del VMM no. Restaurar de un snapshot legacy y volver a congelar esa
	// instancia producía un meta con lista sobre un VMM cuyo disco seguía
	// llamándose "volume" — y la siguiente restauración fallaba con drive not
	// found, de forma determinista y sin arreglo salvo reimportar.
	usados := make([]string, len(vols))
	for i, v := range vols {
		id, err := m.patchVolumeDrive(ctx, c, grabados[i], i, len(vols), v.path)
		if err != nil {
			m.fail(mc, fmt.Errorf("reapuntando el volumen %s: %w", v.name, err))
			return nil, err
		}
		usados[i] = id
	}
	// El nombre que de verdad funcionó se anota en la máquina, para que el
	// próximo commit lo escriba en su meta y la cadena se auto-repare: la
	// generación siguiente ya no necesita heurística ni reintento.
	m.mu.Lock()
	withDriveIDs(mc.Volumes, usados)
	m.mu.Unlock()
	if err := c.Resume(ctx); err != nil {
		m.fail(mc, err)
		return nil, err
	}
	// Y ahora que los discos apuntan a los ficheros de ESTA instancia, el
	// invitado los monta. Se congelaron desmontados a propósito, para que su
	// memoria no llevara dentro la caché de un ext4 que después cambia.
	//
	// Un fallo aquí es un fallo de la máquina: sin volumen, la herramienta
	// escribe en un directorio del overlay que muere con ella, y eso no da ni un
	// aviso hasta que alguien busca lo que guardó.
	if len(vols) > 0 {
		if err := m.acquireVolumes(mc); err != nil {
			m.fail(mc, err)
			return nil, err
		}
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
	m.persist()
	out := *mc
	m.mu.Unlock()

	m.bus.Publish(api.Event{Time: now, Type: api.EvStarted, ID: id, Name: mc.Name,
		Message: fmt.Sprintf("instanciada desde %s en %d ms", req.From, elapsed)})
	return &out, nil
}

// writeMeta guarda el meta.json de un snapshot de forma atómica.
//
// Al lado y renombrar, nunca encima. os.WriteFile TRUNCA primero: un corte a
// media escritura —o simplemente un lector concurrente, y Snapshots() se sirve
// en cada petición del gateway— deja un meta vacío o partido. El servicio
// "desaparece" del catálogo, y encima de forma PERMANENTE: RemoveSnapshot se
// niega a borrar lo que no puede leer, así que el directorio queda varado.
//
// El renombrado dentro de un mismo sistema de ficheros es atómico: o está el
// meta viejo o el nuevo. El proyecto ya usa este patrón en writePending y
// saveRecipe; aquí faltaba.
func writeMeta(dir string, b []byte) error {
	final := filepath.Join(dir, "meta.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// patchVolumeDrive reapunta un disco de volumen y devuelve el nombre que de
// verdad funcionó.
//
// Prefiere el grabado en el snapshot. Si no lo hay —snapshots anteriores al
// campo— cae en la heurística de siempre, y si esa falla con un solo volumen,
// reintenta con el nombre antiguo: eso cura los snapshots que la heurística ya
// dejó rotos, sin exigir reimportar el servicio.
func (m *Manager) patchVolumeDrive(ctx context.Context, c *fc.Client,
	grabado api.VolumeAttachment, i, total int, path string) (string, error) {

	id := grabado.DriveID
	if id == "" {
		id = volumeDriveID(i)
	}
	err := c.PatchDrive(ctx, id, path)
	if err == nil {
		return id, nil
	}
	// Con un solo volumen no hay ambigüedad posible: o el disco se llama
	// "volume0" o se llama "volume", nunca los dos. Así que reintentar no puede
	// reapuntar el disco de otro por error.
	if total == 1 && id != legacyVolumeDriveID {
		if err2 := c.PatchDrive(ctx, legacyVolumeDriveID, path); err2 == nil {
			log.Printf("el disco de este snapshot se llama %q y no %q (cadena antigua); "+
				"queda anotado para los siguientes", legacyVolumeDriveID, id)
			return legacyVolumeDriveID, nil
		}
	}
	return "", err
}

// withDriveIDs deja en los adjuntos el nombre de disco que REALMENTE funcionó,
// para que el próximo commit lo escriba en su meta y la cadena se auto-repare.
func withDriveIDs(vols []api.VolumeAttachment, ids []string) []api.VolumeAttachment {
	for i := range vols {
		if i < len(ids) && ids[i] != "" {
			vols[i].DriveID = ids[i]
		}
	}
	return vols
}
