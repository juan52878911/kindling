//go:build darwin

package share

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Una sesión que mira muchas rutas no se queda con los descriptores de
// vigilancia de todo el daemon: pasado su tope suelta las menos recientes y
// deja sitio a las demás sesiones.
func TestKqueueTopePorSesion(t *testing.T) {
	dir := t.TempDir()
	n := cap(watchFDs)/2 + 10
	for i := range n {
		must(t, os.WriteFile(filepath.Join(dir, "f"+strconv.Itoa(i)), nil, 0o644))
	}
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	antes := len(watchFDs)
	w, err := newWatcher(srv, func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	for i := range n {
		w.note("f"+strconv.Itoa(i), false)
	}
	if usados := len(watchFDs) - antes; usados > cap(watchFDs)/4 {
		t.Fatalf("one session holds %d of the daemon's %d watch descriptors, want at most %d", usados, cap(watchFDs), cap(watchFDs)/4)
	}
	// La más reciente sigue vigilada.
	if w.be.(*kqBackend).byRel["f"+strconv.Itoa(n-1)] == nil {
		t.Fatal("the most recent path is not watched")
	}
}
