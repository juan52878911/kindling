package machine

import (
	"context"
	"errors"
	"testing"

	knet "github.com/juan52878911/kindling/internal/net"
	"github.com/juan52878911/kindling/pkg/api"
)

// usarBarrerasFalsas cambia knet.AsegurarHost por f hasta el final del test.
func usarBarrerasFalsas(t *testing.T, f func() error) {
	old := asegurarBarrerasHost
	t.Cleanup(func() { asegurarBarrerasHost = old; errBarrerasHost.Store(nil) })
	asegurarBarrerasHost = f
}

// S2: el vigilante revisa las barreras del host en cada vuelta si hay alguna
// red en pie (montada por este daemon o de una máquina readoptada), y no
// lanza nada sin ninguna. Antes solo las revisaba Setup:
// tras un reload del cortafuegos, las máquinas vivas seguían sin ellas
// hasta que otra montara su red.
func TestElVigilanteReponeLasBarrerasDelHost(t *testing.T) {
	m := newTestManager(t)
	llamadas := 0
	usarBarrerasFalsas(t, func() error { llamadas++; return nil })
	m.pruebasPasoVigilante = func(nombre string) bool { return nombre == "barrerasHost" }

	m.vueltaVigilante(context.Background())
	if llamadas != 0 {
		t.Fatalf("sin redes montadas: %d comprobaciones, quería 0", llamadas)
	}
	// Una readoptada tras reiniciar el daemon: running, sin redMontada.
	m.byID["bb22cc33dd44ee55"] = &api.Machine{ID: "bb22cc33dd44ee55", State: api.StateRunning}
	m.vueltaVigilante(context.Background())
	if llamadas != 1 {
		t.Fatalf("con una máquina readoptada: %d comprobaciones, quería 1", llamadas)
	}
	delete(m.byID, "bb22cc33dd44ee55")
	m.redMontada.Store("aa11bb22cc33dd44", struct{}{})
	m.vueltaVigilante(context.Background())
	if llamadas != 2 {
		t.Fatalf("con una red montada: %d comprobaciones, quería 2", llamadas)
	}
}

// S2: el thaw que conserva la red del freeze no pasa por Setup; ahora
// comprueba las barreras del host igual, y sin ellas la máquina no
// despierta.
func TestThawConLaRedConservadaCompruebaLasBarreras(t *testing.T) {
	m := newTestManager(t)
	oldR, oldM := redConservada, montarRedHost
	t.Cleanup(func() { redConservada, montarRedHost = oldR, oldM })
	redConservada = func(*Manager, *knet.Net, string) bool { return true }
	montarRedHost = func(*knet.Net, knet.Egress, []string, int) error {
		t.Fatal("con la red conservada no se monta otra")
		return nil
	}
	mc := &api.Machine{ID: "aa11bb22cc33dd44", NetIndex: 3, Egress: "none"}

	llamadas := 0
	usarBarrerasFalsas(t, func() error { llamadas++; return nil })
	if _, err := m.redDelThaw(mc); err != nil || llamadas != 1 {
		t.Fatalf("redDelThaw = %v, %d comprobaciones; quería nil y 1", err, llamadas)
	}
	sinBarreras := errors.New("sin barreras")
	usarBarrerasFalsas(t, func() error { return sinBarreras })
	if _, err := m.redDelThaw(mc); !errors.Is(err, sinBarreras) {
		t.Fatalf("redDelThaw sin barreras = %v, quería %v", err, sinBarreras)
	}
}
