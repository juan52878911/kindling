package machine

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// G-08: reanudar una pausada reescribe StartedAt pero no VMMStartedAt, que
// es lo que identifica al proceso VMM (y con él la caché «agente visto» del
// daemon): es el mismo proceso.
func TestReanudarConservaVidaDelVMM(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	falso := nuevoFcFalso(t)
	mc := m.addForTest(id)
	lanzado := time.Now().Add(-time.Hour)
	pausada := time.Now().Add(-time.Minute)
	m.mu.Lock()
	mc.State, mc.PID, mc.FrozenAt = api.StatePaused, 4242, &pausada
	mc.StartedAt, mc.VMMStartedAt = &lanzado, &lanzado
	m.socket[id] = falso.Sock
	m.mu.Unlock()

	cur, _ := m.Get(id)
	out, err := m.reanudarLocked(context.Background(), cur, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.StartedAt == nil || !out.StartedAt.After(lanzado) {
		t.Fatalf("StartedAt no se reescribió al reanudar: %v", out.StartedAt)
	}
	if out.VMMStartedAt == nil || !out.VMMStartedAt.Equal(lanzado) || out.PID != 4242 {
		t.Fatalf("reanudar cambió la vida del VMM: pid %d, vmm_started_at %v (quería %v)", out.PID, out.VMMStartedAt, lanzado)
	}
}
