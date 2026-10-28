package machine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El healthcheck de una depends: se repite hasta el primer pase, y está
// enfermo cuando lo diría Docker (start_period + retries × interval).
func TestRepetirSalud(t *testing.T) {
	reloj := time.Unix(0, 0)
	ahora := func() time.Time { return reloj }
	h := &api.GraphHealth{Test: []string{"pg_isready"}, IntervalMS: 1000, Retries: 2, StartIntervalMS: 1}

	n := 0
	err := repetirSalud(context.Background(), h, ahora, func(context.Context, time.Duration) (int, error) {
		n++
		reloj = reloj.Add(100 * time.Millisecond)
		if n < 5 {
			return 1, nil
		}
		return 0, nil
	})
	if err != nil || n != 5 {
		t.Fatalf("pasa al quinto: n=%d err=%v", n, err)
	}

	reloj, n = time.Unix(0, 0), 0
	var plazoVisto time.Duration
	err = repetirSalud(context.Background(), h, ahora, func(_ context.Context, plazo time.Duration) (int, error) {
		n++
		plazoVisto = plazo
		reloj = reloj.Add(500 * time.Millisecond)
		return 0, errors.New("agente caído")
	})
	// 2 × 1 s: enfermo al cuarto intento de medio segundo.
	if err == nil || n != 4 || !strings.Contains(err.Error(), "unhealthy after 2s") || !strings.Contains(err.Error(), "agente caído") {
		t.Fatalf("enfermo: n=%d err=%v", n, err)
	}
	if plazoVisto != api.GraphHealthTimeout {
		t.Errorf("plazo de cada intento = %s", plazoVisto)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reloj = time.Unix(0, 0)
	if err := repetirSalud(ctx, h, ahora, func(context.Context, time.Duration) (int, error) { return 1, nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelado: %v", err)
	}
}

// Sin sonda o sin exec en el destino, la espera falla en vez de dar por
// sano lo que no se miró.
func TestEsperarSaludSinExec(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{"id1": {ID: "id1", Name: "db"}},
		grafos: map[string]*api.Graph{"g": {Nodes: map[string]api.GraphNode{"db": {MachineID: "id1"}}}}}
	h := &api.GraphHealth{Test: []string{"true"}}
	if err := m.esperarSalud(context.Background(), "g", "db", h); err == nil {
		t.Fatal("sin sonda: want an error")
	}
	m.FijarSondaExec(func(context.Context, *api.Machine, []string, time.Duration) (int, error) { return 0, nil })
	if err := m.esperarSalud(context.Background(), "g", "db", h); err == nil || !strings.Contains(err.Error(), "without exec") {
		t.Fatalf("sin exec: %v", err)
	}
	m.byID["id1"].AllowExec = true
	if err := m.esperarSalud(context.Background(), "g", "db", h); err != nil {
		t.Fatal(err)
	}
}
