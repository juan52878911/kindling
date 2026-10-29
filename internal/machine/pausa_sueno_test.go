package machine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// M8: una pausada desde antes de que el Mac durmiera se resincroniza al
// reanudarla (su reloj no avanzó dormido); una pausada después, no: reanudar
// sigue siendo solo Resume.
func TestReanudarTrasDormirResincroniza(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	var resyncs atomic.Int32
	agente := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == api.GuestResyncPath {
			resyncs.Add(1)
			_, _ = w.Write([]byte(`{"skew_ms":0}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer agente.Close()

	// Relojes falsos: pared y monotónico avanzan juntos salvo al dormir.
	pared, mono := time.Unix(2_000_000, 0), time.Duration(0)
	m.reloj.leer = func() (time.Time, time.Duration) { return pared, mono }
	m.reloj.durmio()

	id := newID()
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	pausada := pared.Add(-time.Minute)
	m.mu.Lock()
	mc.State, mc.FrozenAt = api.StatePaused, &pausada
	mc.Forwards = map[string]string{"8080": strings.TrimPrefix(agente.URL, "http://")}
	m.socket[id] = falso.Sock
	m.mu.Unlock()

	// Sin dormir: solo Resume.
	cur, _ := m.Get(id)
	if _, err := m.reanudarLocked(context.Background(), cur, nil); err != nil {
		t.Fatal(err)
	}
	if n := resyncs.Load(); n != 0 {
		t.Fatalf("resync sin que el host durmiera: %d", n)
	}

	// Se vuelve a pausar, el Mac duerme una hora y se reanuda.
	m.mu.Lock()
	m.byID[id].State, m.byID[id].FrozenAt = api.StatePaused, &pausada
	m.mu.Unlock()
	pared, mono = pared.Add(time.Hour+time.Second), mono+time.Second
	cur, _ = m.Get(id)
	if _, err := m.reanudarLocked(context.Background(), cur, nil); err != nil {
		t.Fatal(err)
	}
	if n := resyncs.Load(); n != 1 {
		t.Fatalf("resync tras dormir = %d, quería 1", n)
	}

	// Pausada después del sueño: otra vez sin resync.
	despues := pared.Add(time.Second)
	m.mu.Lock()
	m.byID[id].State, m.byID[id].FrozenAt = api.StatePaused, &despues
	m.mu.Unlock()
	pared, mono = pared.Add(2*time.Second), mono+2*time.Second
	cur, _ = m.Get(id)
	if _, err := m.reanudarLocked(context.Background(), cur, nil); err != nil {
		t.Fatal(err)
	}
	if n := resyncs.Load(); n != 1 {
		t.Fatalf("resync de una pausada tras el sueño = %d, quería 1", n)
	}
}
