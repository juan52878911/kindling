package machine

import (
	"os"
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

// Sin ninguna máquina running ni pausada, sweep no recorre los procesos del
// host en cada vuelta: en la primera y luego una de cada vueltasEscaneoSinVMs.
// Con una que corre, en todas.
func TestSweepSinVMsEscaneaPocasVeces(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	escaneos := func(n int) int {
		c := 0
		for range n {
			if m.sweep() != nil {
				c++
			}
		}
		return c
	}
	if got := escaneos(2 * vueltasEscaneoSinVMs); got != 2 {
		t.Fatalf("sin VMs, %d escaneos en %d vueltas; quería 2", got, 2*vueltasEscaneoSinVMs)
	}
	id := newID()
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: "viva", State: api.StatePaused, PID: os.Getpid()}
	m.mu.Unlock()
	soltar := m.lock(id) // que no la dé por muerta: no es un VMM de verdad
	if got := escaneos(3); got != 3 {
		soltar()
		t.Fatalf("con una pausada, %d escaneos en 3 vueltas; quería todas", got)
	}
	soltar()
	m.mu.Lock()
	delete(m.byID, id)
	m.mu.Unlock()
	if m.sweep() == nil {
		t.Fatal("la primera vuelta sin VMs debía escanear")
	}
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
