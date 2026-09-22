package machine

import (
	"encoding/json"

	"github.com/juan52878911/kindling/pkg/api"
)

// SNAPSHOTS DE v0.4.
//
// Hasta v0.4 el meta.json de un snapshot llevaba el catálogo y la salud de MCP
// en campos propios (tools, tools_at, health, health_at, health_err). Desde v0.5
// son anotaciones opacas que escribe kindling-mcp. Un snapshot de v0.4 que nunca
// se volvió a anotar sigue teniendo los campos antiguos en disco, así que al
// leerlo se elevan, sin interpretarlos, a las anotaciones "mcp.tools" y
// "mcp.health" con la forma que usa kindling-mcp. La siguiente escritura del
// meta los deja ya solo como anotaciones.
//
// Se puede quitar cuando ningún host conserve snapshots de v0.4.

func liftV04(raw []byte, s *api.Snapshot) {
	var old map[string]json.RawMessage
	if json.Unmarshal(raw, &old) != nil {
		return
	}
	put := func(key string, v map[string]json.RawMessage) {
		if _, ok := s.Annotations[key]; ok || len(v) == 0 {
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
	tools := map[string]json.RawMessage{}
	if v, ok := old["tools"]; ok {
		tools["tools"] = v
	}
	if v, ok := old["tools_at"]; ok {
		tools["captured_at"] = v
	}
	put("mcp.tools", tools)

	health := map[string]json.RawMessage{}
	if v, ok := old["health"]; ok && string(v) != `""` {
		health["status"] = v
		if at, ok := old["health_at"]; ok {
			health["at"] = at
		}
		if e, ok := old["health_err"]; ok {
			health["error"] = e
		}
	}
	put("mcp.health", health)
}
