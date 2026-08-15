package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
		return nil, fmt.Errorf("invalid snapshot name: %q", name)
	}
	mc, ok := m.Get(ref)
	if !ok {
		return nil, fmt.Errorf("machine %q does not exist", ref)
	}
	if mc.State != api.StateRunning {
		return nil, fmt.Errorf("only a running machine can be committed (is %s)", mc.State)
	}

	m.mu.RLock()
	sock := m.socket[mc.ID]
	m.mu.RUnlock()
	if sock == "" {
		return nil, fmt.Errorf("no socket for %s", mc.ID)
	}

	dir := m.snapDir(name)
	if _, err := os.Stat(dir); err == nil {
		return nil, fmt.Errorf("snapshot %q already exists", name)
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

	// Una plantilla jailed corre chrooteada: no ve snapDir. El overlay dorado y
	// el volcado se escriben en el jail (en su path absoluto) y se recuperan al
	// host después. goldDst es dónde se copia el overlay para que firecracker lo
	// abra; en el host es goldOverlay, en el jail su réplica dentro del chroot.
	jailed := jailerEnabled() && strings.HasPrefix(sock, m.jailRoot(mc.ID))
	goldDst := goldOverlay
	if jailed {
		goldDst = m.jailPath(mc.ID, goldOverlay)
		if err := os.MkdirAll(filepath.Dir(goldDst), 0o755); err != nil {
			os.RemoveAll(dir)
			return nil, err
		}
		if m.priv.Enabled {
			_ = os.Chown(filepath.Dir(goldDst), m.priv.UID, m.priv.GID)
		}
	}

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
		return nil, fmt.Errorf("preparing volumes for freeze: %w", err)
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
		ownOverlay, goldDst).CombinedOutput(); err != nil {
		return abort(fmt.Errorf("copying overlay: %v: %s", err, out))
	}
	// La copia la crea el daemon (root) pero quien va a abrirla es el VMM, que
	// corre sin privilegios. Sin ceder el fichero, el reapuntado falla con
	// "Permission denied".
	if err := m.priv.Own(goldDst); err != nil {
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
		return abort(fmt.Errorf("repointing overlay to golden copy: %w", err))
	}
	if err := c.Snapshot(ctx, snapPath, memPath); err != nil {
		return abort(err)
	}
	if jailed {
		// Recuperar del chroot al host: snapDir es donde runFrom los busca (y
		// los replica de vuelta en el próximo jail). Rename, mismo filesystem.
		for _, f := range []string{"snap.file", "mem.file", "overlay.ext4"} {
			if err := os.Rename(m.jailPath(mc.ID, filepath.Join(dir, f)), filepath.Join(dir, f)); err != nil {
				return abort(fmt.Errorf("recovering %s from jail: %w", f, err))
			}
		}
	}
	// Se devuelve el disco propio: la plantilla sigue viva y no debe escribir en
	// el overlay dorado, que a partir de ahora es plantilla de otras instancias.
	if err := c.PatchDrive(ctx, "overlay", ownOverlay); err != nil {
		return nil, fmt.Errorf("returning overlay to machine: %w", err)
	}
	if err := c.Resume(ctx); err != nil {
		return nil, err
	}
	// La plantilla vuelve a montarlos: sigue viva hasta que la importación la
	// destruya, y con -keep puede quedarse.
	if err := m.acquireVolumes(mc); err != nil {
		log.Printf("warning: template %s ended up without its volumes after freeze: %v", mc.Name, err)
	}

	if out, err := exec.CommandContext(ctx, "fallocate", "--dig-holes", memPath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("punching holes in memory file: %v: %s", err, out)
	}

	// Digests de integridad del rootfs dorado y del volcado de estado.
	//
	// Se calculan aquí, con los ficheros ya en su forma final (el overlay copiado,
	// el snap volcado, la memoria perforada). El mem.file se deja fuera a
	// propósito: es el grande y volver a leerlo entero en cada restauración mataría
	// los ~30 ms del thaw. Ver verifyIntegrity para el porqué completo.
	rootfsSHA, err := fileSHA256(goldOverlay)
	if err != nil {
		return nil, fmt.Errorf("computing digest of golden overlay: %w", err)
	}
	snapSHA, err := fileSHA256(snapPath)
	if err != nil {
		return nil, fmt.Errorf("computing digest of state dump: %w", err)
	}

	snap := &api.Snapshot{
		Name: name, Image: mc.Image, CreatedAt: time.Now(),
		VCPUs: mc.VCPUs, MemMiB: mc.MemMiB, Labels: mc.Labels,
		Egress:       mc.Egress,
		CPUPct:       mc.CPUPct,
		AllowDomains: mc.AllowDomains,
		RootfsSHA256: rootfsSHA,
		SnapSHA256:   snapSHA,
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
		Message: fmt.Sprintf("golden snapshot from %s (%d MiB)", mc.Name, snap.MemBytes>>20)})
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
		return nil, fmt.Errorf("invalid snapshot name: %q", name)
	}
	b, err := os.ReadFile(filepath.Join(m.snapDir(name), "meta.json"))
	if err != nil {
		return nil, fmt.Errorf("snapshot %q does not exist", name)
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
		Message: fmt.Sprintf("catalog updated: %d tool(s)", len(tools))})
	return snap, nil
}

// SetHealth anota en el meta del snapshot el resultado del último sondeo de
// salud. El sondeo real —arrancar una microVM efímera y pedirle tools/list— lo
// hace quien puede hablar con el invitado (el CLI, `kling mcp health`); el daemon
// solo persiste el veredicto, para que `mcp list` y /metrics lo puedan mostrar
// sin volver a despertar nada.
func (m *Manager) SetHealth(name string, healthy bool, probeErr string) (*api.Snapshot, error) {
	snap, err := m.loadSnapshot(name)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if healthy {
		snap.Health, snap.HealthErr = "healthy", ""
	} else {
		snap.Health, snap.HealthErr = "unhealthy", probeErr
	}
	snap.HealthAt = &now

	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeMeta(m.snapDir(name), b); err != nil {
		return nil, err
	}
	m.priv.EnsureReadable(m.snapDir(name))

	estado := "healthy"
	if !healthy {
		estado = "unhealthy"
	}
	m.bus.Publish(api.Event{Time: now, Type: api.EvCommitted, Name: name,
		Message: fmt.Sprintf("health probe: %s", estado)})
	return snap, nil
}

// verifyIntegrity comprueba que el rootfs dorado y el volcado de estado no se
// corrompieron desde que se congelaron.
//
// DECISIÓN — qué se hashea y qué no:
//
//	overlay.ext4 (rootfs) y snap.file  -> SÍ, en cada restauración.
//	mem.file                           -> NO aquí.
//
// El fichero de memoria es el grande —cientos de MiB— y restaurar promete ~30 ms;
// leerlo entero por sha256 en cada thaw tiraría esa cifra por tierra. El rootfs y
// el snap.file son pequeños, y encima el overlay ya se lee entero al copiarlo con
// `cp` justo después de esta comprobación: el sobrecoste real es una segunda
// pasada de lectura sobre unos pocos MiB, no sobre el volcado completo. La
// integridad del mem.file, si algún día se quiere, se comprobaría UNA vez tras
// reiniciar el daemon (fuera del camino caliente), no en cada arranque.
//
// Los snapshots anteriores a esta comprobación no tienen digests grabados: se
// saltan en vez de fallar, o reimportar dejaría de ser opcional para todos.
func (m *Manager) verifyIntegrity(snap *api.Snapshot, snapDir string) error {
	if snap.RootfsSHA256 == "" && snap.SnapSHA256 == "" {
		return nil // snapshot legacy: no hay digests que comprobar
	}
	for _, chk := range []struct{ file, want string }{
		{"overlay.ext4", snap.RootfsSHA256},
		{"snap.file", snap.SnapSHA256},
	} {
		if chk.want == "" {
			continue
		}
		got, err := fileSHA256(filepath.Join(snapDir, chk.file))
		if err != nil {
			return fmt.Errorf("couldn't read %s to verify its integrity: %w", chk.file, err)
		}
		if got != chk.want {
			return fmt.Errorf("snapshot %q is corrupt: %s doesn't match what was frozen "+
				"(sha256 expected %s…, found %s…). Reimport it with `kling mcp import %s -force`",
				snap.Name, chk.file, chk.want[:12], got[:12], snap.Name)
		}
	}
	return nil
}

// fileSHA256 devuelve el sha256 de un fichero en hexadecimal. Con crypto/sha256
// de la stdlib: cero dependencias nuevas.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
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
		return fmt.Errorf("snapshot %q has %d live instance(s) (%v)", name, len(users), users)
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

	// INTEGRIDAD. Antes de tocar nada: si el rootfs dorado o el volcado de estado
	// se corrompieron en disco desde que se congelaron, restaurar produciría una
	// microVM en un estado que ya no es el suyo —o un pánico del invitado— sin una
	// sola señal de la causa. Se falla aquí, claro y pronto, antes de copiar el
	// overlay y de arrancar el VMM.
	if err := m.verifyIntegrity(snap, m.snapDir(req.From)); err != nil {
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
		// Los dominios permitidos viajan con el egress: si se hereda uno, se hereda
		// el otro, o un servicio importado con allowlist despertaría con la lista
		// vacía —sin poder salir a ninguno de sus dominios— sin señal de por qué.
		if len(req.AllowDomains) == 0 {
			req.AllowDomains = snap.AllowDomains
		}
	}

	id := newID()
	if req.Name == "" {
		req.Name = req.From + "-" + id[:6]
	}
	// Reserva de memoria, igual que en Run y por la misma razón: el gateway
	// despierta varios servicios a la vez cuando el anfitrión aprieta, y sin
	// reservar veían todos la misma memoria libre. Ahora el segundo ve que no
	// cabe y el gateway hace sitio antes de reintentar.
	// La clave de compartición es el snapshot de origen: todas sus instancias
	// mapean el MISMO mem.file dorado, así que la segunda y siguientes solo
	// reservan su fracción divergente. Es aquí donde la densidad se vuelve real.
	releaseMem, merr := m.reserveMemory(snap.MemMiB, req.From)
	if merr != nil {
		return nil, merr
	}
	defer releaseMem()

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
		return nil, fmt.Errorf("copying golden overlay: %v: %s", err, out)
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
		return nil, fmt.Errorf("service %q was imported with %d volume(s) and is being given %d.\n"+
			"A restored microVM can't have disks added or removed: reimport it if you want to change it",
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
					return "read-only"
				}
				return "write"
			}
			return nil, fmt.Errorf("volume %q was frozen in %s and is being requested in %s.\n"+
				"The mode is RECORDED in the snapshot and can't change on restore: "+
				"reimport the service if you want the other one",
				v.name, modo(g.ReadOnly), modo(v.readOnly))
		}
		if v.mount != g.Mount {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("volume %q was frozen mounted at %s and is being requested at %s.\n"+
				"The mount point travels in the kernel command line, which was frozen "+
				"with memory: reimport the service if you want to move it",
				v.name, g.Mount, v.mount)
		}
	}

	netcfg := knet.Plan(m.allocNetIndex(), id)
	if err := netcfg.Setup(egress, req.AllowDomains, m.priv.UID); err != nil {
		os.RemoveAll(dir)
		return nil, fmt.Errorf("setting up network: %w", err)
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
		AllowDomains: req.AllowDomains,
		TTLSeconds:   req.TTLSeconds, CPUPct: req.CPUPct,
		Volumes: attachments(vols),
		// Las etiquetas del snapshot se heredan; las de la petición mandan.
		Labels:    api.MergeLabels(snap.Labels, req.Labels),
		CreatedAt: time.Now(),
	}
	m.mu.Lock()
	m.byID[id] = mc
	m.persist()
	m.mu.Unlock()

	// Puerta de arranque: restaurar es cargar un snapshot en KVM, tan intensivo
	// como encender en frío, y es EL camino del gateway cuando despierta varios
	// servicios de golpe. Sin acotarlo, esa ráfaga simultánea cuelga el host bajo
	// anidamiento. El defer suelta el hueco al volver; si el contexto se cancela en
	// la cola, se deshace lo ya montado igual que un fallo de spawn.
	release, glErr := m.enterLaunch(ctx)
	if glErr != nil {
		netcfg.Teardown()
		m.fail(mc, glErr)
		return nil, glErr
	}
	defer release()

	var sock string
	var pid int
	var c *fc.Client
	snapDir := m.snapDir(req.From)
	if jailerEnabled() {
		// Restauración dentro de un jail: firecracker corre chrooteado. Todo lo
		// que va a abrir tiene que estar replicado dentro del jail EN SU RUTA
		// ABSOLUTA, porque LoadSnapshot abre cada drive con el path que quedó
		// grabado —comprobado en el laboratorio—.
		pid, sock, err = m.spawnJailed(id, netcfg)
		if err != nil {
			netcfg.Teardown()
			m.fail(mc, err)
			return nil, err
		}
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			m.fail(mc, err)
			return nil, err
		}
		// Poblar el jail entre el arranque de jailer y la carga: el snapshot y
		// sus discos, el rootfs base, el overlay dorado (que el snapshot abre) y
		// la copia propia, más los volúmenes.
		volPaths := make([]string, len(vols))
		for i, v := range vols {
			volPaths[i] = v.path
		}
		// Los discos de solo lectura de la imagen: la base y, si el servicio se
		// empaquetó por capas, su capa.
		//
		// NO se re-pasa kling.layer= aquí, ni haría falta: la línea de comandos del
		// kernel se congeló DENTRO de la memoria, y el invitado restaurado despierta
		// con la capa ya montada en su tabla de montajes. Lo único que hace falta es
		// que el fichero siga estando donde el snapshot lo grabó — que es justo lo
		// que hace este enlace. Por eso tampoco hay campo Layer en el meta: se
		// resuelve del nombre de la imagen, como la base, y los snapshots viejos no
		// necesitan nada nuevo dentro.
		imgBase, imgLayer, ierr := m.imageLayer(snap.Image)
		if ierr != nil {
			m.fail(mc, ierr)
			return nil, ierr
		}
		toLink := append([]string{
			filepath.Join(snapDir, "snap.file"),
			filepath.Join(snapDir, "mem.file"),
			imgBase,
			imgLayer,
			filepath.Join(snapDir, "overlay.ext4"),
			overlay,
		}, volPaths...)
		if err := m.prepareJail(id, toLink...); err != nil {
			m.fail(mc, err)
			return nil, err
		}
	} else {
		sock = filepath.Join(dir, "fc.sock")
		_ = os.Remove(sock)
		pid, err = m.spawn(id, sock, netcfg)
		if err != nil {
			netcfg.Teardown()
			m.fail(mc, err)
			return nil, err
		}
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			m.fail(mc, err)
			return nil, err
		}
	}

	start := time.Now()
	// Pausada: hay que reapuntar el overlay antes de dejarla correr.
	if err := c.LoadSnapshot(ctx,
		filepath.Join(snapDir, "snap.file"),
		filepath.Join(snapDir, "mem.file"), false); err != nil {
		m.fail(mc, err)
		return nil, err
	}
	// Nada de SetEntropy aquí: tras cargar un snapshot no se pueden añadir
	// dispositivos. El virtio-rng ya viene dentro, porque la plantilla lo tenía
	// al congelarse; y CONFIG_VMGENID hace que el invitado resiembre su pool al
	// detectar que ha sido restaurado.
	if err := c.PatchDrive(ctx, "overlay", overlay); err != nil {
		m.fail(mc, fmt.Errorf("repointing overlay: %w", err))
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
			m.fail(mc, fmt.Errorf("repointing volume %s: %w", v.name, err))
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
		log.Printf("warning: %s: %s", mc.Name, warn)
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
		Message: fmt.Sprintf("instantiated from %s in %d ms", req.From, elapsed)})
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
			log.Printf("this snapshot's disk is called %q, not %q (legacy chain); "+
				"noted for next time", legacyVolumeDriveID, id)
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
