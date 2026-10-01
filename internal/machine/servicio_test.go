package machine

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// TestStopService: el daemon pide parar el servicio solo a un agente que
// anuncia "service" y a una máquina que corre.
func TestStopService(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == api.GuestServiceStopPath {
			hits.Add(1)
		}
		w.Write([]byte(`{"declared":true}`))
	}))
	defer srv.Close()
	m := &Manager{}
	mc := func(state api.State, ag *api.GuestAgent) *api.Machine {
		return &api.Machine{Name: "pg", State: state, Agent: ag,
			Forwards: map[string]string{strconv.Itoa(api.GuestPort): strings.TrimPrefix(srv.URL, "http://")}}
	}
	con := &api.GuestAgent{Caps: []string{api.GuestCapReady, api.GuestCapService}}
	sin := &api.GuestAgent{Caps: []string{api.GuestCapReady}}
	for _, c := range []struct {
		mc   *api.Machine
		want int32
	}{
		{mc(api.StateRunning, con), 1},
		{mc(api.StateRunning, sin), 0},
		{mc(api.StateRunning, &api.GuestAgent{}), 0}, // agente viejo: no anuncia nada
		{mc(api.StateRunning, nil), 0},
		{mc(api.StatePaused, con), 0},
	} {
		hits.Store(0)
		m.stopService(c.mc)
		if hits.Load() != c.want {
			t.Errorf("state %s agent %+v: %d stops, want %d", c.mc.State, c.mc.Agent, hits.Load(), c.want)
		}
	}
}
