package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/internal/api"
)

// Este fichero responde a tres preguntas que la tabla de instancias no contesta:
//
//	¿qué servicios pueden ejecutarse aunque ahora no haya nada corriendo?
//	¿qué pasa exactamente cuando llamo a una herramienta?
//	¿dónde acaba lo que una herramienta escriba?
//
// La tercera es la que más confusión causa, porque la respuesta honesta es
// incómoda: en una microVM efímera, todo lo que se escriba muere con ella.

// Readiness resume si un servicio puede atender una llamada y a qué coste.
type Readiness struct {
	Live    int    // instancias corriendo
	Warm    int    // congeladas: vuelven en milisegundos
	Cold    bool   // no hay ninguna: hay que instanciar del snapshot
	Latency string // lo que costará la próxima llamada
	Blocked string // por qué NO puede ejecutarse, si es el caso
}

func (g Group) Readiness() Readiness {
	var r Readiness
	for _, m := range g.Machines {
		switch m.State {
		case api.StateRunning:
			r.Live++
		case api.StateWarm:
			r.Warm++
		}
	}

	switch {
	case g.Link != nil:
		r.Latency = "whatever the external server takes"
	case g.Snapshot == nil:
		r.Blocked = "no snapshot to instantiate from"
	case len(g.Snapshot.Tools) == 0:
		r.Blocked = "no catalog captured: re-import with kling mcp import"
	case r.Live > 0:
		r.Latency = "immediate: an instance is already running"
	case r.Warm > 0:
		r.Latency = "~30 ms: a frozen instance is waiting"
	default:
		r.Cold = true
		r.Latency = "~250 ms: instantiates from the golden snapshot"
	}
	return r
}

// Flow describe, paso a paso, qué ocurre al llamar a una herramienta de este
// servicio. Es lo que hay que entender para saber por qué una llamada tarda
// 20 ms o 250, y qué se lleva por delante al terminar.
func (g Group) Flow() []string {
	if g.Link != nil {
		return []string{
			"The gateway recognizes that " + g.Service + " is an external server",
			"It doesn't start any machine: it forwards the call to " + g.Link.URL,
			"The server responds with its own state, which persists between calls",
		}
	}
	if g.Snapshot == nil {
		return []string{"No snapshot: the call fails before anything starts"}
	}

	r := g.Readiness()
	steps := []string{"The gateway resolves the tool to the " + g.Service + " service"}

	if g.Snapshot.Stateful() {
		switch {
		case r.Live > 0:
			steps = append(steps, "Reuses its instance, which is already running")
		case r.Warm > 0:
			steps = append(steps, "Thaws its instance (~30 ms), with its state intact")
		default:
			steps = append(steps, "Instantiates a machine from the golden snapshot (~250 ms)")
		}
		steps = append(steps,
			"The bridge forwards the call to the MCP server over stdin/stdout",
			"The machine STAYS ALIVE when it's done; it freezes itself if it goes unused")
		return steps
	}

	steps = append(steps,
		"Takes a microVM from the pre-warmed pool, or instantiates one (~250 ms)",
		"The bridge forwards the call to the MCP server over stdin/stdout",
		"Once it responds, the machine IS DESTROYED — taking with it everything it wrote")
	return steps
}

// writers son las herramientas del servicio que parecen escribir algo.
func (g Group) writers() []string {
	var tools []api.ToolSpec
	switch {
	case g.Link != nil:
		tools = g.Link.Tools
	case g.Snapshot != nil:
		tools = g.Snapshot.Tools
	}

	verbs := []string{"write", "save", "store", "create", "add", "delete", "remove",
		"update", "edit", "move", "rename", "append", "insert", "guardar", "crear", "borrar"}
	out := []string{}
	for _, t := range tools {
		n := strings.ToLower(t.Name)
		for _, v := range verbs {
			if strings.Contains(n, v) {
				out = append(out, t.Name)
				break
			}
		}
	}
	sort.Strings(out)
	if len(out) > 6 {
		out = append(out[:6], fmt.Sprintf("and %d more", len(out)-6))
	}
	return out
}
