//go:build linux

package share

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Las instancias de inotify son del uid, no del proceso: el daemon no pasa de
// su presupuesto, las sesiones de más vigilan por sondeo (y siguen avisando),
// y al cerrar una se devuelve su instancia.
func TestPresupuestoInotify(t *testing.T) {
	old := pollInterval
	pollInterval = 20 * time.Millisecond
	defer func() { pollInterval = old }()
	inotifyBudget.Lock()
	prevMax := inotifyBudget.max
	inotifyBudget.max = inotifyBudget.used + 1
	inotifyBudget.Unlock()
	defer func() {
		inotifyBudget.Lock()
		inotifyBudget.max = prevMax
		inotifyBudget.Unlock()
	}()

	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1"), 0o644))
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	r1 := &recogedor{t: t}
	w1, err := newWatcher(srv, r1.push)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := w1.be.(*inBackend); !ok || w1.polling {
		t.Fatalf("first session: backend %T, want inotify", w1.be)
	}
	r2 := &recogedor{t: t}
	w2, err := newWatcher(srv, r2.push)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.close()
	if _, ok := w2.be.(*pollBackend); !ok || !w2.polling {
		t.Fatalf("over budget: backend %T, want polling", w2.be)
	}
	// La que sondea también se entera de un cambio del host.
	w2.note("f.txt", false)
	must(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("version 2"), 0o644))
	if got := r2.espera(1, 2*time.Second); len(got) != 1 || got[0] != "M f.txt" {
		t.Fatalf("polling session pushes %v, want [M f.txt]", got)
	}

	w1.close()
	w3, err := newWatcher(srv, func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	defer w3.close()
	if _, ok := w3.be.(*inBackend); !ok {
		t.Fatalf("after closing one: backend %T, want inotify again", w3.be)
	}
}
