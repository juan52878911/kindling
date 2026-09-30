package machine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Un state.json roto no es un estado vacío: se aparta, no se pisa, y mientras
// esté apartado el barrido no manda a la papelera directorios que el daemon
// "no conoce" (son justo las máquinas que no pudo leer).
func TestEstadoIlegibleSeApartaYNoSeBorraNada(t *testing.T) {
	m := newTestManager(t)
	if err := os.WriteFile(m.statePath(), []byte(`[{"id":"aa11bb22cc33dd44","na`), 0o644); err != nil {
		t.Fatal(err)
	}
	dirMaquina := m.dir("aa11bb22cc33dd44")
	if err := os.MkdirAll(dirMaquina, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirMaquina, "overlay.ext4"), []byte("datos del usuario"), 0o644); err != nil {
		t.Fatal(err)
	}
	envejecer(t, dirMaquina)

	m.load()
	if !m.barridoBloqueado() {
		t.Fatal("con state.json ilegible el daemon no entró en modo protegido")
	}
	apartados, _ := filepath.Glob(m.statePath() + sufijoCuarentena + "*")
	if len(apartados) != 1 {
		t.Fatalf("el fichero roto no se apartó: %v", apartados)
	}
	if b, _ := os.ReadFile(apartados[0]); !strings.Contains(string(b), "aa11bb22cc33dd44") {
		t.Errorf("lo apartado no es el fichero original: %q", b)
	}

	m.mu.Lock()
	m.sweepMachineDirs()
	m.mu.Unlock()
	m.vaciarPapelera()
	if _, err := os.Stat(filepath.Join(dirMaquina, "overlay.ext4")); err != nil {
		t.Fatalf("el barrido borró una máquina que el estado roto no dejó leer: %v", err)
	}

	// El daemon escribe un state.json nuevo (vacío, lo único que sabe). Al
	// reiniciar, ese fichero SÍ se lee, pero la cuarentena sigue ahí: el modo
	// protegido también.
	m.mu.Lock()
	m.persist()
	m.mu.Unlock()
	m.writePending()
	m2 := newTestManager(t)
	m2.root = m.root
	m2.load()
	if !m2.barridoBloqueado() {
		t.Error("tras reiniciar con un estado apartado, el barrido volvió a estar activo")
	}
	m2.mu.Lock()
	m2.sweepMachineDirs()
	m2.mu.Unlock()
	if _, err := os.Stat(dirMaquina); err != nil {
		t.Fatalf("tras reiniciar, el barrido borró la máquina: %v", err)
	}

	// Retirada la cuarentena, vuelve la recogida.
	os.Remove(apartados[0])
	m3 := newTestManager(t)
	m3.root = m.root
	m3.load()
	if m3.barridoBloqueado() {
		t.Error("sin nada apartado sigue en modo protegido")
	}
}

// Sin state.json (primera arrancada) no hay nada que proteger.
func TestSinEstadoNoHayModoProtegido(t *testing.T) {
	m := newTestManager(t)
	m.load()
	if m.barridoBloqueado() {
		t.Error("sin state.json entró en modo protegido")
	}
}
