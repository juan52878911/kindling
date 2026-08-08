package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// RenderMap produce el informe gráfico: un mapa de llamadas donde cada servicio
// es un carril, y al pulsarlo se ilumina su recorrido y se detalla su flujo.
func RenderMap(info *api.Info, groups []Group, endpoint string, now time.Time, memService string) string {
	nodes := buildNodes(groups, memService)

	var running, warm, ready, ext int
	for _, n := range nodes {
		switch {
		case n.Kind == "externo":
			ext++
		case n.State == "atendiendo":
			running++
		case n.State == "dormido":
			warm++
		case n.State == "listo":
			ready++
		}
	}

	var b strings.Builder
	b.WriteString(`<!doctype html><html lang="es"><head><meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<title>kindling — mapa de llamadas</title>`)
	b.WriteString("<style>" + css + cssMap + "</style></head><body>")

	b.WriteString(`<header><h1>kindling</h1><p class="sub">`)
	b.WriteString(esc(endpoint))
	if info != nil {
		kvm := "sin KVM"
		if info.KVM {
			kvm = "KVM ok"
		}
		b.WriteString(" · " + esc(kvm) + " · " + esc(strings.TrimSpace(info.Firecrack)))
	}
	b.WriteString(" · " + now.Format("2006-01-02 15:04") + `</p></header>`)

	b.WriteString(`<section class="stats">`)
	stat(&b, itoa(running), "atendiendo", "running")
	stat(&b, itoa(warm), "dormidos, vuelven en ~30 ms", "warm")
	stat(&b, itoa(ready), "listos, sin instancia", "")
	stat(&b, itoa(ext), "externos", "")
	b.WriteString(`</section>`)

	// ── el mapa ───────────────────────────────────────────────────────────────
	b.WriteString(`<section class="card"><h2>Mapa de llamadas` +
		`<span class="hint">pulsa un servicio para ver su recorrido</span></h2>`)
	b.WriteString(diagramSVG(nodes, memService))
	b.WriteString(`<p class="legend">
    <span class="k running"></span> atendiendo
    <span class="k warm"></span> dormido, vuelve en ~30 ms
    <span class="k ready"></span> listo, sin instancia viva
    <span class="k ext"></span> externo
  </p></section>`)

	// ── panel de detalle, que rellena el JS al pulsar ─────────────────────────
	b.WriteString(`<section class="card" id="panel"><h2 id="p-title">Elige un servicio</h2>
  <p class="empty" id="p-empty">Pulsa cualquier carril del mapa para ver qué se enciende,
  qué queda vivo al terminar y dónde acaba lo que esa herramienta escriba.</p>
  <div id="p-body" hidden>
    <div class="steps"><h3>Qué pasa al llamarla</h3><ol id="p-steps"></ol></div>
    <div class="persist" id="p-persist"></div>
    <div class="tools"><h3>Herramientas</h3><div id="p-tools" class="chips"></div></div>
  </div></section>`)

	// ── diagrama de persistencia ──────────────────────────────────────────────
	b.WriteString(persistenceSVG(memService))

	// ── instancias vivas, si las hay ──────────────────────────────────────────
	b.WriteString(instanceTable(groups, now))

	b.WriteString(`<footer>Instantánea estática. Regenera con <code>kling export</code>.</footer>`)
	b.WriteString(`<script id="data" type="application/json">` + nodesJSON(nodes) + `</script>`)
	b.WriteString(`<script>` + mapJS + `</script>`)
	b.WriteString(`</body></html>`)
	return b.String()
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }

// persistenceSVG responde en un dibujo a "¿qué pasa si una herramienta quiere
// guardar algo?", que es la duda que más se repite.
func persistenceSVG(memService string) string {
	dest := memService
	if dest == "" {
		dest = "un servicio de memoria"
	}
	var b strings.Builder
	b.WriteString(`<section class="card"><h2>Si una herramienta quiere guardar algo</h2>`)
	b.WriteString(`<div class="map"><svg viewBox="0 0 1210 260" role="img" aria-label="Destino de lo que se escribe">`)

	box(&b, 30, 100, 190, "caller", "la herramienta", "escribe algo", "")

	// Tres destinos posibles, de peor a mejor.
	rows := []struct {
		y            int
		cls, título  string
		sub, veredic string
		ok           bool
	}{
		{30, "n-die", "disco de una microVM efímera", "el fichero, la fila, lo que sea", "se pierde al responder", false},
		{100, "n-keep", "disco de una instancia persistente", "sobrevive a congelarse", "se pierde si la instancia se elimina", false},
		{170, "n-ext", dest, "servidor externo, fuera de las microVMs", "sobrevive a todo", true},
	}
	for _, r := range rows {
		cy := r.y + boxH/2
		link(&b, 220, 100+boxH/2, 400, cy, "e-"+strings.TrimPrefix(r.cls, "n-"), "")
		box(&b, 400, r.y, 300, r.cls, r.título, r.sub, "")
		cls := "store bad"
		mark := "✗"
		if r.ok {
			cls, mark = "store good", "✓"
		}
		fmt.Fprintf(&b, `<text class="%s" x="740" y="%d">%s %s</text>`, cls, cy+5, mark, esc(r.veredic))
	}
	b.WriteString(`</svg></div>`)

	if memService == "" {
		b.WriteString(`<p class="legend">No hay servicio de memoria enlazado. ` +
			`Enlaza uno con <code>kling mcp link engram &lt;url&gt;</code> y actívalo con ` +
			`<code>kling memory enable</code>.</p>`)
	}
	b.WriteString(`</section>`)
	return b.String()
}

// mapJS enlaza el mapa con el panel. Sin dependencias: son cuarenta líneas.
const mapJS = `
const data = JSON.parse(document.getElementById('data').textContent);
const byName = Object.fromEntries(data.map(n => [n.service, n]));
const panel = {
  title: document.getElementById('p-title'),
  empty: document.getElementById('p-empty'),
  body:  document.getElementById('p-body'),
  steps: document.getElementById('p-steps'),
  persist: document.getElementById('p-persist'),
  tools: document.getElementById('p-tools'),
};

function select(svc) {
  const n = byName[svc];
  if (!n) return;

  document.body.classList.add('picked');
  document.querySelectorAll('.svc, .wire').forEach(el =>
    el.classList.toggle('on', el.dataset.svc === svc));

  panel.title.textContent = svc + '  ·  ' + n.latency;
  panel.empty.hidden = true;
  panel.body.hidden = false;

  panel.steps.innerHTML = '';
  n.steps.forEach(s => {
    const li = document.createElement('li');
    li.textContent = s;
    panel.steps.appendChild(li);
  });

  const writes = n.writers.length > 0;
  panel.persist.className = 'persist ' + (n.storeOk ? 'good' : 'bad');
  panel.persist.innerHTML =
    '<h3>' + (!writes ? 'No persiste nada'
                      : n.storeOk ? 'Lo que escriba sobrevive'
                                  : 'Ojo con lo que escriba') + '</h3>' +
    '<p>' + (writes
      ? 'Escribe con <b>' + n.writers.join('</b>, <b>') + '</b>. ' + n.store + '.'
      : 'Ninguna de sus herramientas escribe: no hay nada que guardar.') + '</p>';

  panel.tools.innerHTML = '';
  n.names.forEach(t => {
    const s = document.createElement('span');
    s.className = 'chip';
    s.textContent = t;
    panel.tools.appendChild(s);
  });
  panel.title.scrollIntoView({behavior: 'smooth', block: 'nearest'});
}

document.querySelectorAll('.svc').forEach(el => {
  el.addEventListener('click', () => select(el.dataset.svc));
  el.addEventListener('keydown', e => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); select(el.dataset.svc); }
  });
});
`

// instanceTable lista las máquinas que existen ahora mismo. Solo aparece si hay
// alguna: cuando todo está apagado, el mapa ya cuenta lo que hace falta saber.
func instanceTable(groups []Group, now time.Time) string {
	var total int
	for _, g := range groups {
		total += len(g.Machines)
	}
	if total == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(`<section class="card"><h2>Instancias vivas</h2>`)
	b.WriteString(`<table><thead><tr><th>Servicio</th><th>Máquina</th><th>Estado</th><th>IP</th>` +
		`<th>Salida</th><th>CPU/RAM</th><th>Disco</th><th>Última operación</th><th>Edad</th>` +
		`<th>Etiquetas</th></tr></thead><tbody>`)
	for _, g := range groups {
		for _, m := range g.Machines {
			ip := m.IP
			if m.State != api.StateRunning || ip == "" {
				ip = "—"
			}
			eg := "⌀"
			if m.Egress == "internet" {
				eg = "→"
			}
			b.WriteString(`<tr><td>` + esc(g.Service) + `</td>`)
			b.WriteString(`<td><b>` + esc(m.Name) + `</b><span class="id">` + esc(m.ID[:8]) + `</span></td>`)
			b.WriteString(`<td><span class="badge ` + stateClass(m.State) + `">` + esc(string(m.State)) + `</span></td>`)
			b.WriteString(`<td class="mono">` + esc(ip) + `</td>`)
			b.WriteString(`<td class="mono ctr">` + eg + `</td>`)
			b.WriteString(fmt.Sprintf(`<td class="mono">%d / %d MiB</td>`, m.VCPUs, m.MemMiB))
			b.WriteString(`<td class="mono">` + human(m.DiskBytes) + `</td>`)
			b.WriteString(`<td class="mono">` + esc(lastOp(m)) + `</td>`)
			b.WriteString(`<td class="mono">` + age(m.CreatedAt, now) + `</td>`)
			b.WriteString(`<td>` + labels(m.Labels) + `</td></tr>`)
		}
	}
	b.WriteString(`</tbody></table></section>`)
	return b.String()
}
