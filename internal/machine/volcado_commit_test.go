package machine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// M15: el volcado temporal de un commit cortado (.commit-mem.file, del tamaño
// de la RAM) no se queda en el directorio de la máquina: lo barren reconcile
// al arrancar el daemon y Stop.
func TestVolcadoTemporalDeCommitSeBarre(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	plantar := func(id string) {
		t.Helper()
		for _, n := range []string{volcadoTmpMem, volcadoTmpSnap} {
			if err := os.WriteFile(filepath.Join(m.dir(id), n), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	quedan := func(id string) bool {
		_, err1 := os.Lstat(filepath.Join(m.dir(id), volcadoTmpMem))
		_, err2 := os.Lstat(filepath.Join(m.dir(id), volcadoTmpSnap))
		return err1 == nil || err2 == nil
	}

	// reconcile: una parada con el temporal de antes del reinicio.
	a := newID()
	m.byID[a] = &api.Machine{ID: a, Name: "a", State: api.StateStopped}
	if err := os.MkdirAll(m.dir(a), 0o755); err != nil {
		t.Fatal(err)
	}
	plantar(a)
	m.reconcile()
	if quedan(a) {
		t.Fatal("reconcile dejó el volcado temporal del commit")
	}

	// Stop: una running sin VMM (el kill no tiene a quién matar).
	b := newID()
	m.byID[b] = &api.Machine{ID: b, Name: "b", State: api.StateRunning}
	if err := os.MkdirAll(m.dir(b), 0o755); err != nil {
		t.Fatal(err)
	}
	plantar(b)
	if _, err := m.Stop(b); err != nil {
		t.Fatal(err)
	}
	if quedan(b) {
		t.Fatal("Stop dejó el volcado temporal del commit")
	}
	if _, err := os.Stat(m.dir(b)); err != nil {
		t.Fatalf("Stop se llevó el directorio: %v", err)
	}
}
