package machine

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// Dos commit con el mismo nombre desde máquinas distintas: m.lock es por
// máquina y reserveDir cuenta, no cierra, así que los dos pasaban el Stat y
// volcaban en el mismo directorio. El que fallaba borraba al limpiar los
// ficheros del otro. Ahora el segundo en crear el directorio falla como
// conflicto y no toca nada.
func TestCommitsConElMismoNombreNoSePisan(t *testing.T) {
	m := newTestManager(t)
	const ga, gb = "c0aa170000000101", "c0aa170000000102"
	fa := plantillaSinProceso(t, m, ga)
	fb := plantillaSinProceso(t, m, gb)
	dir := m.snapDir("dorado")
	testigo := filepath.Join(dir, "snap.file")

	// B se para dentro de la ventana: ha visto el nombre libre y aún no ha
	// creado el directorio. Solo la primera llamada (la de B) espera; con un
	// sync.Once la de A esperaría también a que acabe la de B.
	bEnVentana, seguirB := make(chan struct{}), make(chan struct{})
	var primera atomic.Bool
	antesDeCrearDirSnapshot = func(string) {
		if primera.CompareAndSwap(false, true) {
			close(bEnVentana)
			<-seguirB
		}
	}
	t.Cleanup(func() { antesDeCrearDirSnapshot = nil })

	// A vuelca: escribe su estado y se queda ahí hasta que B haya acabado.
	// Luego falla, para que el test no dependa de fallocate.
	aVolcando, seguirA := make(chan struct{}), make(chan struct{})
	fa.fallar(http.MethodPut, "/snapshot/create", http.StatusBadRequest, "fin de la prueba")
	fa.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			_ = os.WriteFile(testigo, []byte("de A"), 0o644)
			close(aVolcando)
			<-seguirA
		}
	})
	// Sin el arreglo B llegaría a volcar: que falle, y su limpieza borre.
	fb.fallar(http.MethodPut, "/snapshot/create", http.StatusBadRequest, "disco lleno")

	errB := make(chan error, 1)
	go func() {
		_, err := m.Commit(context.Background(), gb, "dorado", false)
		errB <- err
	}()
	esperarCanal(t, bEnVentana, "B no llegó a la ventana")

	errA := make(chan error, 1)
	go func() {
		_, err := m.Commit(context.Background(), ga, "dorado", false)
		errA <- err
	}()
	esperarCanal(t, aVolcando, "A no llegó a volcar")

	close(seguirB)
	select {
	case err := <-errB:
		if !errors.Is(err, ErrCommitConflict) {
			t.Errorf("el segundo commit = %v, quería ErrCommitConflict", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("el commit de B no terminó")
	}
	if b, err := os.ReadFile(testigo); err != nil || string(b) != "de A" {
		t.Errorf("el commit perdedor tocó el volcado del otro: %q, %v", b, err)
	}
	if n := len(fb.llamadasA(http.MethodPut, "/snapshot/create")); n != 0 {
		t.Errorf("B volcó %d veces en un directorio que no era suyo", n)
	}

	close(seguirA)
	select {
	case <-errA:
	case <-time.After(10 * time.Second):
		t.Fatal("el commit de A no terminó")
	}
}

func esperarCanal(t *testing.T, c <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(10 * time.Second):
		t.Fatal(msg)
	}
}

// La marca de fork se escribe antes que el meta.json: con el meta ya en disco
// el snapshot no es un resto de commit, y sin la marca barrerForks tampoco lo
// ve. Antes Fork la escribía después de commit, y un corte entre las dos
// dejaba un dorado temporal que no recogía nadie.
func TestCommitDeForkMarcaAntesDelMeta(t *testing.T) {
	if _, err := exec.LookPath("fallocate"); err != nil && !volcadoEnLaMaquina {
		t.Skip("sin fallocate no se puede perforar el volcado")
	}
	m := newTestManager(t)
	const id = "c0aa170000000103"
	falso := plantillaSinProceso(t, m, id)
	if err := os.WriteFile(m.KernelPath(), []byte("vmlinux de prueba"), 0o644); err != nil {
		t.Fatal(err)
	}
	name := nombreSnapshotFork(id)
	dir := m.snapDir(name)
	// Donde escribiría el VMM: en macOS, en el directorio de su máquina, y el
	// daemon lo publica (publicar_volcado.go); en Linux, en el del snapshot.
	snapTmp, memTmp := filepath.Join(dir, "snap.file"), filepath.Join(dir, "mem.file")
	if volcadoEnLaMaquina {
		snapTmp, memTmp = filepath.Join(m.dir(id), volcadoTmpSnap), filepath.Join(m.dir(id), volcadoTmpMem)
	}
	falso.enGancho(func(metodo, ruta string) {
		if ruta == "/snapshot/create" {
			_ = os.WriteFile(snapTmp, []byte("estado"), 0o644)
			_ = os.WriteFile(memTmp, make([]byte, 1<<20), 0o644)
		}
	})

	if _, err := m.commit(context.Background(), id, name, false, nil, false, true, false); err != nil {
		t.Fatalf("commit de fork: %v", err)
	}
	marca, err := os.Stat(filepath.Join(dir, forkMarca))
	if err != nil {
		t.Fatalf("commit de fork no dejó la marca: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, forkMarca)); string(b) != id+"\n" {
		t.Errorf("la marca dice %q, quería el id del original", b)
	}
	meta, err := os.Stat(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if marca.ModTime().After(meta.ModTime()) {
		t.Errorf("la marca (%s) es posterior al meta.json (%s): un corte entre los dos deja un fork huérfano",
			marca.ModTime().Format(time.RFC3339Nano), meta.ModTime().Format(time.RFC3339Nano))
	}
}
