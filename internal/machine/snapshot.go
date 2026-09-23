package machine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"log"

	"github.com/juan52878911/kindling/internal/fc"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"

	"github.com/juan52878911/kindling/pkg/durable"
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
func (m *Manager) Commit(ctx context.Context, ref, name string, replace bool) (*api.Snapshot, error) {
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

	// Reservado mientras dure el commit: sin meta.json todavía, este directorio
	// es indistinguible de los restos de un commit interrumpido, y el barrido
	// (sweepSnapshotLeftovers) o un RemoveSnapshot lo borrarían bajo nuestros pies.
	soltar := m.reserveDir(reservaSnapshot(name))
	defer soltar()

	dir := m.snapDir(name)
	if _, err := os.Stat(dir); err == nil {
		if !replace {
			return nil, fmt.Errorf("snapshot %q already exists (use `kling commit -replace` to replace it)", name)
		}
		// Reemplazar pasa por RemoveSnapshot y no por un RemoveAll directo: es
		// quien sabe negarse si el snapshot tiene instancias vivas, que seguirían
		// mapeando un mem.file que estaríamos pisando debajo de ellas. El borrado
		// previo no es atómico, pero el caso que motiva -replace es un snapshot
		// que un reinicio del host ya dejó irrestaurable: no hay nada que salvar.
		if err := m.removeSnapshot(name, true); err != nil {
			return nil, fmt.Errorf("replacing snapshot %q: %w", name, err)
		}
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
	if out, err := copiarDisco(ctx, ownOverlay, goldDst); err != nil {
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

	if out, err := perforarHuecos(ctx, memPath); err != nil {
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
		VCPUs: mc.VCPUs, MemMiB: mc.MemMiB, MemMaxMiB: mc.MemMaxMiB, Labels: mc.Labels,
		Egress:       mc.Egress,
		CPUPct:       mc.CPUPct,
		AllowDomains: mc.AllowDomains,
		// La puerta de exec se congela con la memoria: las instancias la tendrán
		// quiera quien las cree o no, y el snapshot tiene que decirlo.
		AllowExec:    mc.AllowExec,
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
	if err := m.firmar(snap); err != nil {
		return nil, err
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
	// El DIRECTORIO manda sobre lo que diga el meta.
	//
	// `runFrom` resuelve el snapshot por `snapDir(nombre)`, o sea por directorio.
	// Si el meta declara otro nombre —un meta copiado a mano, un renombrado a
	// medias— el listado enseñaría un servicio que no se puede instanciar, y dos
	// directorios que declaren el mismo nombre saldrían como DUPLICADOS
	// indistinguibles. Observado: `sequentialthinking` apareció dos veces, con
	// tamaños distintos y un solo directorio en disco.
	if s.Name != name {
		log.Printf("snapshot %q: its meta claims the name %q; going with the directory",
			name, s.Name)
		s.Name = name
	}
	liftV04(b, &s)
	return &s, nil
}

// verifyIntegrity comprueba que el rootfs dorado y el volcado de estado no se
// corrompieron desde que se congelaron.
//
// DECISIÓN — qué se hashea y qué no:
//
//	overlay.ext4 (rootfs) y snap.file  -> SÍ, la PRIMERA vez.
//	mem.file                           -> NO aquí.
//
// El fichero de memoria es el grande —cientos de MiB— y restaurar promete ~30 ms;
// leerlo entero por sha256 en cada thaw tiraría esa cifra por tierra.
//
// CORRECCIÓN, medida: la versión anterior de esta nota daba por hecho que "el
// rootfs y el snap.file son pequeños". No lo son. El overlay dorado son 512 MiB
// nominales, y sha256 los lee ENTEROS —los huecos de un fichero disperso se leen
// como ceros, y por el hash pasan igual—. Medido en fc-test: hashearlo cuesta
// 2.866 ms de los 4.280 que tarda una instanciación, el 67%.
//
// El razonamiento era bueno; el supuesto estaba mal por dos órdenes de magnitud.
//
// Lo que arregla el desfase no es hashear menos sino hashear MENOS VECES: un
// dorado es INMUTABLE desde que se congela, así que verificarlo en cada una de
// las 142 instanciaciones no aporta nada sobre verificarlo en la primera. Se
// recuerda el veredicto y se reusa mientras el fichero no cambie de tamaño ni de
// fecha; si cambia, se vuelve a verificar. Un dorado corrupto se sigue detectando,
// y se detecta igual de pronto.
//
// Los snapshots anteriores a esta comprobación no tienen digests grabados: se
// saltan en vez de fallar, o reimportar dejaría de ser opcional para todos.
func (m *Manager) verifyIntegrity(snap *api.Snapshot, snapDir string) error {
	if snap.RootfsSHA256 == "" && snap.SnapSHA256 == "" {
		return nil // snapshot legacy: no hay digests que comprobar
	}
	if m.integridadYaVista(snap.Name, snapDir) {
		return nil
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
	m.anotarIntegridad(snap.Name, snapDir)
	return nil
}

// huellaSnapshot describe los ficheros verificados sin leerlos: tamaño y fecha de
// los dos que se hashean. Basta para saber si el dorado sigue siendo el mismo, y
// cuesta dos stat en vez de 512 MiB de sha256.
type huellaSnapshot struct {
	tam   [2]int64
	fecha [2]int64
}

func huellaDe(snapDir string) (huellaSnapshot, bool) {
	var h huellaSnapshot
	for i, f := range [2]string{"overlay.ext4", "snap.file"} {
		st, err := os.Stat(filepath.Join(snapDir, f))
		if err != nil {
			return h, false
		}
		h.tam[i] = st.Size()
		h.fecha[i] = st.ModTime().UnixNano()
	}
	return h, true
}

// integridadYaVista dice si este dorado, EXACTAMENTE como está ahora en disco, ya
// pasó la verificación. El veredicto vive en memoria y no en disco a propósito:
// tras reiniciar el daemon se vuelve a verificar una vez, que es barato y cubre
// una corrupción ocurrida mientras estaba parado.
func (m *Manager) integridadYaVista(name, snapDir string) bool {
	h, ok := huellaDe(snapDir)
	if !ok {
		return false
	}
	m.mu.RLock()
	visto, hay := m.integridad[name]
	m.mu.RUnlock()
	return hay && visto == h
}

func (m *Manager) anotarIntegridad(name, snapDir string) {
	h, ok := huellaDe(snapDir)
	if !ok {
		return
	}
	m.mu.Lock()
	if m.integridad == nil {
		m.integridad = map[string]huellaSnapshot{}
	}
	m.integridad[name] = h
	m.mu.Unlock()
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
func (m *Manager) RemoveSnapshot(name string) error { return m.removeSnapshot(name, false) }

// removeSnapshot es RemoveSnapshot; propio=true lo llama el commit que TIENE la
// reserva de ese nombre (el camino de -replace), que no debe toparse con ella.
func (m *Manager) removeSnapshot(name string, propio bool) error {
	if _, err := m.loadSnapshot(name); err != nil {
		// Sin meta.json pero con directorio: son los restos de un commit que se
		// interrumpió antes de escribirlo (el meta es lo último). Antes esto
		// abortaba aquí, así que esos GiB solo se recuperaban con un rm -rf a
		// mano, y `commit -replace` con el mismo nombre fallaba igual.
		if !restosDeCommit(m.snapDir(name)) {
			return err
		}
		m.mu.RLock()
		enCurso := m.reserved[reservaSnapshot(name)]
		m.mu.RUnlock()
		if enCurso && !propio {
			return fmt.Errorf("snapshot %q is being committed right now", name)
		}
		log.Printf("snapshot %q: removing the leftovers of an interrupted commit", name)
		return os.RemoveAll(m.snapDir(name))
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

// explainRestoreErr traduce los fallos de restauración de Firecracker con causa
// conocida a un error que dice qué pasó y qué hacer. Los que no reconoce pasan
// intactos: adornar un error sin entenderlo es peor que dejarlo crudo.
//
// El caso que motiva esto: el snapshot graba la frecuencia del TSC del host, y
// esa frecuencia se mide de nuevo en cada arranque — tras reiniciar el host,
// TODOS los snapshots anteriores dejan de restaurar a la vez, con un
// "Could not set TSC scaling ... Invalid argument (os error 22)" que no apunta
// a nada. La recuperación es siempre la misma: rehacer el snapshot.
func explainRestoreErr(err error, what, remedy string) error {
	if !api.EsFalloTSC(err) {
		return err
	}
	return fmt.Errorf("%s can't be restored on this host: the snapshot records the CPU's TSC "+
		"frequency, which changes on every host boot, so a host reboot invalidates every "+
		"snapshot taken before it (this is a Firecracker limitation, not corruption).\n"+
		"Recover by recreating it:\n%s\nUnderlying error: %v", what, remedy, err)
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
	if err := m.comprobarFirma(snap); err != nil {
		return nil, err
	}
	if err := m.verifyIntegrity(snap, m.snapDir(req.From)); err != nil {
		return nil, err
	}

	// La ejecución se encendió (o no) al arrancar la plantilla, en la línea de
	// comandos del kernel que se congeló con la memoria: no se puede conceder al
	// restaurar. Pedirla sobre un snapshot que no la tiene es un error, no un
	// silencio que acabaría en un 404 del invitado.
	if req.AllowExec && !snap.AllowExec {
		return nil, fmt.Errorf("%w: snapshot %q was made from a machine without exec; "+
			"commit one created with allow_exec (kling run -allow-exec) to use it as a sandbox",
			ErrExecNotInSnapshot, req.From)
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
	if err := m.admitir(); err != nil {
		return nil, err
	}
	releaseMem, merr := m.reserveMemoryMakingRoom(ctx, snap.MemMiB, req.From, "")
	if merr != nil {
		return nil, merr
	}
	defer releaseMem()

	// El directorio nace RESERVADO: desde aquí hasta la entrada en byID —copiar
	// el overlay dorado, resolver volúmenes, montar la red— es de lejos la
	// ventana más ancha del daemon, y el barrido de huérfanos corre cada 10 s en
	// cuanto el disco aprieta. Ver makeMachineDir.
	dir, unreserve, err := m.makeMachineDir(id)
	if err != nil {
		return nil, err
	}
	defer unreserve()

	// Copia del overlay dorado: mismo contenido, fichero propio. Compartirlo
	// haría que las instancias se pisaran el disco entre ellas.
	overlay := filepath.Join(dir, "overlay.ext4")
	if out, err := copiarDisco(ctx, filepath.Join(m.snapDir(req.From), "overlay.ext4"), overlay); err != nil {
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
	vols, verr := m.reservarVolumenes(req, id, req.Name)
	// Igual que en Run: se suelta al salir, haya publicado o no. Aqui la ventana
	// era la mas ancha del daemon —incluye copiar el overlay del dorado— y por
	// eso reservar es lo que de verdad la cierra.
	defer m.soltarReservas(id)
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

	creada := time.Now()
	mc := &api.Machine{
		ID: id, Name: req.Name, Image: snap.Image, From: req.From,
		State: api.StateCreated, VCPUs: snap.VCPUs, MemMiB: snap.MemMiB, MemMaxMiB: snap.MemMaxMiB,
		IP: netcfg.NSIP, NetIndex: netcfg.Index, Egress: string(egress),
		AllowDomains: req.AllowDomains,
		TTLSeconds:   req.TTLSeconds, CPUPct: req.CPUPct,
		Volumes:   attachments(vols),
		AllowExec: snap.AllowExec, OnTTL: req.OnTTL,
		// Las etiquetas del snapshot se heredan; las de la petición mandan.
		Labels:    api.MergeLabels(snap.Labels, req.Labels),
		CreatedAt: creada,
		TTLAt:     &creada,
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

	// La máquina queda en disco ANTES de que exista su VMM (ver persistirYa).
	m.persistirYa()

	// abortar es la única salida de error a partir de aquí.
	//
	// m.fail() llama a kill(), que lee el PID de la MÁQUINA, y ese PID no se
	// escribe hasta el final de esta función: sin matar explícitamente el proceso
	// que acabamos de lanzar, cada restauración fallida —un snapshot con el TSC
	// invalidado tras reiniciar el host, por ejemplo— deja un firecracker vivo
	// reteniendo su RAM, invisible para `kling ps` y para la contabilidad de
	// memoria. El arranque en frío ya aprendió esta lección (ver Run); este
	// camino no la había copiado.
	abortar := func(err error) (*api.Machine, error) {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		netcfg.Teardown()
		m.fail(mc, err)
		return nil, err
	}
	if jailerEnabled() {
		// Restauración dentro de un jail: firecracker corre chrooteado. Todo lo
		// que va a abrir tiene que estar replicado dentro del jail EN SU RUTA
		// ABSOLUTA, porque LoadSnapshot abre cada drive con el path que quedó
		// grabado —comprobado en el laboratorio—.
		pid, sock, err = m.spawnJailed(id, netcfg)
		if err != nil {
			return abortar(err)
		}
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			return abortar(err)
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
			return abortar(ierr)
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
			return abortar(err)
		}
	} else {
		sock = filepath.Join(dir, "fc.sock")
		_ = os.Remove(sock)
		pid, err = m.spawn(id, sock, netcfg)
		if err != nil {
			return abortar(err)
		}
		c = fc.New(sock)
		if err := waitSocket(ctx, c); err != nil {
			return abortar(err)
		}
	}

	// macOS: la política de salida viaja con la instancia, no con el snapshot.
	if err := m.redAntesDeArrancar(ctx, c, id); err != nil {
		return abortar(err)
	}
	start := time.Now()
	// Pausada: hay que reapuntar el overlay antes de dejarla correr.
	if err := c.LoadSnapshot(ctx,
		filepath.Join(snapDir, "snap.file"),
		filepath.Join(snapDir, "mem.file"), false); err != nil {
		// Con causa conocida (TSC tras reiniciar el host) se traduce ANTES de
		// propagar: este error acaba en el 502 del gateway y en el CLI, y el
		// texto crudo de Firecracker no le dice a nadie qué hacer.
		err = explainRestoreErr(err, fmt.Sprintf("snapshot %q", req.From), fmt.Sprintf(
			"  kling mcp import %s -force    (imported MCP service)\n"+
				"  kling commit -replace <machine> %s    (manual snapshot)", req.From, req.From))
		return abortar(err)
	}
	// Nada de SetEntropy aquí: tras cargar un snapshot no se pueden añadir
	// dispositivos. El virtio-rng ya viene dentro, porque la plantilla lo tenía
	// al congelarse; y CONFIG_VMGENID hace que el invitado resiembre su pool al
	// detectar que ha sido restaurado.
	if err := c.PatchDrive(ctx, "overlay", overlay); err != nil {
		return abortar(fmt.Errorf("repointing overlay: %w", err))
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
			return abortar(fmt.Errorf("repointing volume %s: %w", v.name, err))
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
		return abortar(err)
	}
	// macOS: sin reenvíos el host no llega al invitado, y acquireVolumes de
	// aquí abajo ya necesita hablarle.
	if err := m.abrirReenvios(ctx, c, id); err != nil {
		return abortar(err)
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
			return abortar(err)
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
	// Durable, no solo atomico. Un meta perdido en un corte no da error: el
	// servicio DESAPARECE del catalogo en silencio (Snapshots() salta lo que no
	// puede leer), y RemoveSnapshot se niega despues a borrar lo ilegible, asi
	// que el directorio queda varado con su mem.file de cientos de MB.
	return durable.Escribir(filepath.Join(dir, "meta.json"), b, 0o644)
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

// reservaSnapshot es la clave con la que un commit en curso reserva su
// directorio en m.reserved, el mismo registro que protege los de las máquinas.
func reservaSnapshot(name string) string { return "snap:" + name }

// restosDeCommit dice si dir es un snapshot a medias: existe pero no tiene
// meta.json, que es lo último que escribe un commit.
func restosDeCommit(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "meta.json"))
	return errors.Is(err, os.ErrNotExist)
}

// sweepSnapshotLeftovers aparta a la papelera los snapshots a medias que lleven
// más de dirGrace sin tocarse y que nadie esté escribiendo. Solo mueve (rápido);
// vaciarPapelera borra. Se llama con m.mu tomado.
func (m *Manager) sweepSnapshotLeftovers() {
	base := filepath.Join(m.root, "snapshots")
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if m.reserved[reservaSnapshot(e.Name())] || !restosDeCommit(filepath.Join(base, e.Name())) {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) < dirGrace {
			continue
		}
		papelera := filepath.Join(m.root, "machines", papeleraDir)
		if err := os.MkdirAll(papelera, 0o700); err != nil {
			return
		}
		destino := filepath.Join(papelera, fmt.Sprintf("snap-%s-%d", e.Name(), time.Now().UnixNano()))
		if err := os.Rename(filepath.Join(base, e.Name()), destino); err != nil {
			log.Printf("reconcile: couldn't move the leftovers of snapshot %q: %v", e.Name(), err)
			continue
		}
		log.Printf("reconcile: snapshot %q was an interrupted commit; its leftovers go to the trash", e.Name())
	}
}
