package machine

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Mientras Freeze vuelca, la máquina sigue figurando running: lo que se ve
// desde fuera (List, Get) tiene que decir que está a medio congelar, y dejar
// de decirlo al acabar. Sin la marca, el planificador del gateway la adoptaba.
func TestFreezeSeVeEnTransicion(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	m.addForTest(id)
	if err := os.MkdirAll(m.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	falso := nuevoFcFalso(t)
	m.mu.Lock()
	m.socket[id] = falso.Sock
	m.mu.Unlock()

	var (
		mu    sync.Mutex
		vista string
		visto bool
	)
	falso.enGancho(func(metodo, ruta string) {
		if metodo != http.MethodPatch || ruta != "/vm" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if visto {
			return
		}
		visto = true
		for _, mc := range m.List() {
			if mc.ID == id {
				vista = mc.Transition
			}
		}
	})

	if _, err := m.Freeze(context.Background(), id); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !visto {
		t.Fatal("Freeze no llegó a pausar")
	}
	if vista != api.TransitionFreezing {
		t.Fatalf("durante el volcado List dijo transition=%q; quería %q", vista, api.TransitionFreezing)
	}
	mc, _ := m.Get(id)
	if mc.State != api.StateWarm || mc.Transition != "" {
		t.Fatalf("tras congelar: state=%s transition=%q", mc.State, mc.Transition)
	}
	m.persistirYa()
	for _, p := range readState(t, m) {
		if p.Transition != "" {
			t.Fatalf("la transición llegó al estado persistido: %+v", p)
		}
	}
}
