package machine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un thaw que pierde la carrera con un rm (la máquina desaparece mientras
// espera su cerrojo) es un "no existe": el daemon lo contesta con un 404 y el
// planificador (pkg/scheduler/aislada.go) da la sesión por perdida. Con un
// error sin marcar era un 400 y el planificador no lo reconocía.
func TestThawPierdeLaCarreraConRm(t *testing.T) {
	m := newTestManager(t)
	mc := m.addForTest("7aa0000000000001")
	m.mu.Lock()
	mc.State = api.StateWarm
	m.mu.Unlock()

	soltar := m.lock(mc.ID)
	hecho := make(chan error, 1)
	go func() {
		_, err := m.thaw(context.Background(), mc.ID)
		hecho <- err
	}()
	// Esperar a que el thaw ya resolviera la referencia y espere el cerrojo.
	for plazo := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		m.lifecycle.mu.Lock()
		refs := m.lifecycle.m[mc.ID].refs
		m.lifecycle.mu.Unlock()
		if refs == 2 {
			break
		}
		if time.Now().After(plazo) {
			t.Fatal("el thaw no llegó a esperar el cerrojo")
		}
	}
	m.mu.Lock()
	delete(m.byID, mc.ID)
	m.mu.Unlock()
	soltar()

	if err := <-hecho; !errors.Is(err, ErrNoMachine) {
		t.Fatalf("thaw tras el rm: %v, quería ErrNoMachine", err)
	}
}
