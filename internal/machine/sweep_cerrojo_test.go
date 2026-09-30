package machine

import (
	"os/exec"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// pidMuerto devuelve el PID de un proceso que ya terminó y se recogió.
func pidMuerto(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Skipf("no pude lanzar el proceso de prueba: %v", err)
	}
	return cmd.Process.Pid
}

// Freeze mata el VMM y solo después, con el volcado sellado, apunta la máquina
// warm con PID 0. En ese hueco el vigilante veía "running con un PID que ya no
// existe" y la daba por muerta: failed, red desmontada, EvFailed falso. Con el
// cerrojo de la máquina tomado por otro, no se toca.
func TestSweepNoTocaUnaMaquinaConSuCerrojoTomado(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := newID()
	m.byID[id] = &api.Machine{ID: id, Name: "congelandose", State: api.StateRunning, PID: pidMuerto(t)}

	soltar := m.lock(id) // Freeze, entre killPaused y apuntarla warm
	m.sweep()
	if mc, _ := m.Get(id); mc.State != api.StateRunning {
		soltar()
		t.Fatalf("el vigilante marcó %s una máquina que otra operación tenía entre manos", mc.State)
	}
	soltar()

	// Suelta, y con el proceso de verdad desaparecido, sí es asunto suyo.
	m.sweep()
	if mc, _ := m.Get(id); mc.State != api.StateFailed {
		t.Fatalf("estado = %s; sin nadie con el cerrojo, un VMM desaparecido es failed", mc.State)
	}
}
