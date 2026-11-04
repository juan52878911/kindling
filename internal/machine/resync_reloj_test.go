package machine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
