package machine

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func estadoEnDisco(t *testing.T, m *Manager, id string) api.State {
	t.Helper()
	for _, mc := range readState(t, m) {
		if mc.ID == id {
			return mc.State
		}
	}
	return ""
}

// Close espera a la operación de ciclo de vida en curso antes de cerrar la
// escritura del estado: la transición con la que acaba (un Freeze que apunta
// la máquina warm) llega a state.json. Antes Close cerraba persistLoop por
// debajo y esa última escritura se perdía.
func TestCloseEsperaALaOperacionEnCurso(t *testing.T) {
	m := newTestManager(t)
	mc := m.addForTest(newID())

	soltar := m.lock(mc.ID) // un Freeze a medias
	cerrado := make(chan struct{})
	go func() {
		m.Close()
		close(cerrado)
	}()
	select {
	case <-cerrado:
		t.Fatal("Close volvió con una operación de ciclo de vida en curso")
	case <-time.After(100 * time.Millisecond):
	}

	m.mu.Lock()
	m.byID[mc.ID].State = api.StateWarm
	m.persist()
	m.mu.Unlock()
	soltar()

	select {
	case <-cerrado:
	case <-time.After(5 * time.Second):
		t.Fatal("Close no volvió tras acabar la operación")
	}
	if got := estadoEnDisco(t, m, mc.ID); got != api.StateWarm {
		t.Fatalf("state.json dice %q: se perdió la última transición", got)
	}
}

// Una operación que acaba DESPUÉS de Close (pasado su plazo) escribe ella
// misma su foto al soltar el cerrojo: persistLoop ya no está para hacerlo.
func TestOperacionTrasCloseAunSeEscribe(t *testing.T) {
	m := newTestManager(t)
	mc := m.addForTest(newID())
	m.Close()

	soltar := m.lock(mc.ID)
	m.mu.Lock()
	m.byID[mc.ID].State = api.StateStopped
	m.persist()
	m.mu.Unlock()
	soltar()

	if got := estadoEnDisco(t, m, mc.ID); got != api.StateStopped {
		t.Fatalf("state.json dice %q tras una operación acabada después de Close", got)
	}
}

func TestEsperarLibresConPlazo(t *testing.T) {
	c := nuevosCerrojos()
	if !c.esperarLibres(0) {
		t.Fatal("sin cerrojos tomados no hay nada que esperar")
	}
	soltar := c.tomar("x")
	inicio := time.Now()
	if c.esperarLibres(50 * time.Millisecond) {
		t.Fatal("dijo libre con un cerrojo tomado")
	}
	if time.Since(inicio) > 2*time.Second {
		t.Fatal("no respetó el plazo")
	}
	soltar()
	if !c.esperarLibres(time.Second) {
		t.Fatal("no vio el cerrojo soltado")
	}
}
