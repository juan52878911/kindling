package report

import (
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// Render produce el informe: un árbol navegable con varias vistas del mismo
// sistema. La página es un cascarón; el modelo va en un <script> y el navegador
// dibuja el árbol que corresponda a la vista y la profundidad elegidas.
func Render(info *api.Info, groups []Group, endpoint string, now time.Time) string {
	return RenderMap(info, groups, endpoint, now, "")
}

func RenderMap(info *api.Info, groups []Group, endpoint string, now time.Time, memService string) string {
	m := BuildModel(info, groups, endpoint, now, memService)

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>kindling — topology</title>`)
	b.WriteString("<style>" + css + cssTree + "</style></head><body>")

	b.WriteString(`<header><h1>kindling</h1><p class="sub">` + esc(m.Host.Endpoint))
	if m.Host.Firecracker != "" {
		kvm := "no KVM"
		if m.Host.KVM {
			kvm = "KVM ok"
		}
		b.WriteString(" · " + esc(kvm) + " · " + esc(m.Host.Firecracker))
	}
	b.WriteString(" · " + esc(m.Host.Generated) + `</p></header>`)

	b.WriteString(`<section class="stats">`)
	stat(&b, itoa(m.Host.Running), "running", "running")
	stat(&b, itoa(m.Host.Warm), "asleep", "warm")
	stat(&b, itoa(m.Host.Ready), "ready, no instance", "")
	stat(&b, itoa(m.Host.External), "external", "")
	stat(&b, m.Host.RAMShared, "shared memory", "")
	b.WriteString(`</section>`)

	// El árbol y su barra de vistas. Todo lo demás lo pinta el navegador.
	b.WriteString(`<section class="card">
  <nav class="tabs" id="tabs"></nav>
  <p class="viewhint" id="viewhint"></p>
  <div class="crumbs" id="crumbs"></div>
  <div class="map" id="map"></div>
  <p class="legend" id="legend"></p>
</section>

<section class="card" id="panel">
  <h2 id="p-title">Choose a node</h2>
  <p class="empty" id="p-empty">Click any box in the tree. The ones with children expand
  and collapse; details for whichever one you pick show up here.</p>
  <div id="p-body" hidden></div>
</section>`)

	b.WriteString(`<footer>Static snapshot. Regenerate with <code>kling export</code>.</footer>`)
	b.WriteString(`<script id="model" type="application/json">` + modelJSON(m) + `</script>`)
	b.WriteString(`<script>` + jsViews + jsTree + `</script>`)
	b.WriteString(`</body></html>`)
	return b.String()
}
