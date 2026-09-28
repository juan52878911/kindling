package machine

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func codigo(err error) int {
	var se *api.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

func escribirVolumen(t *testing.T, m *Manager, vol, contenido string) {
	t.Helper()
	if err := os.MkdirAll(m.volumesDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.volumePath(vol), []byte(contenido), 0o644); err != nil {
		t.Fatal(err)
	}
}

func leerVolumen(t *testing.T, m *Manager, vol string) string {
	t.Helper()
	b, err := os.ReadFile(m.volumePath(vol))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func montar(m *Manager, id string, estado api.State, vol string, ro bool) {
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: "svc-" + id, State: estado,
		Volumes: []api.VolumeAttachment{{Name: vol, Mount: "/data", ReadOnly: ro}}}
	m.mu.Unlock()
}

// Los nombres acaban en volumes/snapshots/<vol>/<snap>.ext4: nada que se salga
// del directorio, y "undo" es de restore.
func TestSnapshotDeVolumenValidaNombres(t *testing.T) {
	m := newTestManager(t)
	escribirVolumen(t, m, "datos", "v1")
	ctx := context.Background()
	for _, s := range []string{"../fuera", "a/b", "Mayus", "con espacio", ".oculto", strings.Repeat("x", 65)} {
		if _, err := m.SnapshotVolume(ctx, "datos", s); codigo(err) != http.StatusBadRequest {
			t.Errorf("snapshot %q: quería 400, dio %v", s, err)
		}
		if _, err := m.RestoreVolume(ctx, "datos", s); codigo(err) != http.StatusBadRequest {
			t.Errorf("restore %q: quería 400, dio %v", s, err)
		}
		if err := m.RemoveVolumeSnapshot("datos", s); codigo(err) != http.StatusBadRequest {
			t.Errorf("rm %q: quería 400, dio %v", s, err)
		}
	}
	if _, err := m.SnapshotVolume(ctx, "../images/min", "a"); codigo(err) != http.StatusBadRequest {
		t.Errorf("volumen con travesía: quería 400, dio %v", err)
	}
	if _, err := m.SnapshotVolume(ctx, "datos", "undo"); codigo(err) != http.StatusBadRequest {
		t.Errorf("undo está reservado: quería 400, dio %v", err)
	}
	// Sin nombre, la hora UTC, que también es un nombre válido.
	s, err := m.SnapshotVolume(ctx, "datos", "")
	if err != nil {
		t.Fatal(err)
	}
	if !reVolume.MatchString(s.Name) || len(s.Name) != len("20260928-153012") {
		t.Errorf("nombre por defecto inesperado: %q", s.Name)
	}
	if _, err := m.SnapshotVolume(ctx, "no-existe", "a"); codigo(err) != http.StatusNotFound {
		t.Errorf("volumen inexistente: quería 404, dio %v", err)
	}
	if _, err := m.RestoreVolume(ctx, "datos", "no-existe"); codigo(err) != http.StatusNotFound {
		t.Errorf("snapshot inexistente: quería 404, dio %v", err)
	}
}

// Un snapshot sin escritores; los lectores no cambian los bloques. Un restore
// sin nadie. Una máquina congelada o warm cuenta: tiene el volumen montado.
func TestSnapshotYRestoreRespetanALosUsuarios(t *testing.T) {
	ctx := context.Background()
	for _, estado := range []api.State{api.StateRunning, api.StateWarm, api.StatePaused} {
		m := newTestManager(t)
		escribirVolumen(t, m, "datos", "v1")
		montar(m, "w", estado, "datos", false)
		if _, err := m.SnapshotVolume(ctx, "datos", "a"); codigo(err) != http.StatusConflict {
			t.Errorf("%s en escritura: el snapshot debería dar 409, dio %v", estado, err)
		}
	}

	m := newTestManager(t)
	escribirVolumen(t, m, "datos", "v1")
	montar(m, "r", api.StateRunning, "datos", true)
	if _, err := m.SnapshotVolume(ctx, "datos", "a"); err != nil {
		t.Fatalf("con solo lectores el snapshot debería valer: %v", err)
	}
	if _, err := m.RestoreVolume(ctx, "datos", "a"); codigo(err) != http.StatusConflict {
		t.Errorf("restore con un lector: quería 409, dio %v", err)
	}
	// Parada ya no lo retiene.
	m.mu.Lock()
	m.byID["r"].State = api.StateStopped
	m.mu.Unlock()
	if _, err := m.RestoreVolume(ctx, "datos", "a"); err != nil {
		t.Errorf("con la máquina parada el restore debería valer: %v", err)
	}
	// Y la reserva se soltó: nadie queda como usuario.
	if u := m.volumeUsers()["datos"].all(); len(u) != 0 {
		t.Errorf("quedó una reserva colgada: %v", u)
	}
}

// Mientras dura la operación, un arranque en escritura (o cualquiera, durante
// un restore) se queda fuera, y el error dice por qué.
func TestOperacionEnCursoExcluyeArranquesYOtrasOperaciones(t *testing.T) {
	m := newTestManager(t)
	escribirVolumen(t, m, "datos", "v1")

	soltar, err := m.reservarOperacion("datos", opSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.reservarVolumenes(api.RunRequest{Volume: "datos"}, newID(), "x")
	if err == nil || !strings.Contains(err.Error(), "being snapshotted") {
		t.Errorf("un escritor entró durante el snapshot: %v", err)
	}
	if _, err := m.reservarVolumenes(api.RunRequest{Volume: "datos", VolumeReadOnly: true}, newID(), "y"); err != nil {
		t.Errorf("un lector debería poder entrar durante el snapshot: %v", err)
	}
	if _, err := m.reservarOperacion("datos", opBorrarSnp); codigo(err) != http.StatusConflict {
		t.Errorf("dos operaciones a la vez: quería 409, dio %v", err)
	}
	if err := m.RemoveVolume("datos", true); err == nil {
		t.Error("borró el volumen en mitad de un snapshot")
	}
	soltar()

	m2 := newTestManager(t)
	escribirVolumen(t, m2, "datos", "v1")
	soltar, err = m2.reservarOperacion("datos", opRestore)
	if err != nil {
		t.Fatal(err)
	}
	defer soltar()
	_, err = m2.reservarVolumenes(api.RunRequest{Volume: "datos", VolumeReadOnly: true}, newID(), "z")
	if err == nil || !strings.Contains(err.Error(), "being restored") {
		t.Errorf("un lector entró durante el restore: %v", err)
	}
}

// Borrar un snapshot no toca el disco de las máquinas: no cuenta como usuario.
func TestBorrarSnapshotNoCuentaComoUsuario(t *testing.T) {
	m := newTestManager(t)
	escribirVolumen(t, m, "datos", "v1")
	soltar, err := m.reservarOperacion("datos", opBorrarSnp)
	if err != nil {
		t.Fatal(err)
	}
	defer soltar()
	if u := m.volumeUsers()["datos"].all(); len(u) != 0 {
		t.Errorf("borrar un snapshot aparece como usuario: %v", u)
	}
	if _, err := m.reservarVolumenes(api.RunRequest{Volume: "datos"}, newID(), "x"); err != nil {
		t.Errorf("un escritor debería poder entrar mientras se borra un snapshot: %v", err)
	}
}

// El ciclo entero: snapshot, cambio, restore, y el undo deshace el restore.
func TestRestoreYUndoIdaYVuelta(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	escribirVolumen(t, m, "datos", "v1")
	if _, err := m.SnapshotVolume(ctx, "datos", "uno"); err != nil {
		t.Fatal(err)
	}
	escribirVolumen(t, m, "datos", "v2-roto")

	res, err := m.RestoreVolume(ctx, "datos", "uno")
	if err != nil {
		t.Fatal(err)
	}
	if got := leerVolumen(t, m, "datos"); got != "v1" {
		t.Errorf("tras restaurar: %q, quería v1", got)
	}
	if res.Undo == nil || !res.Undo.Undo {
		t.Errorf("el resultado no trae el undo: %+v", res)
	}
	// Restaurar desde undo: el origen se lee entero antes de pisar undo.
	if _, err := m.RestoreVolume(ctx, "datos", "undo"); err != nil {
		t.Fatal(err)
	}
	if got := leerVolumen(t, m, "datos"); got != "v2-roto" {
		t.Errorf("tras deshacer: %q, quería v2-roto", got)
	}
	b, err := os.ReadFile(m.volSnapPath("datos", "undo"))
	if err != nil || string(b) != "v1" {
		t.Errorf("undo debería tener ahora v1: %q %v", b, err)
	}
	// El snapshot original no se tocó.
	if b, _ := os.ReadFile(m.volSnapPath("datos", "uno")); string(b) != "v1" {
		t.Errorf("el snapshot cambió: %q", b)
	}
	l, err := m.VolumeSnapshots("datos")
	if err != nil || len(l) != 2 {
		t.Fatalf("quería uno y undo: %v %v", l, err)
	}
	// Sin restos.
	for _, d := range []string{m.volumesDir(), m.volSnapsDir("datos")} {
		es, _ := os.ReadDir(d)
		for _, e := range es {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Errorf("quedó %s en %s", e.Name(), d)
			}
		}
	}
}

// 16 como mucho, sin contar undo; y un nombre repetido es 409, no pisar.
func TestTopeDeSnapshotsPorVolumen(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	escribirVolumen(t, m, "datos", "v1")
	for i := 0; i < maxVolumeSnapshots; i++ {
		if _, err := m.SnapshotVolume(ctx, "datos", "s"+string(rune('a'+i))); err != nil {
			t.Fatalf("snapshot %d: %v", i, err)
		}
	}
	if _, err := m.SnapshotVolume(ctx, "datos", "sobra"); codigo(err) != http.StatusConflict {
		t.Errorf("el 17: quería 409, dio %v", err)
	}
	if _, err := m.SnapshotVolume(ctx, "datos", "sa"); codigo(err) != http.StatusConflict {
		t.Errorf("repetido: quería 409, dio %v", err)
	}
	// undo no cuenta: un restore sigue pudiendo guardarlo.
	if _, err := m.RestoreVolume(ctx, "datos", "sa"); err != nil {
		t.Errorf("restore con el tope lleno: %v", err)
	}
	if err := m.RemoveVolumeSnapshot("datos", "sa"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemoveVolumeSnapshot("datos", "sa"); codigo(err) != http.StatusNotFound {
		t.Errorf("borrar dos veces: quería 404, dio %v", err)
	}
	if _, err := m.SnapshotVolume(ctx, "datos", "sobra"); err != nil {
		t.Errorf("tras borrar uno debería caber: %v", err)
	}
}

// Con snapshots, borrar el volumen exige pedirlo: son la única copia de su pasado.
func TestBorrarVolumenConSnapshotsExigePedirlo(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	escribirVolumen(t, m, "datos", "v1")
	if _, err := m.SnapshotVolume(ctx, "datos", "uno"); err != nil {
		t.Fatal(err)
	}
	vols := m.Volumes()
	if len(vols) != 1 || vols[0].Snapshots != 1 {
		t.Fatalf("Volumes debería contar el snapshot (y no listar snapshots/ como volumen): %+v", vols)
	}
	err := m.RemoveVolume("datos", false)
	if codigo(err) != http.StatusConflict || !strings.Contains(err.Error(), "-snapshots") {
		t.Errorf("quería 409 que diga -snapshots, dio %v", err)
	}
	if _, err := os.Stat(m.volumePath("datos")); err != nil {
		t.Fatal("el volumen se borró pese al rechazo")
	}
	if err := m.RemoveVolume("datos", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.volSnapsDir("datos")); !os.IsNotExist(err) {
		t.Errorf("los snapshots siguen ahí: %v", err)
	}
	if err := m.RemoveVolume("datos", true); codigo(err) != http.StatusNotFound {
		t.Errorf("borrar dos veces: quería 404, dio %v", err)
	}
}

// Los snapshots son de root: directorios 0700 y ficheros 0600. El volumen
// restaurado vuelve a ser escribible.
func TestPermisosDeLosSnapshots(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()
	escribirVolumen(t, m, "datos", "v1")
	if _, err := m.SnapshotVolume(ctx, "datos", "uno"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{m.volSnapsBase(), m.volSnapsDir("datos")} {
		fi, err := os.Stat(d)
		if err != nil || fi.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v, quería 0700", d, fi.Mode().Perm(), err)
		}
	}
	fi, err := os.Stat(m.volSnapPath("datos", "uno"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("snapshot: %v %v, quería 0600", fi.Mode().Perm(), err)
	}
	if _, err := m.RestoreVolume(ctx, "datos", "uno"); err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(m.volumePath("datos"))
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("volumen restaurado: %v %v, quería 0640", fi.Mode().Perm(), err)
	}
	// Un snapshots/ que sea un enlace no se sigue.
	m2 := newTestManager(t)
	escribirVolumen(t, m2, "datos", "v1")
	fuera := t.TempDir()
	if err := os.Symlink(fuera, m2.volSnapsBase()); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.SnapshotVolume(ctx, "datos", "uno"); err == nil {
		t.Error("escribió snapshots a través de un enlace simbólico")
	}
	if es, _ := os.ReadDir(fuera); len(es) != 0 {
		t.Errorf("apareció algo fuera: %v", es)
	}
}

// Al arrancar se barren los .tmp de copias a medias, y solo los .tmp.
func TestBarridoDeTmpDeVolumenes(t *testing.T) {
	m := newTestManager(t)
	escribirVolumen(t, m, "datos", "v1")
	if _, err := m.asegurarDirSnapshots("datos"); err != nil {
		t.Fatal(err)
	}
	restos := []string{m.volumePath("datos") + ".tmp", m.volSnapPath("datos", "a") + ".tmp"}
	for _, r := range restos {
		if err := os.WriteFile(r, []byte("medio"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	buenos := []string{m.volumePath("datos"), m.volSnapPath("datos", "b")}
	if err := os.WriteFile(buenos[1], []byte("b"), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(m.volSnapsDir("datos"), 0o755)
	m.barrerTmpVolumenes()
	for _, r := range restos {
		if _, err := os.Stat(r); !os.IsNotExist(err) {
			t.Errorf("%s sigue ahí", filepath.Base(r))
		}
	}
	for _, b := range buenos {
		if _, err := os.Stat(b); err != nil {
			t.Errorf("se llevó %s", filepath.Base(b))
		}
	}
	if fi, _ := os.Stat(m.volSnapsDir("datos")); fi.Mode().Perm() != 0o700 {
		t.Errorf("no devolvió el directorio a 0700: %v", fi.Mode().Perm())
	}
}

// Sin sitio para una copia completa, 507 y nada a medias.
func TestCopiaCompletaSinSitioEs507(t *testing.T) {
	m := newTestManager(t)
	escribirVolumen(t, m, "datos", strings.Repeat("x", 1<<16))
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "999999999")
	err := m.cabeCopia(m.volumePath("datos"))
	if codigo(err) != http.StatusInsufficientStorage {
		t.Fatalf("quería 507, dio %v", err)
	}
	if !strings.Contains(err.Error(), "reflink") {
		t.Errorf("el error debería explicar la salida (reflink): %v", err)
	}
	t.Setenv("KLING_MIN_FREE_DISK_MIB", "0")
	t.Setenv("KLING_GC_DISK_HIGH", "100")
	if err := m.cabeCopia(m.volumePath("datos")); err != nil {
		t.Errorf("64 KiB deberían caber: %v", err)
	}
}
