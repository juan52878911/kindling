package machine

import "testing"

// killOrphanVMMs recorre /proc (o lanza ps en macOS) SIN m.mu: con el candado
// global tomado, ese escaneo congelaba ps, run y thaw mientras duraba.
func TestKillOrphanVMMsEscaneaSinElCandado(t *testing.T) {
	m := newTestManager(t)
	escaneos, libre := 0, true
	m.pruebasEscaneoHuerfanos = func() {
		escaneos++
		if m.mu.TryLock() {
			m.mu.Unlock()
		} else {
			libre = false
		}
	}
	m.reconcile()
	if escaneos == 0 {
		t.Fatal("reconcile no buscó VMMs huérfanos")
	}
	if !libre {
		t.Fatal("killOrphanVMMs escaneó los procesos con m.mu tomado")
	}
}
