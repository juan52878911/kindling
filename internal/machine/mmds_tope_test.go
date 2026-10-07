package machine

import (
	"context"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// Un almacén que no cabe en el del VMM se rechaza con el tope, antes de
// mandárselo: el error crudo de Firecracker no diría cuál es.
func TestPutMMDSQueNoCabeSeRechazaAntes(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	mc, falso := maquinaConVMMSinProceso(t, m, "dddd000000000001")
	grande := map[string]any{"env": map[string]any{"BLOB": strings.Repeat("x", api.MaxMMDSBytes)}}
	_, err := m.PutMMDS(context.Background(), mc.ID, grande)
	if err == nil || !strings.Contains(err.Error(), "over the limit") {
		t.Fatalf("PutMMDS de más de %d bytes = %v; quería el error del tope", api.MaxMMDSBytes, err)
	}
	if ll := falso.llamadasA("PUT", "/mmds"); len(ll) != 0 {
		t.Fatalf("se mandó al VMM igualmente: %d PUT /mmds", len(ll))
	}
	// Lo que cabe pasa.
	if _, err := m.PutMMDS(context.Background(), mc.ID, map[string]any{"env": map[string]any{"K": "v"}}); err != nil {
		t.Fatalf("PutMMDS pequeño: %v", err)
	}
}
