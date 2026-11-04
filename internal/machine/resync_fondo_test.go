package machine

import (
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// maquinaEnFondo da de alta una máquina running cuyo agente es srvURL.
func maquinaEnFondo(t *testing.T, m *Manager, srvURL string) string {
	t.Helper()
	id := newID()
	mc := m.addForTest(id)
	empezo := time.Now()
	m.mu.Lock()
	mc.State, mc.StartedAt, mc.VMM, mc.PID = api.StateRunning, &empezo, "kling-hv 0.20.1", 4242
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(srvURL, "http://")}
	mc.Agent = &api.GuestAgent{Caps: []string{api.GuestCapResync, guest.CapResyncClock}}
	m.mu.Unlock()
	return id
}

// Un resync en segundo plano no se manda si, cuando le toca, la máquina ya no
// está en la ejecución en la que se pidió: congelada (o parada) mientras
// esperaba el cerrojo, o despertada otra vez.
func TestResyncEnFondoOtraEjecucion(t *testing.T) {
	m := newTestManager(t)
	agente, pedidos := agenteReloj(t, nil)
	id := maquinaEnFondo(t, m, agente.URL)
	ej, ok := m.ejecucionDe(id)
	if !ok {
		t.Fatal("running machine without an execution")
	}

	// El Freeze tiene el cerrojo: el resync espera y, al soltarlo, la
	// máquina ya es un volcado.
	soltar := m.lock(id)
	hecho := make(chan struct{})
	go func() {
		m.resyncEnFondo(id, ej, guest.ResyncResume)
		close(hecho)
	}()
	select {
	case <-hecho:
		t.Fatal("the resync did not wait for the lifecycle lock")
	case <-time.After(50 * time.Millisecond):
	}
	m.mu.Lock()
	m.byID[id].State = api.StateWarm
	m.mu.Unlock()
	soltar()
	<-hecho
	select {
	case req := <-pedidos:
		t.Fatalf("resync %+v sent to a frozen machine", req)
	default:
	}

	// Despertada otra vez: otra ejecución, aunque esté running.
	otra := time.Now().Add(time.Second)
	m.mu.Lock()
	m.byID[id].State, m.byID[id].StartedAt = api.StateRunning, &otra
	m.mu.Unlock()
	m.resyncEnFondo(id, ej, guest.ResyncResume)
	select {
	case req := <-pedidos:
		t.Fatalf("resync %+v from an earlier execution", req)
	default:
	}

	// La misma ejecución sí lo recibe.
	ej, _ = m.ejecucionDe(id)
	m.resyncEnFondo(id, ej, guest.ResyncResume)
	select {
	case req := <-pedidos:
		if req.Restore != guest.ResyncResume {
			t.Fatalf("Restore = %q", req.Restore)
		}
	default:
		t.Fatal("no resync for the same execution")
	}
}

// Con un agente que no contesta, un Freeze que llega durante el resync no le
// espera el resyncPlazo entero: el resync cede pasado resyncCedeMax.
func TestResyncEnFondoCedeAlFreeze(t *testing.T) {
	m := newTestManager(t)
	colgado := make(chan struct{})
	defer close(colgado)
	agente, pedidos := agenteReloj(t, colgado)
	id := maquinaEnFondo(t, m, agente.URL)
	ej, _ := m.ejecucionDe(id)

	hecho := make(chan struct{})
	go func() {
		m.resyncEnFondo(id, ej, guest.ResyncResume)
		close(hecho)
	}()
	select {
	case <-pedidos:
	case <-time.After(3 * time.Second):
		t.Fatal("no resync reached the agent")
	}
	start := time.Now()
	soltar := m.lock(id)
	espera := time.Since(start)
	soltar()
	<-hecho
	if espera >= resyncPlazo/2 {
		t.Fatalf("the freeze waited %s for a hung resync", espera)
	}
	if espera < resyncCedeMax/2 {
		t.Fatalf("the freeze got the lock in %s: the resync did not hold it", espera)
	}
}
