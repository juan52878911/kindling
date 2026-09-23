// Package mcp reúne el vocabulario de MCP que hasta v0.4 vivía repartido por el
// núcleo: el catálogo de herramientas, la salud de un servicio, los servidores
// externos enlazados, la marca de "con estado" y el cable JSON-RPC sobre
// Streamable HTTP.
//
// Guarda su estado en el daemon con las piezas genéricas del núcleo —anotaciones
// de snapshot y el store— y no con campos propios. Es la mitad de kindling que
// se muda a kindling-mcp; mientras dure la transición (v0.5) habla con daemons
// anteriores cayendo a las rutas antiguas cuando las nuevas no existen.
package mcp

import (
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// ToolSpec y Link son alias durante la transición: el API del núcleo todavía
// los expone por compatibilidad. En v0.6 pasan a ser tipos de este paquete.
type (
	ToolSpec = api.ToolSpec
	Link     = api.Link
)

// Claves de las anotaciones que este paquete cuelga de cada snapshot. Sus formas
// JSON las comparte internal/machine/legacy_mcp.go para espejar los campos de
// v0.4; TestFormasCompatiblesConElNucleo lo vigila.
const (
	ToolsKey  = "mcp.tools"
	HealthKey = "mcp.health"
)

// Tools es la anotación mcp.tools: el catálogo que declaró el servidor al
// importarlo, capturado una vez para no despertar máquinas al listarlo.
type Tools struct {
	Tools      []ToolSpec `json:"tools"`
	CapturedAt *time.Time `json:"captured_at,omitempty"`
}

// Estados de salud.
const (
	Healthy   = "healthy"
	Unhealthy = "unhealthy"
)

// Health es la anotación mcp.health: el veredicto del último sondeo. Status
// vacío significa "nunca se ha sondeado".
type Health struct {
	Status string     `json:"status"`
	At     *time.Time `json:"at,omitempty"`
	Error  string     `json:"error,omitempty"`
}

// LabelStateful marca los servicios que ACUMULAN estado entre llamadas.
//
// El modo efímero destruye la máquina tras cada acción, y con ella todo lo que
// el servidor guardara en memoria o en su disco. Para un servidor de ficheros
// da igual —el estado está fuera—, pero un grafo de conocimiento o un
// razonamiento por pasos perderían su contenido en cada invocación.
//
// Los servicios marcados así usan una instancia persistente, que se congela al
// quedar ociosa y vuelve en milisegundos conservando lo que tenía.
const LabelStateful = "stateful"

// Stateful indica si el servicio del snapshot debe conservar estado entre
// llamadas.
func Stateful(s *api.Snapshot) bool {
	return s != nil && s.Labels != nil && s.Labels[LabelStateful] == "true"
}
