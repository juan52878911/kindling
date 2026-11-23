package machine

import (
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// Una copia (run -from) que el reinicio pilló "created" y cuyo VMM ya no está
// (kill -9 entre persistirYa y spawn, o reinicio del host) no puede quedar
// zombi: failed con errCopiaCortada, red desmontada y nombre recogible.
func TestReproR6_CreatedSinVMMQuedaZombi(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "c0b1a000000000aa"
	mc := m.addForTest(id)
	hace := time.Now().Add(-48 * time.Hour)
	m.mu.Lock()
	mc.Name, mc.From, mc.State, mc.PID = "copia", "dorado", api.StateCreated, 0
	mc.TTLSeconds, mc.CreatedAt, mc.TTLAt, mc.OnTTL = 60, hace, &hace, api.OnTTLRemove
	m.mu.Unlock()

	desmontadas := 0
	previo := desmontarRedHost
	desmontarRedHost = func(*knet.Net) { desmontadas++ }
	defer func() { desmontarRedHost = previo }()

	m.reconcile()

	viva := vivaDe(t, m, id)
	if viva.State != api.StateFailed || viva.LastErr != errCopiaCortada || viva.FailedAt == nil {
		t.Errorf("state=%s lastErr=%q failedAt=%v: debía quedar failed con errCopiaCortada", viva.State, viva.LastErr, viva.FailedAt)
	}
	if desmontadas == 0 {
		t.Errorf("reconcile no desmontó su red")
	}
}

// Una created sin VMM que no es copia sin entregar (arranque en frío) va a stopped.
func TestReconcileCreatedSinVMMEnFrioVaAStopped(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "c0b1a000000000bb"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.Name, mc.State, mc.PID = "fria", api.StateCreated, 0
	m.mu.Unlock()
	desmontadas := 0
	previo := desmontarRedHost
	desmontarRedHost = func(*knet.Net) { desmontadas++ }
	defer func() { desmontarRedHost = previo }()
	m.reconcile()
	if s := vivaDe(t, m, id).State; s != api.StateStopped {
		t.Errorf("state=%s, quería stopped", s)
	}
	if desmontadas != 1 {
		t.Errorf("desmontadas=%d, quería 1", desmontadas)
	}
}

// limpiarMaquina no borraba m.secretos[id].
func TestReproR6_SecretosNoSeOlvidanAlBorrar(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "5ec0000000000001"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.State = api.StateStopped
	m.secretos = map[string]*estadoSecreto{id: {}}
	foto := mc.Clone()
	m.mu.Unlock()
	if err := m.limpiarMaquina(foto); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	_, queda := m.secretos[id]
	_, existe := m.byID[id]
	m.mu.RUnlock()
	if existe {
		t.Fatal("la máquina sigue en byID")
	}
	if queda {
		t.Errorf("m.secretos conserva la entrada de %s tras borrarla", shortID(id))
	}
}
