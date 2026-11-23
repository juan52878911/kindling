package machine

import (
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// A-1 paso 1: persistirYa publica la foto con write+rename, sin fsync en el
// camino de quien lanza el VMM; el fsync lo hace la escritura con debounce.
func TestPersistirYaNoHaceFsyncYLoDejaParaElDebounce(t *testing.T) {
	m := newTestManager(t)
	// Una foto ya asentada: sin ella no hay de dónde recuperar un state.json
	// roto y persistirYa escribe con fsync (R3-1).
	m.mu.Lock()
	m.persist()
	m.mu.Unlock()
	if err := m.writePending(); err != nil {
		t.Fatal(err)
	}
	var completas, fsyncs atomic.Int32
	prevE, prevF := escribirEstado, fsyncEstado
	escribirEstado = func(ruta string, datos []byte, perm os.FileMode) error {
		completas.Add(1)
		return prevE(ruta, datos, perm)
	}
	fsyncEstado = func(ruta string) error { fsyncs.Add(1); return prevF(ruta) }
	t.Cleanup(func() { escribirEstado, fsyncEstado = prevE, prevF })

	m.addForTest("ab00000000000002")
	if err := m.persistirYa(); err != nil {
		t.Fatal(err)
	}
	if got := readState(t, m); len(got) != 1 || got[0].ID != "ab00000000000002" {
		t.Fatalf("la foto no está publicada: %+v", got)
	}
	if c := completas.Load(); c != 0 {
		t.Fatalf("persistirYa usó la escritura con fsync (%d veces)", c)
	}
	plazo := time.Now().Add(3 * time.Second)
	for fsyncs.Load() == 0 {
		if time.Now().After(plazo) {
			t.Fatal("el fsync diferido no llegó")
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if f := fsyncs.Load(); f != 1 {
		t.Fatalf("fsync diferido %d veces, quería 1", f)
	}
}
