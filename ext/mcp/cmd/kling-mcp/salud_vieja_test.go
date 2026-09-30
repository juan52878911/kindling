package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
)

// Un "sano" de hace días no es salud de ahora: la línea no pinta ✓ con un
// veredicto más viejo que el doble del intervalo del vigía.
func TestLaLineaDeSaludNoCreeVeredictosViejos(t *testing.T) {
	now := time.Now()
	snap := func(name string, at *time.Time) *api.Snapshot {
		b, _ := json.Marshal(mcp.Health{Status: mcp.Healthy, At: at})
		return &api.Snapshot{Name: name, Annotations: map[string]json.RawMessage{mcp.HealthKey: b}}
	}
	viejo := now.Add(-3 * 24 * time.Hour)
	got := mcpHealthLine([]*api.Snapshot{snap("a", &viejo), snap("b", &viejo)})
	if strings.Contains(got, "✓") {
		t.Errorf("a 3-day-old verdict shows as healthy: %q", got)
	}
	reciente := now.Add(-time.Hour)
	got = mcpHealthLine([]*api.Snapshot{snap("a", &reciente), snap("b", &viejo)})
	if !strings.Contains(got, "✓ 1 healthy") || !strings.Contains(got, "1 never probed or stale") {
		t.Errorf("recent + old = %q", got)
	}
}
