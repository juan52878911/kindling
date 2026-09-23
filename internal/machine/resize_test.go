package machine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El globo en reposo retiene la diferencia entre el techo y la memoria. Es lo
// que el apretón (squeeze) tiene que restaurar al terminar: volver a 0 le daría
// al invitado su techo entero.
func TestGloboBase(t *testing.T) {
	for _, c := range []struct {
		mem, max, want int
	}{{512, 0, 0}, {512, 512, 0}, {512, 2048, 1536}, {2048, 512, 0}} {
		if got := globoBase(&api.Machine{MemMiB: c.mem, MemMaxMiB: c.max}); got != c.want {
			t.Errorf("mem %d, techo %d: %d, quería %d", c.mem, c.max, got, c.want)
		}
	}
	if globoBase(nil) != 0 {
		t.Error("nil")
	}
}

// Los rechazos de resize, que se deciden antes de tocar Firecracker.
func TestResizeRechazos(t *testing.T) {
	m := newTestManager(t)
	ctx := context.Background()

	if _, err := m.Resize(ctx, "nada", 512); !errors.Is(err, ErrNoMachine) {
		t.Errorf("máquina inexistente: %v", err)
	}

	fija := m.addForTest("f1x0000000000001")
	m.mu.Lock()
	fija.MemMiB = 512
	m.mu.Unlock()
	if _, err := m.Resize(ctx, fija.ID, 256); err == nil || !strings.Contains(err.Error(), "mem_max_mib") {
		t.Errorf("sin techo tenía que decir que es fija: %v", err)
	}

	elastica := m.addForTest("e1a5000000000001")
	m.mu.Lock()
	elastica.MemMiB, elastica.MemMaxMiB = 512, 2048
	m.mu.Unlock()
	for _, mem := range []int{64, 4096} {
		if _, err := m.Resize(ctx, elastica.ID, mem); err == nil || !strings.Contains(err.Error(), "between") {
			t.Errorf("%d MiB fuera de rango: %v", mem, err)
		}
	}

	m.mu.Lock()
	elastica.State = api.StateWarm
	m.mu.Unlock()
	if _, err := m.Resize(ctx, elastica.ID, 1024); !errors.Is(err, ErrNotRunning) {
		t.Errorf("congelada: %v", err)
	}
}
