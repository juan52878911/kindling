package machine

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// R3-1: persistirYa publica state.json sin fsync. Si el host se cae antes del
// fsync con debounce, un sistema de ficheros sin flush al renombrar puede
// dejarlo vacío. Antes eso era cero máquinas y modo protegido; ahora load
// recupera la última foto asentada (state.json.prev).
func TestEstadoTruncadoRecuperaLaFotoAsentada(t *testing.T) {
	m := newTestManager(t)
	asentada, nueva := "aa00000000000001", "aa00000000000002"
	m.addForTest(asentada)
	if err := m.persistirYa(); err != nil {
		t.Fatal(err)
	}
	// El fsync que la escritura con debounce hace después: esta foto queda
	// asentada.
	if err := m.writePending(); err != nil {
		t.Fatal(err)
	}
	m.addForTest(nueva)
	if err := m.persistirYa(); err != nil { // write+rename sin fsync
		t.Fatal(err)
	}
	m.Close()

	// El corte de luz: el rename llegó a disco y los datos no.
	if err := os.Truncate(m.statePath(), 0); err != nil {
		t.Fatal(err)
	}

	m2 := newTestManager(t)
	m2.root = m.root
	m2.load()
	if m2.barridoBloqueado() {
		t.Fatalf("con la foto anterior en disco el daemon entró en modo protegido: %s", m2.estadoIlegible)
	}
	if apartados, _ := filepath.Glob(m2.statePath() + sufijoCuarentena + "*"); len(apartados) != 0 {
		t.Fatalf("se apartó a cuarentena teniendo de dónde recuperar: %v", apartados)
	}
	m2.mu.RLock()
	_, hay := m2.byID[asentada]
	m2.mu.RUnlock()
	if !hay {
		t.Fatal("no se recuperó la máquina de la foto asentada")
	}
}

// Si state.json y la foto anterior están rotos los dos, no hay de dónde
// recuperar: cuarentena y modo protegido, como antes.
func TestEstadoYPrevioRotosVanACuarentena(t *testing.T) {
	m := newTestManager(t)
	for _, p := range []string{m.statePath(), m.statePath() + ".prev"} {
		if err := os.WriteFile(p, []byte(`{"version":`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m.load()
	if !m.barridoBloqueado() {
		t.Fatal("con los dos estados rotos el daemon no entró en modo protegido")
	}
}

// Sin enlaces duros (o sin foto anterior, la primera vez) no hay de dónde
// recuperar: persistirYa escribe con fsync, como antes de A-1. Más lento, no
// menos seguro.
func TestPersistirYaSinEnlacesEscribeConFsync(t *testing.T) {
	m := newTestManager(t)
	var completas atomic.Int32
	prevE, prevL := escribirEstado, enlazarEstadoPrevio
	escribirEstado = func(ruta string, datos []byte, perm os.FileMode) error {
		completas.Add(1)
		return prevE(ruta, datos, perm)
	}
	t.Cleanup(func() { escribirEstado, enlazarEstadoPrevio = prevE, prevL })

	// La primera: no hay foto que enlazar.
	m.addForTest("aa00000000000011")
	if err := m.persistirYa(); err != nil {
		t.Fatal(err)
	}
	if completas.Load() != 1 {
		t.Fatalf("la primera foto, sin anterior, se escribió sin fsync (%d con fsync)", completas.Load())
	}
	// Un sistema de ficheros sin enlaces duros.
	enlazarEstadoPrevio = func(string) error { return errors.New("operation not supported") }
	m.addForTest("aa00000000000012")
	if err := m.persistirYa(); err != nil {
		t.Fatal(err)
	}
	if completas.Load() != 2 {
		t.Fatalf("sin enlaces duros se escribió sin fsync (%d con fsync)", completas.Load())
	}
	if _, err := os.Stat(m.statePath() + sufijoPrevio); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quedó un .prev sin poder enlazarlo: %v", err)
	}
}
