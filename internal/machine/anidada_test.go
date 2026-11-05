package machine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// -nested se valida antes de reservar nada: con from, o con un backend que no
// es kling-hv, es un error de la petición (400) que dice qué falta.
func TestAnidadaSeValidaAntesDeArrancar(t *testing.T) {
	m := newTestManager(t)
	if err := m.validarAnidada(api.RunRequest{}); err != nil {
		t.Fatalf("sin nested: %v", err)
	}
	err := m.validarAnidada(api.RunRequest{Nested: true, From: "dorado"})
	if !errors.Is(err, ErrRunRequest) || !strings.Contains(err.Error(), "from") {
		t.Errorf("nested con from = %v", err)
	}
	if m.Backend() != BackendHV {
		err := m.validarAnidada(api.RunRequest{Nested: true})
		if !errors.Is(err, ErrRunRequest) || !strings.Contains(err.Error(), "kling-hv") {
			t.Errorf("nested con %s = %v, quería que pidiera kling-hv", m.Backend(), err)
		}
	}
	if m.kernelDe(false) != m.KernelPath() || m.kernelDe(true) == m.KernelPath() {
		t.Errorf("kernels: normal %s, anidada %s", m.kernelDe(false), m.kernelDe(true))
	}
}

// Una máquina anidada no se vuelca a disco: Freeze y Commit se niegan sin
// hablar con el VMM, con un error que dice por qué.
func TestAnidadaNoSeCongelaNiSeHaceSnapshot(t *testing.T) {
	m := newTestManager(t)
	id := "a1dada0000000001"
	falso, _ := plantillaParaCommit(t, m, id)
	m.mu.Lock()
	m.byID[id].Nested = true
	m.mu.Unlock()

	if _, err := m.Freeze(context.Background(), id); !errors.Is(err, ErrWrongState) ||
		!strings.Contains(err.Error(), "nested") {
		t.Errorf("Freeze = %v", err)
	}
	if _, err := m.Commit(context.Background(), id, "dorado", false); !errors.Is(err, ErrCommitConflict) ||
		!strings.Contains(err.Error(), "nested") {
		t.Errorf("Commit = %v", err)
	}
	if ll := falso.todas(); len(ll) != 0 {
		t.Errorf("habló con el VMM de una máquina anidada: %+v", ll)
	}
	if _, err := os.Stat(m.snapDir("dorado")); !os.IsNotExist(err) {
		t.Errorf("quedó un directorio de snapshot: %v", err)
	}
}

// Como con los secretos: el TTL que congela no se puede cumplir, así que se
// retira una vez y la máquina sigue sana.
func TestTTLDeUnaMaquinaAnidadaSeRetira(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	id := "a1dada0000000002"
	mc := m.addForTest(id)
	m.mu.Lock()
	mc.Nested = true
	mc.TTLSeconds = 60
	m.mu.Unlock()

	m.handleFreezeFailure(id, errors.New("nested"))

	cur, _ := m.Get(id)
	if cur.TTLSeconds != 0 || cur.State != api.StateRunning {
		t.Fatalf("ttl = %d, estado = %s; quería 0 y running", cur.TTLSeconds, cur.State)
	}
	if n := m.freezeFails[id]; n != 0 {
		t.Errorf("fallos apuntados = %d, want 0", n)
	}
}
