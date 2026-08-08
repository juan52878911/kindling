package report

import (
	"encoding/json"
	"fmt"
	"strings"
)

// El informe es un DIAGRAMA, no una tabla con adornos.
//
// Lo que hay que ver de un vistazo es el camino de una llamada: quién la hace,
// qué se enciende a su paso, qué queda vivo después y dónde acaba lo que se
// escriba. Un carril por servicio, coloreado por estado, y al pulsarlo se
// ilumina su recorrido completo.

const (
	dxCaller  = 30
	dxGateway = 190
	dxService = 380
	dxFate    = 640
	dxStore   = 880
	canvasW   = 1210
	rowH      = 74
	boxH      = 46
	topPad    = 56
)

type node struct {
	Service   string   `json:"service"`
	Kind      string   `json:"kind"`    // microvm | externo | roto
	State     string   `json:"state"`   // atendiendo | listo | dormido | bloqueado
	Latency   string   `json:"latency"` // coste de la próxima llamada
	Tools     int      `json:"tools"`   // cuántas herramientas
	Ephemeral bool     `json:"ephemeral"`
	Fate      string   `json:"fate"`    // qué pasa con la máquina al terminar
	Store     string   `json:"store"`   // dónde acaba lo que escriba
	StoreOK   bool     `json:"storeOk"` // ¿sobrevive?
	Steps     []string `json:"steps"`   // el flujo, paso a paso
	Writers   []string `json:"writers"` // herramientas que escriben
	Names     []string `json:"names"`   // catálogo
	Live      int      `json:"live"`
	Warm      int      `json:"warm"`
}

func buildNodes(groups []Group, memService string) []node {
	out := make([]node, 0, len(groups))
	for _, g := range groups {
		r := g.Readiness()
		n := node{
			Service: g.Service, Tools: toolCount(g), Steps: g.Flow(),
			Writers: g.writers(), Names: toolNames(g),
			Live: r.Live, Warm: r.Warm, Latency: r.Latency,
		}

		switch {
		case g.Link != nil:
			n.Kind, n.State = "externo", "listo"
			n.Fate = "sigue vivo donde está"
			n.Store, n.StoreOK = "propio, fuera de kindling", true
		case r.Blocked != "":
			n.Kind, n.State = "roto", "bloqueado"
			n.Latency = r.Blocked
			n.Fate, n.Store = "—", "—"
		default:
			n.Kind = "microvm"
			n.Ephemeral = g.Ephemeral()
			switch {
			case r.Live > 0:
				n.State = "atendiendo"
			case r.Warm > 0:
				n.State = "dormido"
			default:
				n.State = "listo"
			}
			if n.Ephemeral {
				n.Fate = "la máquina muere"
				n.Store, n.StoreOK = "se pierde", false
			} else {
				n.Fate = "se congela al quedar ociosa"
				n.Store, n.StoreOK = "vive mientras viva la instancia", false
			}
		}

		if len(n.Writers) == 0 && n.Kind != "roto" {
			n.Store, n.StoreOK = "no escribe nada", true
		} else if !n.StoreOK && memService != "" {
			n.Store += " → usa " + memService
		}
		out = append(out, n)
	}
	return out
}

// style es la clase de color del carril. Un servicio externo se pinta de azul
// aunque su estado sea "listo": lo que importa visualmente es que no es una
// microVM nuestra.
func (n node) style() string {
	if n.Kind == "externo" {
		return "ext"
	}
	return n.State
}

func toolCount(g Group) int {
	switch {
	case g.Link != nil:
		return len(g.Link.Tools)
	case g.Snapshot != nil:
		return len(g.Snapshot.Tools)
	}
	return 0
}

func toolNames(g Group) []string {
	out := []string{}
	switch {
	case g.Link != nil:
		for _, t := range g.Link.Tools {
			out = append(out, t.Name)
		}
	case g.Snapshot != nil:
		for _, t := range g.Snapshot.Tools {
			out = append(out, t.Name)
		}
	}
	return out
}

// diagramSVG dibuja el mapa de llamadas.
func diagramSVG(nodes []node, memService string) string {
	h := topPad + len(nodes)*rowH + 40
	if h < 300 {
		h = 300
	}
	mid := topPad + (len(nodes)*rowH)/2 - rowH/2

	var b strings.Builder
	fmt.Fprintf(&b, `<div class="map"><svg viewBox="0 0 %d %d" role="img" aria-label="Mapa de llamadas">`, canvasW, h)

	// Encabezados de carril: dicen qué significa cada columna.
	head := func(x int, t string) {
		fmt.Fprintf(&b, `<text class="col" x="%d" y="28">%s</text>`, x, esc(t))
	}
	head(dxCaller, "quien llama")
	head(dxGateway, "gateway")
	head(dxService, "servicio")
	head(dxFate, "al terminar")
	head(dxStore, "si escribe algo")

	// Modelo y gateway, una sola vez.
	box(&b, dxCaller, mid, 120, "caller", "modelo", "", "")
	box(&b, dxGateway, mid, 130, "gw", "kling gateway", "", "")
	link(&b, dxCaller+120, mid+boxH/2, dxGateway, mid+boxH/2, "trunk", "")

	for i, n := range nodes {
		y := topPad + i*rowH
		cy := y + boxH/2

		// gateway -> servicio
		lat := shortLatency(n.Latency)
		fmt.Fprintf(&b, `<g class="wire" data-svc="%s">`, esc(n.Service))
		link(&b, dxGateway+130, mid+boxH/2, dxService, cy, "e-"+n.style(), lat)
		b.WriteString(`</g>`)

		sub := fmt.Sprintf("%d herramientas", n.Tools)
		fmt.Fprintf(&b, `<g class="svc" data-svc="%s" tabindex="0">`, esc(n.Service))
		box(&b, dxService, y, 200, "n-"+n.style(), n.Service, sub, n.style())
		b.WriteString(`</g>`)

		if n.Kind == "roto" {
			fmt.Fprintf(&b, `<g class="wire" data-svc="%s"><text class="fate blocked" x="%d" y="%d">`+
				`no ejecutable</text></g>`, esc(n.Service), dxFate, cy+5)
			continue
		}

		// servicio -> destino de la máquina
		fmt.Fprintf(&b, `<g class="wire" data-svc="%s">`, esc(n.Service))
		link(&b, dxService+200, cy, dxFate-8, cy, "e-"+n.style(), "")
		cls := "fate keep"
		switch {
		case n.Kind == "externo":
			cls = "fate ext"
		case n.Ephemeral:
			cls = "fate die"
		}
		fmt.Fprintf(&b, `<text class="%s" x="%d" y="%d">%s</text>`, cls, dxFate, cy+5, esc(n.Fate))
		b.WriteString(`</g>`)

		// destino de lo que escriba
		if len(n.Writers) > 0 {
			fmt.Fprintf(&b, `<g class="wire" data-svc="%s">`, esc(n.Service))
			link(&b, dxFate+170, cy, dxStore-8, cy, "e-store", "")
			sc := "store bad"
			if n.StoreOK {
				sc = "store good"
			}
			fmt.Fprintf(&b, `<text class="%s" x="%d" y="%d">%s</text>`,
				sc, dxStore, cy+5, esc(trimStore(n.Store, memService)))
			b.WriteString(`</g>`)
		}
	}

	b.WriteString(`</svg></div>`)
	return b.String()
}

// trimStore acorta el destino para que quepa; el detalle va en el panel.
func trimStore(s, mem string) string {
	if i := strings.Index(s, " → "); i > 0 {
		return s[:i] + " → " + mem
	}
	if len([]rune(s)) > 40 {
		return string([]rune(s)[:39]) + "…"
	}
	return s
}

// shortLatency deja solo la cifra: "~250 ms", "inmediata". Lo demás va al panel.
func shortLatency(l string) string {
	if i := strings.Index(l, ":"); i > 0 {
		return strings.TrimSpace(l[:i])
	}
	if len([]rune(l)) > 12 {
		return "externo"
	}
	return l
}

func box(b *strings.Builder, x, y, w int, cls, title, sub, badge string) {
	fmt.Fprintf(b, `<rect class="node %s" x="%d" y="%d" width="%d" height="%d" rx="9"/>`,
		cls, x, y, w, boxH)
	ty := y + 20
	if sub == "" {
		ty = y + boxH/2 + 5
	}
	fmt.Fprintf(b, `<text class="t" x="%d" y="%d">%s</text>`, x+14, ty, esc(title))
	if sub != "" {
		fmt.Fprintf(b, `<text class="s" x="%d" y="%d">%s</text>`, x+14, y+36, esc(sub))
	}
	if badge != "" {
		fmt.Fprintf(b, `<circle class="dot %s" cx="%d" cy="%d" r="5"/>`, badge, x+w-16, y+16)
	}
}

// link dibuja una curva entre dos puntos, con etiqueta opcional.
func link(b *strings.Builder, x1, y1, x2, y2 int, cls, label string) {
	c := (x2 - x1) / 2
	fmt.Fprintf(b, `<path class="edge %s" d="M%d %d C%d %d %d %d %d %d"/>`,
		cls, x1, y1, x1+c, y1, x2-c, y2, x2, y2)
	if label != "" {
		fmt.Fprintf(b, `<text class="lat" x="%d" y="%d">%s</text>`, x2-10, y2-9, esc(label))
	}
}

// nodesJSON alimenta el panel lateral, que se rellena al pulsar un servicio.
func nodesJSON(nodes []node) string {
	b, err := json.Marshal(nodes)
	if err != nil {
		return "[]"
	}
	// El JSON va dentro de un <script>: hay que impedir que un "</script>" en
	// algún nombre de herramienta cierre la etiqueta antes de tiempo.
	return strings.ReplaceAll(string(b), "<", `<`)
}
