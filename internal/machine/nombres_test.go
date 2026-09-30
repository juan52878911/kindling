package machine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func (m *Manager) addNombradaForTest(id, nombre string) {
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: nombre, State: api.StateRunning, CreatedAt: time.Now()}
	m.mu.Unlock()
}

// Un nombre que llevan dos máquinas (un estado de antes de que fueran únicos)
// no resuelve a ninguna: antes se devolvía la primera del mapa, al azar, y la
// autorización por nombre podía caer sobre la de otro inquilino.
func TestGetNombreAmbiguoNoResuelve(t *testing.T) {
	m := newTestManager(t)
	m.addNombradaForTest("aaaa000000000001", "web")
	m.addNombradaForTest("bbbb000000000002", "web")
	for i := 0; i < 20; i++ {
		if mc, ok := m.Get("web"); ok {
			t.Fatalf("Get(web) with two machines named web resolved to %s", mc.ID)
		}
	}
	// El ID sigue llevando a cada una.
	if mc, ok := m.Get("bbbb000000000002"); !ok || mc.Name != "web" {
		t.Fatalf("Get by full ID: %v %v", mc, ok)
	}
}

// El nombre exacto gana a un prefijo de ID, y un prefijo que casa con dos no
// resuelve.
func TestGetNombreAntesQuePrefijo(t *testing.T) {
	m := newTestManager(t)
	m.addNombradaForTest("abcd000000000001", "uno")
	m.addNombradaForTest("ffff000000000002", "abcd")
	for i := 0; i < 20; i++ {
		mc, ok := m.Get("abcd")
		if !ok || mc.ID != "ffff000000000002" {
			t.Fatalf("Get(abcd) = %v %v, want the machine named abcd", mc, ok)
		}
	}
	m.addNombradaForTest("abce000000000003", "tres")
	if mc, ok := m.Get("abc"); ok {
		t.Fatalf("a 3-char prefix resolved to %s", mc.ID)
	}
	m.addNombradaForTest("1234000000000004", "cuatro")
	m.addNombradaForTest("1234000000000005", "cinco")
	if mc, ok := m.Get("1234"); ok {
		t.Fatalf("an ambiguous prefix resolved to %s", mc.ID)
	}
}

// Crear con un nombre que ya lleva otra máquina es un 409, antes de reservar
// ni arrancar nada.
func TestRunRechazaNombreRepetido(t *testing.T) {
	m := newTestManager(t)
	m.addNombradaForTest("aaaa000000000001", "web")
	for _, req := range []api.RunRequest{
		{Name: "web", Image: "default"},
		{Name: "web", From: "plantilla"},
		{Name: "aaaa000000000001", Image: "default"}, // el ID de otra
	} {
		_, err := m.Run(context.Background(), req)
		if !errors.Is(err, ErrNameTaken) {
			t.Fatalf("Run(%+v) = %v, want ErrNameTaken", req, err)
		}
	}
	if n := m.Count(); n != 1 {
		t.Fatalf("%d machines after the rejected runs, want 1", n)
	}
}

// Dos creaciones simultáneas con el mismo nombre: la reserva deja pasar a una.
func TestReservarNombreExcluye(t *testing.T) {
	m := newTestManager(t)
	soltar, err := m.reservarNombre("web")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.reservarNombre("web"); !errors.Is(err, ErrNameTaken) {
		t.Fatalf("second reservation: %v, want ErrNameTaken", err)
	}
	soltar()
	soltar2, err := m.reservarNombre("web")
	if err != nil {
		t.Fatalf("after releasing: %v", err)
	}
	soltar2()
	s, err := m.reservarNombre("")
	if err != nil {
		t.Fatalf("empty name: %v", err)
	}
	s()
}
