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
		r.Latency = "lo que tarde el servidor externo"
	case g.Snapshot == nil:
		r.Blocked = "no hay snapshot del que instanciar"
	case len(g.Snapshot.Tools) == 0:
		r.Blocked = "sin catálogo capturado: reimporta con kling mcp import"
	case r.Live > 0:
		r.Latency = "inmediata: ya hay una instancia atendiendo"
	case r.Warm > 0:
		r.Latency = "~30 ms: hay una congelada esperando"
	default:
		r.Cold = true
		r.Latency = "~250 ms: se instancia del snapshot dorado"
	}
	return r
}

// Flow describe, paso a paso, qué ocurre al llamar a una herramienta de este
// servicio. Es lo que hay que entender para saber por qué una llamada tarda
// 20 ms o 250, y qué se lleva por delante al terminar.
func (g Group) Flow() []string {
	if g.Link != nil {
		return []string{
			"El gateway reconoce que " + g.Service + " es un servidor externo",
			"No arranca ninguna máquina: reenvía la llamada a " + g.Link.URL,
			"El servidor responde con su propio estado, que persiste entre llamadas",
		}
	}
	if g.Snapshot == nil {
		return []string{"Sin snapshot: la llamada falla antes de arrancar nada"}
	}

	r := g.Readiness()
	steps := []string{"El gateway resuelve la herramienta al servicio " + g.Service}

	if g.Snapshot.Stateful() {
		switch {
		case r.Live > 0:
			steps = append(steps, "Reutiliza su instancia, que ya está en marcha")
		case r.Warm > 0:
			steps = append(steps, "Descongela su instancia (~30 ms), con su estado intacto")
		default:
			steps = append(steps, "Instancia una máquina del snapshot dorado (~250 ms)")
		}
		steps = append(steps,
			"El puente reenvía la llamada al servidor MCP por stdin/stdout",
			"La máquina SIGUE VIVA al terminar; se congela sola si deja de usarse")
		return steps
	}

	steps = append(steps,
		"Toma una microVM del fondo pre-calentado, o instancia una (~250 ms)",
		"El puente reenvía la llamada al servidor MCP por stdin/stdout",
		"Al responder, la máquina SE DESTRUYE — y con ella todo lo que escribiera")
	return steps
}

// Persistence explica dónde acaba lo que una herramienta escriba.
//
// memService es el servicio de memoria configurado, si lo hay.
func (g Group) Persistence(memService string) (string, string) {
	writers := g.writers()

	if g.Link != nil {
		return "El servidor externo gestiona su propio almacenamiento, fuera de kindling.", ""
	}
	if len(writers) == 0 {
		return "Ninguna de sus herramientas escribe: no hay nada que persistir.", ""
	}

	list := strings.Join(writers, ", ")
	if g.Snapshot != nil && g.Snapshot.Stateful() {
		return fmt.Sprintf(
				"Escribe con %s. Lo escrito vive en el disco de su instancia y sobrevive a que "+
					"se congele, pero NO a que la instancia se elimine.", list),
			advice(memService)
	}
	return fmt.Sprintf(
			"Escribe con %s. La máquina se destruye al terminar la llamada, así que "+
				"lo escrito DESAPARECE: un fichero guardado no estará ahí en la siguiente llamada.", list),
		advice(memService)
}

func advice(memService string) string {
	if memService == "" {
		return "Para lo que deba sobrevivir, enlaza un servicio de memoria: " +
			"kling mcp link engram <url> && kling memory enable"
	}
	return "Para lo que deba sobrevivir, guárdalo en " + memService +
		": es un servidor externo, vive fuera de las microVMs y no se lo lleva ninguna."
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
	var out []string
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
		out = append(out[:6], fmt.Sprintf("y %d más", len(out)-6))
	}
	return out
}
