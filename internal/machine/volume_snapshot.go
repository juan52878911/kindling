package machine

// Snapshots de volumen: copias puntuales de un volumen a las que se puede
// volver con restore.
//
// La coherencia no se consigue congelando el sistema de ficheros en caliente
// (no hay fsfreeze dentro del invitado), sino con la misma regla que ya decide
// quién monta: un snapshot solo se toma sin ESCRITORES (los lectores no cambian
// los bloques), y un restore solo sin NINGÚN usuario. Una máquina congelada o
// warm cuenta como usuaria: tiene el volumen montado, con su caché de ext4 en la
// memoria congelada. Lo impone una reserva sintética en volReservas, bajo m.mu,
// así que un arranque que llegue a medias ve la operación y se queda fuera.
//
// Los snapshots viven en volumes/snapshots/<vol>/<snap>.ext4, directorios 0700
// y ficheros 0600 de root: el VMM no tiene por qué leerlos, y uno comprometido
// no puede reescribir el pasado al que se vuelve.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/durable"
)

// maxVolumeSnapshots es el tope por volumen, sin contar "undo". Sin reflink
// cada snapshot es una copia completa: el tope es lo que impide que un bucle
// llene el disco de copias.
const maxVolumeSnapshots = 16

// clonarPlazo acota una copia completa. Un volumen de 64 GiB (el máximo) a
// 100 MB/s son unos 11 minutos; más que eso es un disco que no responde.
const clonarPlazo = 30 * time.Minute

// opPrefijo marca las reservas sintéticas. Empieza por un byte nulo para que
// ningún ID de máquina pueda coincidir.
const opPrefijo = "\x00volop:"

// Qué hace una operación, y por tanto a quién excluye.
type tipoOperacion int

const (
	opSnapshot  tipoOperacion = iota // lee: fuera los escritores
	opRestore                        // reescribe: fuera todos
	opBorrarSnp                      // solo toca snapshots: fuera las demás operaciones
)

func (t tipoOperacion) desc() string {
	switch t {
	case opSnapshot:
		return "being snapshotted"
	case opRestore:
		return "being restored"
	default:
		return "having a snapshot removed"
	}
}

func (m *Manager) volSnapsBase() string          { return filepath.Join(m.volumesDir(), "snapshots") }
func (m *Manager) volSnapsDir(vol string) string { return filepath.Join(m.volSnapsBase(), vol) }
func (m *Manager) volSnapPath(vol, s string) string {
	return filepath.Join(m.volSnapsDir(vol), s+".ext4")
}

func errEstado(code int, format string, a ...any) error {
	return &api.StatusError{Code: code, Message: fmt.Sprintf(format, a...)}
}

// validarNombres comprueba volumen y snapshot. Los dos acaban siendo
// componentes de ruta, así que la misma expresión que los volúmenes.
func validarNombres(vol, snap string) error {
	if !reVolume.MatchString(vol) {
		return errEstado(http.StatusBadRequest, "invalid volume name %q", vol)
	}
	if !reVolume.MatchString(snap) {
		return errEstado(http.StatusBadRequest,
			"invalid snapshot name %q: lowercase letters, digits, hyphen and underscore", snap)
	}
	return nil
}

// operacionEnCursoLocked dice qué operación de snapshot hay sobre vol, o "".
func (m *Manager) operacionEnCursoLocked(vol string) string {
	for _, r := range m.volReservas[vol] {
		if r.maquina == opPrefijo+vol {
			return r.nombre
		}
	}
	return ""
}

// reservarOperacion comprueba Y reserva en la misma sección crítica, como
// reservarVolumenes: entre mirar y copiar no se puede colar un arranque que
// monte el volumen en escritura. Devuelve con qué soltarla.
func (m *Manager) reservarOperacion(vol string, t tipoOperacion) (func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if op := m.operacionEnCursoLocked(vol); op != "" {
		return nil, errEstado(http.StatusConflict, "volume %q is %s: try again when it finishes", vol, op)
	}
	u := m.volumeUsersLocked()[vol]
	switch t {
	case opSnapshot:
		if len(u.writers) > 0 {
			return nil, errEstado(http.StatusConflict,
				"volume %q is mounted in WRITE mode by %s.\n"+
					"A snapshot needs no writers, so it is consistent: stop it first "+
					"(a frozen machine still has it mounted)", vol, strings.Join(u.writers, ", "))
		}
	case opRestore:
		if users := u.all(); len(users) > 0 {
			return nil, errEstado(http.StatusConflict,
				"volume %q is used by %d machine(s): %s.\n"+
					"Restoring rewrites the whole disk: stop them first (a frozen machine still has it mounted)",
				vol, len(users), strings.Join(users, ", "))
		}
	}
	if m.volReservas == nil {
		m.volReservas = map[string][]reservaVolumen{}
	}
	d := t.desc()
	m.volReservas[vol] = append(m.volReservas[vol], reservaVolumen{
		maquina: opPrefijo + vol, nombre: d, desc: "(" + d + ")",
		soloLect: t != opRestore, sinUso: t == opBorrarSnp,
	})
	return func() { m.soltarReservas(opPrefijo + vol) }, nil
}

// asegurarDirSnapshots crea volumes/snapshots/<vol> de root y 0700, y se
// niega si alguno de los dos es otra cosa que un directorio de verdad.
func (m *Manager) asegurarDirSnapshots(vol string) (string, error) {
	for _, d := range []string{m.volSnapsBase(), m.volSnapsDir(vol)} {
		if err := os.Mkdir(d, 0o700); err != nil && !os.IsExist(err) {
			return "", err
		}
		fi, err := os.Lstat(d)
		if err != nil {
			return "", err
		}
		if !fi.IsDir() {
			return "", fmt.Errorf("%s is not a directory: refusing to write snapshots there", d)
		}
		if fi.Mode().Perm() != 0o700 {
			_ = os.Chmod(d, 0o700)
		}
	}
	return m.volSnapsDir(vol), nil
}

// ficheroRegular comprueba que la ruta es un fichero normal: cp sobre un FIFO
// o un dispositivo se quedaría colgado o copiaría otra cosa.
func ficheroRegular(ruta string) (os.FileInfo, error) {
	fi, err := os.Lstat(ruta)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", ruta)
	}
	return fi, nil
}

// snapshotsDe lee los snapshots de un volumen. Sin directorio, ninguno.
func (m *Manager) snapshotsDe(vol string) ([]*api.VolumeSnapshot, error) {
	// os.OpenRoot: lo que se lee no puede salirse del directorio aunque alguien
	// plante un enlace dentro.
	root, err := os.OpenRoot(m.volSnapsDir(vol))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer root.Close()
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	nombres, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return nil, err
	}
	var out []*api.VolumeSnapshot
	for _, n := range nombres {
		snap, ok := strings.CutSuffix(n, ".ext4")
		if !ok || !reVolume.MatchString(snap) {
			continue // los .tmp y lo que no sea nuestro
		}
		fi, err := root.Lstat(n)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		out = append(out, m.describirSnapshot(vol, snap, fi))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (m *Manager) describirSnapshot(vol, snap string, fi os.FileInfo) *api.VolumeSnapshot {
	return &api.VolumeSnapshot{
		Volume: vol, Name: snap, CreatedAt: fi.ModTime().UTC(),
		SizeBytes: fi.Size(), UsedBytes: allocatedBytes(m.volSnapPath(vol, snap)),
		Undo: snap == api.VolumeSnapshotUndo,
	}
}

// contarSnapshots cuenta los de un volumen, "undo" incluido.
func (m *Manager) contarSnapshots(vol string) int {
	l, _ := m.snapshotsDe(vol)
	return len(l)
}

// VolumeSnapshots lista los snapshots de un volumen, del más antiguo al último.
func (m *Manager) VolumeSnapshots(vol string) ([]*api.VolumeSnapshot, error) {
	if !reVolume.MatchString(vol) {
		return nil, errEstado(http.StatusBadRequest, "invalid volume name %q", vol)
	}
	l, err := m.snapshotsDe(vol)
	if err != nil {
		return nil, err
	}
	if len(l) == 0 {
		if _, err := os.Stat(m.volumePath(vol)); err != nil {
			return nil, errEstado(http.StatusNotFound, "volume %q not found", vol)
		}
	}
	if l == nil {
		l = []*api.VolumeSnapshot{}
	}
	return l, nil
}

// SnapshotVolume copia un volumen sin escritores. snap vacío = hora UTC.
func (m *Manager) SnapshotVolume(ctx context.Context, vol, snap string) (*api.VolumeSnapshot, error) {
	if snap == "" {
		snap = time.Now().UTC().Format("20060102-150405")
	}
	if err := validarNombres(vol, snap); err != nil {
		return nil, err
	}
	if snap == api.VolumeSnapshotUndo {
		return nil, errEstado(http.StatusBadRequest,
			"%q is reserved: restore keeps the previous state there", api.VolumeSnapshotUndo)
	}
	soltar, err := m.reservarOperacion(vol, opSnapshot)
	if err != nil {
		return nil, err
	}
	defer soltar()

	src := m.volumePath(vol)
	if _, err := ficheroRegular(src); err != nil {
		if os.IsNotExist(err) {
			return nil, errEstado(http.StatusNotFound, "volume %q not found", vol)
		}
		return nil, err
	}
	dir, err := m.asegurarDirSnapshots(vol)
	if err != nil {
		return nil, err
	}
	existentes, err := m.snapshotsDe(vol)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, s := range existentes {
		if s.Name == snap {
			return nil, errEstado(http.StatusConflict, "snapshot %s@%s already exists", vol, snap)
		}
		if !s.Undo {
			n++
		}
	}
	if n >= maxVolumeSnapshots {
		return nil, errEstado(http.StatusConflict,
			"volume %q already has %d snapshots, the maximum: remove one with `kling volume rm %s@<snapshot>`",
			vol, n, vol)
	}

	dst := filepath.Join(dir, snap+".ext4")
	tmp := dst + ".tmp"
	modo, err := m.clonarTmp(ctx, src, tmp)
	if err != nil {
		return nil, err
	}
	if err := publicarSnapshot(tmp, dst); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(dst)
	if err != nil {
		return nil, err
	}
	s := m.describirSnapshot(vol, snap, fi)
	s.Mode = modo
	log.Printf("volume %s: snapshot %s taken (%s)", vol, snap, modo)
	return s, nil
}

// RestoreVolume devuelve un volumen al contenido de un snapshot.
//
// Antes guarda el estado actual en "undo", así que un restore equivocado se
// deshace con otro restore. El orden importa para que un corte a medias no
// pierda nada:
//
//  1. actual   -> undo.ext4.tmp
//  2. snapshot -> <vol>.ext4.tmp   (el origen ya está leído entero, aunque sea undo)
//  3. publicar undo                (volumen intacto, undo = volumen)
//  4. publicar el volumen
//
// Un corte entre 3 y 4 deja el volumen como estaba y un undo que es su copia.
func (m *Manager) RestoreVolume(ctx context.Context, vol, snap string) (*api.RestoreVolumeResult, error) {
	if err := validarNombres(vol, snap); err != nil {
		return nil, err
	}
	soltar, err := m.reservarOperacion(vol, opRestore)
	if err != nil {
		return nil, err
	}
	defer soltar()

	dstVol := m.volumePath(vol)
	if _, err := ficheroRegular(dstVol); err != nil {
		if os.IsNotExist(err) {
			return nil, errEstado(http.StatusNotFound, "volume %q not found", vol)
		}
		return nil, err
	}
	src := m.volSnapPath(vol, snap)
	if _, err := ficheroRegular(src); err != nil {
		if os.IsNotExist(err) {
			return nil, errEstado(http.StatusNotFound, "snapshot %s@%s not found: list them with `kling volume snapshots %s`", vol, snap, vol)
		}
		return nil, err
	}
	dir, err := m.asegurarDirSnapshots(vol)
	if err != nil {
		return nil, err
	}

	undo := filepath.Join(dir, api.VolumeSnapshotUndo+".ext4")
	undoTmp := undo + ".tmp"
	if _, err := m.clonarTmp(ctx, dstVol, undoTmp); err != nil {
		return nil, fmt.Errorf("saving the current state to %s@undo: %w", vol, err)
	}
	volTmp := dstVol + ".tmp"
	modo, err := m.clonarTmp(ctx, src, volTmp)
	if err != nil {
		_ = os.Remove(undoTmp)
		return nil, err
	}
	if err := publicarSnapshot(undoTmp, undo); err != nil {
		_ = os.Remove(volTmp)
		return nil, err
	}
	// El volumen vuelve a ser del VMM ANTES de publicarlo: el snapshot era de
	// root y 0600, y un volumen que el VMM no puede escribir no sirve.
	_ = os.Chmod(volTmp, 0o640)
	m.permisosVolumen(volTmp)
	if err := durable.Renombrar(volTmp, dstVol); err != nil {
		_ = os.Remove(volTmp)
		return nil, err
	}
	fi, err := os.Lstat(undo)
	if err != nil {
		return nil, err
	}
	log.Printf("volume %s: restored to %s (%s); previous state in %s@undo", vol, snap, modo, vol)
	return &api.RestoreVolumeResult{Volume: vol, Snapshot: snap, Mode: modo,
		Undo: m.describirSnapshot(vol, api.VolumeSnapshotUndo, fi)}, nil
}

// RemoveVolumeSnapshot borra un snapshot. No toca el volumen, así que no
// espera a que lo suelten las máquinas; solo a otras operaciones de snapshot.
func (m *Manager) RemoveVolumeSnapshot(vol, snap string) error {
	if err := validarNombres(vol, snap); err != nil {
		return err
	}
	soltar, err := m.reservarOperacion(vol, opBorrarSnp)
	if err != nil {
		return err
	}
	defer soltar()

	root, err := os.OpenRoot(m.volSnapsDir(vol))
	if err != nil {
		if os.IsNotExist(err) {
			return errEstado(http.StatusNotFound, "snapshot %s@%s not found", vol, snap)
		}
		return err
	}
	defer root.Close()
	if err := root.Remove(snap + ".ext4"); err != nil {
		if os.IsNotExist(err) {
			return errEstado(http.StatusNotFound, "snapshot %s@%s not found", vol, snap)
		}
		return err
	}
	return nil
}

// clonarTmp copia src en tmp con clonarDisco, bajo plazo, y deja tmp con la
// hora de ahora (clonefile conserva la del origen, y la hora es la del
// snapshot). Si falla, no deja tmp.
func (m *Manager) clonarTmp(ctx context.Context, src, tmp string) (string, error) {
	_ = os.Remove(tmp)
	cctx, cancel := context.WithTimeout(ctx, clonarPlazo)
	defer cancel()
	modo, err := clonarDisco(cctx, src, tmp, func() error { return m.cabeCopia(src) })
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	ahora := time.Now()
	_ = os.Chtimes(tmp, ahora, ahora)
	return modo, nil
}

// publicarSnapshot deja tmp de root y 0600 y lo publica con durable.Renombrar.
func publicarSnapshot(tmp, dst string) error {
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := durable.Renombrar(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// cabeCopia se llama justo antes de una copia COMPLETA (sin reflink ni
// clonefile). Se niega si lo asignado del origen no cabe en el disco libre
// menos lo que se reserva: el suelo de admisión o el hueco hasta la marca alta
// del gc, lo que sea mayor. Una copia que llene el disco no la paga quien la
// pidió, la pagan todas las microVMs del host.
func (m *Manager) cabeCopia(src string) error {
	var st syscall.Statfs_t
	if err := syscall.Statfs(m.volumesDir(), &st); err != nil {
		return nil // sin poder medirlo no se bloquea nada, como checkDisk
	}
	libre := int64(st.Bavail) * int64(st.Bsize)
	total := int64(st.Blocks) * int64(st.Bsize)
	reserva := max(minFreeDiskMiB()<<20, total*int64(100-gcDiskHighPct())/100)
	falta := allocatedBytes(src)
	if falta <= libre-reserva {
		return nil
	}
	return errEstado(http.StatusInsufficientStorage,
		"not enough disk for a full copy of %s: it needs %d MiB and only %d MiB are free above the "+
			"%d MiB kept in reserve.\nThis filesystem cannot share blocks (no reflink): "+
			"on XFS or Btrfs a snapshot costs nothing until it diverges. Remove old snapshots "+
			"(`kling volume snapshots <vol>`) or free disk",
		filepath.Base(src), falta>>20, max(libre, 0)>>20, reserva>>20)
}

// permisosVolumen deja un fichero de volumen escribible por el VMM, como lo
// dejaba EnsureWritable: dueño el usuario del VMM, grupo el suyo, 0640.
func (m *Manager) permisosVolumen(ruta string) {
	if m.priv == nil || !m.priv.Enabled {
		return
	}
	_ = os.Lchown(ruta, m.priv.UID, m.priv.GID)
	_ = os.Chmod(ruta, 0o640)
}

// barrerTmpVolumenes borra al arrancar los .tmp que dejó un daemon muerto a
// media copia, y devuelve a su sitio los permisos de los snapshots. Solo al
// arrancar: en marcha, un .tmp puede ser una copia en curso.
func (m *Manager) barrerTmpVolumenes() {
	borrar := func(dir string) {
		entradas, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entradas {
			if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".tmp") {
				if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
					log.Printf("volumes: removed leftover %s", filepath.Join(filepath.Base(dir), e.Name()))
				}
			}
		}
	}
	borrar(m.volumesDir())
	base := m.volSnapsBase()
	fi, err := os.Lstat(base)
	if err != nil || !fi.IsDir() {
		return
	}
	_ = os.Chmod(base, 0o700)
	entradas, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entradas {
		if !e.IsDir() || !reVolume.MatchString(e.Name()) {
			continue
		}
		dir := filepath.Join(base, e.Name())
		_ = os.Chmod(dir, 0o700)
		borrar(dir)
	}
}
