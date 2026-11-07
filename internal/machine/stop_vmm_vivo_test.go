package machine

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Stop con un VMM que sobrevive al SIGKILL (p. ej. en estado D) no puede dar
// la máquina por parada: quedaba stopped con PID 0 y kling start lanzaba un
// segundo VMM sobre el mismo overlay. Como Remove, devuelve error y conserva
// el PID y la red.
func TestStopConElVMMVivoNoLoDaPorMuerto(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "aa0000000000005a"
	maquinaCorriendo(t, m, id)
	m.mu.Lock()
	m.byID[id].NetIndex = 7
	m.mu.Unlock()

	prevSenal, prevPlazo := senalMatar, plazoMuerte
	senalMatar, plazoMuerte = func(int) {}, 20*time.Millisecond
	t.Cleanup(func() { senalMatar, plazoMuerte = prevSenal, prevPlazo })

	if _, err := m.Stop(id); err == nil {
		t.Fatal("Stop no devolvió error con el VMM vivo")
	}
	viva := vivaDe(t, m, id)
	if viva.State == api.StateStopped || viva.PID == 0 || viva.NetIndex != 7 {
		t.Fatalf("quería la máquina sin parar, con su PID y su red; hay state=%s pid=%d net=%d",
			viva.State, viva.PID, viva.NetIndex)
	}
	senalMatar = prevSenal
}
