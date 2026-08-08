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
	Machines  []*api.Machine
	RAMShared int64
}

// Build agrupa por la etiqueta "service" y, en su defecto, por el snapshot del
// que salieron: dos máquinas del mismo snapshot comparten memoria, así que
// pertenecen juntas aunque nadie las haya etiquetado.
func Build(machines []*api.Machine, snaps []*api.Snapshot) []Group {
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

// Render produce el HTML completo.
func Render(info *api.Info, groups []Group, endpoint string, now time.Time) string {
	var b strings.Builder

	var running, warm, other int
	var diskOwn, ramShared int64
	for _, g := range groups {
		ramShared += g.RAMShared
		for _, m := range g.Machines {
			diskOwn += m.DiskBytes
			switch m.State {
			case api.StateRunning:
				running++
			case api.StateWarm:
				warm++
			default:
				other++
			}
		}
	}

	b.WriteString(`<!doctype html><html lang="es"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>kindling — topología</title>`)
	b.WriteString("<style>" + css + "</style></head><body>")

	// ── cabecera ──────────────────────────────────────────────────────────────
	b.WriteString(`<header><h1>kindling</h1><p class="sub">`)
	b.WriteString(esc(endpoint))
	if info != nil {
		kvm := "sin KVM"
		if info.KVM {
			kvm = "KVM ok"
		}
		b.WriteString(" · " + esc(kvm) + " · " + esc(strings.TrimSpace(info.Firecrack)))
	}
	b.WriteString(` · generado ` + now.Format("2006-01-02 15:04:05") + `</p></header>`)

	b.WriteString(`<section class="stats">`)
	stat(&b, fmt.Sprintf("%d", running), "en marcha", "running")
	stat(&b, fmt.Sprintf("%d", warm), "congeladas", "warm")
	stat(&b, fmt.Sprintf("%d", other), "detenidas", "other")
	stat(&b, human(ramShared), "memoria compartida", "")
	stat(&b, human(diskOwn), "disco propio", "")
	b.WriteString(`</section>`)

	// ── diagrama ──────────────────────────────────────────────────────────────
	b.WriteString(`<section class="card"><h2>Topología</h2>`)
	b.WriteString(diagram(groups))
	b.WriteString(`<p class="legend">
    <span class="k running"></span> en marcha
    <span class="k warm"></span> congelada (~30 ms para volver)
    <span class="k other"></span> detenida o fallida
    &nbsp;·&nbsp; <b>→</b> salida a internet &nbsp; <b>⌀</b> aislada
  </p></section>`)

	// ── grupos ────────────────────────────────────────────────────────────────
	for _, g := range groups {
		b.WriteString(`<section class="card"><h2>` + esc(g.Service))
		if g.Snapshot != nil {
			b.WriteString(`<span class="tag">snapshot ` + esc(g.Snapshot.Name) +
				` · ` + human(g.Snapshot.MemBytes) + ` compartidos</span>`)
		}
		b.WriteString(`</h2>`)

		if len(g.Machines) == 0 {
			b.WriteString(`<p class="empty">Sin instancias. Arranca una con <code>kling run -from ` +
				esc(g.Snapshot.Name) + `</code></p></section>`)
			continue
		}

		b.WriteString(`<table><thead><tr><th>Máquina</th><th>Estado</th><th>IP</th>` +
			`<th>Salida</th><th>CPU/RAM</th><th>Disco</th><th>Última operación</th><th>Edad</th>` +
			`<th>Etiquetas</th></tr></thead><tbody>`)
		for _, m := range g.Machines {
			ip := m.IP
			if m.State != api.StateRunning || ip == "" {
				ip = "—"
			}
			eg := "⌀"
			if m.Egress == "internet" {
				eg = "→"
			}
			b.WriteString(`<tr><td><b>` + esc(m.Name) + `</b><span class="id">` + esc(m.ID[:8]) + `</span></td>`)
			b.WriteString(`<td><span class="badge ` + stateClass(m.State) + `">` + esc(string(m.State)) + `</span></td>`)
			b.WriteString(`<td class="mono">` + esc(ip) + `</td>`)
			b.WriteString(`<td class="mono ctr">` + eg + `</td>`)
			b.WriteString(fmt.Sprintf(`<td class="mono">%d / %d MiB</td>`, m.VCPUs, m.MemMiB))
			b.WriteString(`<td class="mono">` + human(m.DiskBytes) + `</td>`)
			b.WriteString(`<td class="mono">` + esc(lastOp(m)) + `</td>`)
			b.WriteString(`<td class="mono">` + age(m.CreatedAt, now) + `</td>`)
			b.WriteString(`<td>` + labels(m.Labels) + `</td></tr>`)
		}
		b.WriteString(`</tbody></table></section>`)
	}

	b.WriteString(`<footer>Instantánea estática. Regenera con <code>kling export</code>.</footer>`)
	b.WriteString(`</body></html>`)
	return b.String()
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
// cambiar en cada regeneración.
func diagram(groups []Group) string {
	const (
		hostX, colGroup, colMachine = 40, 230, 470
		rowH, boxH, top             = 34, 26, 30
	)
	rows := 0
	for _, g := range groups {
		rows += 1 + len(g.Machines)
		if len(g.Machines) == 0 {
			rows++
		}
	}
	h := top*2 + rows*rowH
	if h < 160 {
		h = 160
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<div class="diagram"><svg viewBox="0 0 900 %d" role="img" aria-label="Topología">`, h)

	hostY := h / 2
	fmt.Fprintf(&b, `<rect class="node host" x="%d" y="%d" width="150" height="34" rx="6"/>`, hostX, hostY-17)
	fmt.Fprintf(&b, `<text class="lbl host" x="%d" y="%d">host</text>`, hostX+12, hostY+5)

	y := top
	for _, g := range groups {
		gy := y + boxH/2
		// host → grupo
		fmt.Fprintf(&b, `<path class="link" d="M%d %d C%d %d %d %d %d %d"/>`,
			hostX+150, hostY, hostX+195, hostY, colGroup-45, gy, colGroup, gy)

		gw := 200
		fmt.Fprintf(&b, `<rect class="node group" x="%d" y="%d" width="%d" height="%d" rx="6"/>`,
			colGroup, y, gw, boxH)
		fmt.Fprintf(&b, `<text class="lbl group" x="%d" y="%d">%s</text>`,
			colGroup+10, y+17, esc(trunc(g.Service, 22)))
		if g.RAMShared > 0 {
			fmt.Fprintf(&b, `<text class="lbl shared" x="%d" y="%d">%s compartidos</text>`,
				colGroup+gw+10, y+17, human(g.RAMShared))
		}
		y += rowH

		if len(g.Machines) == 0 {
			fmt.Fprintf(&b, `<text class="lbl muted" x="%d" y="%d">sin instancias</text>`, colMachine, y+17)
			y += rowH
			continue
		}
		for _, m := range g.Machines {
			my := y + boxH/2
			fmt.Fprintf(&b, `<path class="link" d="M%d %d C%d %d %d %d %d %d"/>`,
				colGroup+gw, gy, colGroup+gw+40, gy, colMachine-40, my, colMachine, my)
			fmt.Fprintf(&b, `<rect class="node %s" x="%d" y="%d" width="190" height="%d" rx="6"/>`,
				stateClass(m.State), colMachine, y, boxH)
			fmt.Fprintf(&b, `<text class="lbl" x="%d" y="%d">%s</text>`,
				colMachine+10, y+17, esc(trunc(m.Name, 18)))
			ip := m.IP
			if m.State != api.StateRunning || ip == "" {
				ip = "—"
			}
			fmt.Fprintf(&b, `<text class="lbl meta" x="%d" y="%d">%s · %s</text>`,
				colMachine+200, y+17, esc(ip), esc(lastOp(m)))
			y += rowH
		}
	}
	b.WriteString(`</svg></div>`)
	return b.String()
}

func trunc(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}
