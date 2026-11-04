package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// agenteReloj es un agente falso que apunta el Restore de cada /resync y,
// si soltar no es nil, no contesta hasta que se cierre.
func agenteReloj(t *testing.T, soltar chan struct{}) (*httptest.Server, chan api.GuestResync) {
	t.Helper()
	pedidos := make(chan api.GuestResync, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != api.GuestResyncPath {
			http.NotFound(w, r)
			return
		}
		var req api.GuestResync
		_ = json.NewDecoder(r.Body).Decode(&req)
		pedidos <- req
		if soltar != nil {
			<-soltar
		}
		_, _ = w.Write([]byte(`{"skew_ms":0}`))
	}))
	t.Cleanup(srv.Close)
	return srv, pedidos
}

// Reanudar una pausada pone su reloj en hora (kling-hv para el contador del
// invitado durante la pausa), pero en segundo plano: reanudarLocked vuelve sin
// esperar al /resync. Y como "resume", que el agente no cuenta como
// restauración.
func TestReanudarPoneElRelojEnSegundoPlano(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	soltar := make(chan struct{})
	defer close(soltar)
	agente, pedidos := agenteReloj(t, soltar)

	id := newID()
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	pausada := time.Now().Add(-time.Minute)
	m.mu.Lock()
	mc.State, mc.FrozenAt = api.StatePaused, &pausada
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(agente.URL, "http://")}
	mc.Agent = &api.GuestAgent{Caps: []string{api.GuestCapResync, guest.CapResyncClock}}
	m.socket[id] = falso.Sock
	m.mu.Unlock()

	cur, _ := m.Get(id)
	hecho := make(chan error, 1)
	go func() {
		_, err := m.reanudarLocked(context.Background(), cur, nil)
		hecho <- err
	}()
	select {
	case err := <-hecho:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("resuming waited for the guest's /resync")
	}
	select {
	case req := <-pedidos:
		if req.Restore != guest.ResyncResume {
			t.Fatalf("Restore = %q, want %q", req.Restore, guest.ResyncResume)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no /resync after resuming")
	}
}

// Un agente anterior (sin guest.CapResyncClock) no recibe nada al reanudar
// salvo si el host durmió (TestReanudarTrasDormirResincroniza): lo tomaría
// por una restauración.
func TestReanudarAgenteAnteriorSinResync(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	agente, pedidos := agenteReloj(t, nil)
	id := newID()
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	pausada := time.Now().Add(-time.Minute)
	m.mu.Lock()
	mc.State, mc.FrozenAt = api.StatePaused, &pausada
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(agente.URL, "http://")}
	mc.Agent = &api.GuestAgent{Caps: []string{api.GuestCapResync}}
	m.socket[id] = falso.Sock
	m.mu.Unlock()
	cur, _ := m.Get(id)
	if _, err := m.reanudarLocked(context.Background(), cur, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case req := <-pedidos:
		t.Fatalf("resync %+v to an agent that predates resume", req)
	case <-time.After(200 * time.Millisecond):
	}
}

// Tras un arranque en frío, en cuanto el agente contesta: un /resync "boot"
// (el RTC del invitado va a segundos), solo si el agente lo entiende.
func TestResyncArranque(t *testing.T) {
	m := newTestManager(t)
	agente, pedidos := agenteReloj(t, nil)
	id := newID()
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.State = api.StateRunning
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(agente.URL, "http://")}
	m.mu.Unlock()

	m.resyncArranque(id, &api.GuestAgent{Caps: []string{api.GuestCapResync}})
	select {
	case req := <-pedidos:
		t.Fatalf("boot resync %+v to an agent that predates it", req)
	default:
	}
	antes := time.Now()
	m.resyncArranque(id, &api.GuestAgent{Caps: []string{api.GuestCapResync, guest.CapResyncClock}})
	select {
	case req := <-pedidos:
		if req.Restore != guest.ResyncBoot {
			t.Fatalf("Restore = %q, want %q", req.Restore, guest.ResyncBoot)
		}
		// La hora se toma al mandar el cuerpo, no antes de conectar.
		if h := time.Unix(0, req.UnixNano); h.Before(antes) || time.Since(h) > time.Second {
			t.Fatalf("host time %v, sent at %v", h, antes)
		}
	default:
		t.Fatal("no boot resync")
	}
}

// El cuerpo de /resync anuncia su longitud y toma la hora al leerse.
func TestCuerpoResyncHoraAlLeer(t *testing.T) {
	c, err := nuevoCuerpoResync(make([]byte, api.GuestResyncEntropy), guest.ResyncBoot)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	antes := time.Now()
	b := make([]byte, 4096)
	n, _ := c.Read(b)
	if n != c.n {
		t.Fatalf("read %d bytes, announced %d", n, c.n)
	}
	var req api.GuestResync
	if err := json.Unmarshal(b[:n], &req); err != nil {
		t.Fatal(err)
	}
	if time.Unix(0, req.UnixNano).Before(antes) {
		t.Fatalf("time %v taken before the read (%v)", time.Unix(0, req.UnixNano), antes)
	}
}
