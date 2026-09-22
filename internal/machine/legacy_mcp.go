package machine

import (
	"encoding/json"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// COMPATIBILIDAD CON MCP DENTRO DEL NÚCLEO — se retira en v0.6.
//
// Hasta v0.4 el snapshot llevaba dos campos de MCP: el catálogo (Tools, ToolsAt)
// y la salud (Health, HealthAt, HealthErr). Desde v0.5 lo canónico son dos
// anotaciones opacas, "mcp.tools" y "mcp.health", que el núcleo no interpreta.
// Mientras dure la transición:
//
//   - al LEER un meta.json de v0.4 sin anotaciones, los campos antiguos se
//     elevan a anotaciones en memoria (liftLegacyMCP), así kindling-mcp los
//     encuentra donde espera;
//   - al ESCRIBIR, los campos antiguos se rellenan a partir de las anotaciones
//     (mirrorLegacyMCP), así un CLI anterior los sigue leyendo y bajar de
//     versión el daemon no pierde nada.
//
// Las formas JSON de las anotaciones son las que fija kindling-mcp
// (internal/mcp); un test allí comprueba que coinciden con estas.

const (
	legacyToolsKey  = "mcp.tools"
	legacyHealthKey = "mcp.health"
)

type legacyToolsAnnotation struct {
	Tools      []api.ToolSpec `json:"tools"`
	CapturedAt *time.Time     `json:"captured_at,omitempty"`
}

type legacyHealthAnnotation struct {
	Status string     `json:"status"`
	At     *time.Time `json:"at,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// liftLegacyMCP eleva los campos de v0.4 a anotaciones, solo si la anotación
// no existe ya: una anotación escrita por v0.5 manda sobre el campo antiguo.
func liftLegacyMCP(s *api.Snapshot) {
	put := func(key string, v any) {
		if _, ok := s.Annotations[key]; ok {
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		if s.Annotations == nil {
			s.Annotations = map[string]json.RawMessage{}
		}
		s.Annotations[key] = b
	}
	if s.Tools != nil || s.ToolsAt != nil {
		put(legacyToolsKey, legacyToolsAnnotation{Tools: s.Tools, CapturedAt: s.ToolsAt})
	}
	if s.Health != "" {
		put(legacyHealthKey, legacyHealthAnnotation{Status: s.Health, At: s.HealthAt, Error: s.HealthErr})
	}
}

// mirrorLegacyMCP rellena los campos de v0.4 a partir de las anotaciones. Si
// una anotación se borró, su campo también.
func mirrorLegacyMCP(s *api.Snapshot) {
	var t legacyToolsAnnotation
	if ok, err := s.Annotation(legacyToolsKey, &t); ok && err == nil {
		s.Tools, s.ToolsAt = t.Tools, t.CapturedAt
	} else if !ok {
		s.Tools, s.ToolsAt = nil, nil
	}
	var h legacyHealthAnnotation
	if ok, err := s.Annotation(legacyHealthKey, &h); ok && err == nil {
		s.Health, s.HealthAt, s.HealthErr = h.Status, h.At, h.Error
	} else if !ok {
		s.Health, s.HealthAt, s.HealthErr = "", nil, ""
	}
}

// SetCatalog es la ruta antigua PUT /snapshots/{name}/catalog: escribe la
// anotación "mcp.tools".
func (m *Manager) SetCatalog(name string, tools []api.ToolSpec) (*api.Snapshot, error) {
	now := time.Now()
	b, err := json.Marshal(legacyToolsAnnotation{Tools: tools, CapturedAt: &now})
	if err != nil {
		return nil, err
	}
	return m.SetAnnotation(name, legacyToolsKey, b)
}

// SetHealth es la ruta antigua PUT /snapshots/{name}/health: escribe la
// anotación "mcp.health".
func (m *Manager) SetHealth(name string, healthy bool, probeErr string) (*api.Snapshot, error) {
	now := time.Now()
	h := legacyHealthAnnotation{Status: "healthy", At: &now}
	if !healthy {
		h.Status, h.Error = "unhealthy", probeErr
	}
	b, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	return m.SetAnnotation(name, legacyHealthKey, b)
}
