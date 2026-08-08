// Package report genera el informe HTML de la topología.
//
// Se construye en el CLI, no en el daemon: el fichero acaba en la máquina desde
// la que trabajas, aunque el daemon esté al otro lado de un SSH.
//
// Es autocontenido a propósito —sin CDN, sin fuentes remotas, sin peticiones— para
// que se pueda abrir sin red y sin filtrar a nadie la topología del homelab.
package report

import (
	"fmt"
	"html"
	"sort"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// Group es un conjunto de máquinas que comparten servicio MCP.
type Group struct {
	Service   string
	Snapshot  *api.Snapshot
	Link      *api.Link // servicio externo: no corre aquí
	Machines  []*api.Machine
	RAMShared int64
}

// Kind describe de dónde sale un servicio.
func (g Group) Kind() string {
	switch {
	case g.Link != nil:
		return "externo"
	case g.Snapshot != nil:
		return "microVM"
	default:
		return "sin snapshot"
	}
}

// Ephemeral indica si sus instancias mueren tras cada acción.
func (g Group) Ephemeral() bool {
	return g.Link == nil && g.Snapshot != nil && !g.Snapshot.Stateful()
}

// Trigger explica qué hace aparecer a una instancia de este servicio.
func (g Group) Trigger() string {
	switch {
	case g.Link != nil:
		return "no arranca nada: se enruta a " + g.Link.URL
	case g.Snapshot == nil:
		return "sin snapshot: no puede instanciarse"
	case g.Snapshot.Stateful():
		return "primera llamada a una de sus herramientas; se congela al quedar ociosa"
	default:
		return "cada llamada a una de sus herramientas; muere al terminar"
	}
}

// Shares describe qué comparten sus instancias entre sí.
func (g Group) Shares() string {
	switch {
	case g.Link != nil:
		return "el proceso remoto y su estado"
	case g.Snapshot == nil:
		return "—"
	default:
		return human(g.Snapshot.MemBytes) + " de memoria del snapshot, y la imagen base"
	}
}

// Runnable dice si el servicio puede atender una llamada ahora mismo.
func (g Group) Runnable() (bool, string) {
	switch {
	case g.Link != nil:
		return true, "enrutado a un servidor externo"
	case g.Snapshot == nil:
		return false, "no hay snapshot del que instanciar"
	case len(g.Snapshot.Tools) == 0:
		return false, "sin catálogo: reimporta con kling mcp import"
	}
	for _, m := range g.Machines {
		if m.State == api.StateRunning {
			return true, "ya hay una instancia en marcha"
		}
		if m.State == api.StateWarm {
			return true, "instancia congelada: vuelve en milisegundos"
		}
	}
	return true, "se instanciará del snapshot dorado"
}

// Build agrupa por la etiqueta "service" y, en su defecto, por el snapshot del
// que salieron: dos máquinas del mismo snapshot comparten memoria, así que
// pertenecen juntas aunque nadie las haya etiquetado.
func Build(machines []*api.Machine, snaps []*api.Snapshot) []Group {
	return BuildWith(machines, snaps, nil)
}

// BuildWith incluye además los servidores externos enlazados.
func BuildWith(machines []*api.Machine, snaps []*api.Snapshot, links []*api.Link) []Group {
	byName := map[string]*api.Snapshot{}
	for _, s := range snaps {
		byName[s.Name] = s
	}

	key := func(m *api.Machine) string {
		if svc := m.Service(); svc != "" {
			return svc
		}
		if m.From != "" {
			return m.From
		}
		return "(sin agrupar)"
	}

	idx := map[string]*Group{}
	var order []string
	for _, m := range machines {
		k := key(m)
		g, ok := idx[k]
		if !ok {
			g = &Group{Service: k}
			if m.From != "" {
				g.Snapshot = byName[m.From]
			}
			idx[k] = g
			order = append(order, k)
		}
		if g.Snapshot == nil && m.From != "" {
			g.Snapshot = byName[m.From]
		}
		g.Machines = append(g.Machines, m)
	}

	// Snapshots sin instancias: también son parte del inventario.
	for _, s := range snaps {
		k := s.Name
		if svc := s.Service(); svc != "" {
			k = svc
		}
		if _, ok := idx[k]; !ok {
			idx[k] = &Group{Service: k, Snapshot: s}
			order = append(order, k)
		}
	}

	// Servidores externos: son servicios sin máquinas.
	for _, l := range links {
		k := l.Service()
		if g, ok := idx[k]; ok {
			g.Link = l
			continue
		}
		idx[k] = &Group{Service: k, Link: l}
		order = append(order, k)
	}

	out := make([]Group, 0, len(order))
	for _, k := range order {
		g := idx[k]
		sort.Slice(g.Machines, func(i, j int) bool { return g.Machines[i].Name < g.Machines[j].Name })
		if g.Snapshot != nil {
			g.RAMShared = g.Snapshot.MemBytes
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}

func esc(s string) string { return html.EscapeString(s) }

func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%d MB", b>>20)
	case b >= 1<<10:
		return fmt.Sprintf("%d KB", b>>10)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func age(t time.Time, now time.Time) string {
	d := now.Sub(t).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

// Render produce el informe: el mapa de llamadas.
func Render(info *api.Info, groups []Group, endpoint string, now time.Time) string {
	return RenderMap(info, groups, endpoint, now, "")
}

func stat(b *strings.Builder, value, label, class string) {
	b.WriteString(`<div class="stat ` + class + `"><span class="v">` + esc(value) +
		`</span><span class="l">` + esc(label) + `</span></div>`)
}

func stateClass(s api.State) string {
	switch s {
	case api.StateRunning:
		return "running"
	case api.StateWarm:
		return "warm"
	default:
		return "other"
	}
}

func lastOp(m *api.Machine) string {
	switch {
	case m.ThawMS > 0 && m.State == api.StateRunning:
		return fmt.Sprintf("thaw %d ms", m.ThawMS)
	case m.State == api.StateWarm:
		return fmt.Sprintf("freeze %d ms", m.FreezeMS)
	case m.BootMS > 0:
		return fmt.Sprintf("boot %d ms", m.BootMS)
	}
	return "—"
}

func labels(l map[string]string) string {
	if len(l) == 0 {
		return `<span class="muted">—</span>`
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		sb.WriteString(`<span class="chip">` + esc(k) + `=` + esc(l[k]) + `</span>`)
	}
	return sb.String()
}

// diagram dibuja host → grupos → máquinas en SVG.
//
// Layout jerárquico determinista, no dirigido por fuerzas: la topología ES una
// jerarquía y un grafo de fuerzas solo la haría más difícil de leer, además de

func trunc(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

// names lista los nombres de herramienta en compacto: es lo barato del catálogo.
func names(tools []api.ToolSpec) string {
	if len(tools) == 0 {
		return ` <span class="muted">(sin catálogo)</span>`
	}
	ns := make([]string, 0, len(tools))
	for _, t := range tools {
		ns = append(ns, t.Name)
	}
	sort.Strings(ns)
	return fmt.Sprintf(` — %d herramienta(s): <span class="tools">%s</span>`,
		len(ns), esc(strings.Join(ns, ", ")))
}
