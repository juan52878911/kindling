package machine

import (
	"context"
	"net/http"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// M4: un Pause que llega al VMM pero cuya respuesta es un error dejaba la
// máquina en pausa figurando como running. Ahora se reanuda, como en Freeze.
func TestPauseFallidaReanudaLaMaquina(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "a4a4000000000001"
	falso, _ := maquinaCorriendo(t, m, id)
	falso.ponerEstado("Running")
	falso.fallar(http.MethodPatch, "/vm", http.StatusInternalServerError, "")
	primera := true
	falso.enGancho(func(metodo, ruta string) {
		if metodo == http.MethodPatch && ruta == "/vm" && primera {
			// La pausa se aplica y la respuesta falla; lo siguiente ya no.
			primera = false
			falso.ponerEstado("Paused")
			falso.dejarDeFallar(http.MethodPatch, "/vm")
		}
	})

	if _, err := m.Pause(context.Background(), id); err == nil {
		t.Fatal("Pause con el VMM fallando no devolvió error")
	}
	if st := falso.verEstado(); st != "Running" {
		t.Fatalf("el VMM quedó %s: Pause no lo reanudó tras fallar", st)
	}
	if viva := vivaDe(t, m, id); viva.State != api.StateRunning {
		t.Fatalf("estado %s; quería running", viva.State)
	}
}

// Si tampoco se puede reanudar, queda pendiente para el vigilante y el
// planificador no le enruta nada (transición freezing).
func TestPauseFallidaSinPoderReanudarQuedaPendiente(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "a4a4000000000002"
	falso, _ := maquinaCorriendo(t, m, id)
	falso.fallar(http.MethodPatch, "/vm", http.StatusInternalServerError, "")
	// Un plazo y no una respuesta del VMM: no se sabe si la pausa llegó.
	ctx, cancel := context.WithCancel(context.Background())
	falso.enGancho(func(metodo, ruta string) {
		if metodo == http.MethodPatch && ruta == "/vm" {
			cancel()
		}
	})

	if _, err := m.Pause(ctx, id); err == nil {
		t.Fatal("Pause cortada no devolvió error")
	}
	m.mu.RLock()
	_, pendiente := m.reanudaciones[id]
	tr := m.transicion[id]
	m.mu.RUnlock()
	if !pendiente || tr != api.TransitionFreezing {
		t.Fatalf("pendiente %v, transición %q; quería pendiente y freezing", pendiente, tr)
	}
}

// M2: un Pause cortado por plazo sin llegar al VMM, y un Resume que el VMM
// rechaza porque no estaba en pausa: la máquina sigue despierta y no queda
// pendiente para el vigilante.
func TestPauseCortadaConResumeRechazadoSigueRunning(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "a4a4000000000003"
	falso, _ := maquinaCorriendo(t, m, id)
	falso.ponerEstado("Running")
	falso.fallar(http.MethodPatch, "/vm", http.StatusBadRequest, "The requested operation is not supported in the current state")
	ctx, cancel := context.WithCancel(context.Background())
	primera := true
	falso.enGancho(func(metodo, ruta string) {
		if metodo == http.MethodPatch && ruta == "/vm" && primera {
			primera = false
			cancel()
		}
	})

	if _, err := m.Pause(ctx, id); err == nil {
		t.Fatal("Pause cortada no devolvió error")
	}
	m.mu.RLock()
	_, pendiente := m.reanudaciones[id]
	tr := m.transicion[id]
	m.mu.RUnlock()
	if pendiente || tr != "" {
		t.Fatalf("pendiente %v, transición %q; quería ni pendiente ni transición", pendiente, tr)
	}
	if viva := vivaDe(t, m, id); viva.State != api.StateRunning {
		t.Fatalf("estado %s; quería running", viva.State)
	}
}
