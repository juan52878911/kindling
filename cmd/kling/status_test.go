package main

import (
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/api"
)

// "services: ✓ 9" hablaba del inventario y se leía como salud: nueve servicios
// pasaron 26 horas caídos detrás de ese ✓. La línea de salud tiene que decir la
// verdad — incluida la incómoda: que nunca se ha sondeado.
func TestLaLineaDeSaludNoFingeSaludSinDatos(t *testing.T) {
	snap := func(name, health string) *api.Snapshot {
		return &api.Snapshot{Name: name, Health: health}
	}

	// Sin un solo sondeo, nada de ✓: hay que decir que no se sabe y cómo saberlo.
	got := mcpHealthLine([]*api.Snapshot{snap("a", ""), snap("b", "")})
	if strings.Contains(got, "✓") {
		t.Errorf("puso un ✓ sin ningún dato de salud: %q", got)
	}
	for _, q := range []string{"ever been probed", "kling mcp health"} {
		if !strings.Contains(got, q) {
			t.Errorf("sin sondeos debe decirlo y decir cómo sondear; falta %q en %q", q, got)
		}
	}

	// Con enfermos, se nombran: un contador sin nombres obliga a otra búsqueda.
	got = mcpHealthLine([]*api.Snapshot{snap("sano", "healthy"), snap("roto", "unhealthy"), snap("nunca", "")})
	for _, q := range []string{"✗", "1 unhealthy", "roto", "1 healthy", "1 never probed"} {
		if !strings.Contains(got, q) {
			t.Errorf("falta %q en %q", q, got)
		}
	}

	// Todo sano y sondeado: el ✓ ya es honesto.
	got = mcpHealthLine([]*api.Snapshot{snap("a", "healthy"), snap("b", "healthy")})
	if !strings.Contains(got, "✓ 2 healthy") {
		t.Errorf("con todo sano debía decirlo: %q", got)
	}

	// El nombre mostrado es el del SERVICIO (etiqueta), no el del snapshot,
	// igual que en mcp ls: es el nombre por el que el usuario lo conoce.
	s := snap("interno", "unhealthy")
	s.Labels = map[string]string{api.LabelService: "github"}
	got = mcpHealthLine([]*api.Snapshot{s})
	if !strings.Contains(got, "github") {
		t.Errorf("no usa el nombre de servicio: %q", got)
	}
}
