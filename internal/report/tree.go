package report

// El renderizador. No sabe nada de kindling: recibe un árbol de nodos y lo
// dibuja de izquierda a derecha, un nodo por fila salvo los padres, que se
// centran sobre sus hijos. Las anotaciones grises van todas a una misma
// columna a la derecha para que nunca choquen con una caja.
const jsTree = `
const BOXW = 196, BOXH = 42, COLW = 250, ROWH = 58, PADX = 24, PADY = 22;

let view = 'topologia';
const opened = {};            // por vista: qué nodos están abiertos
let path = [];                // profundizar: ids desde la raíz hasta el foco
let selId = null;

const $ = id => document.getElementById(id);
const cut = (s, max) => !s ? '' : (s.length > max ? s.slice(0, max - 1) + '…' : s);
const xml = s => String(s).replace(/[&<>"]/g, c =>
  ({'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;'}[c]));

function openSet() { return opened[view] || (opened[view] = new Set()); }

// ── construir y enfocar ────────────────────────────────────────────────────
function buildTree() {
  const root = VIEWS[view].build();
  const set = openSet();
  (function mark(nd) {
    if (set.size) nd.open = set.has(nd.id);
    nd.kids.forEach(mark);
  })(root);
  return root;
}

// focus baja por path; si un id ya no existe (el modelo cambió), se queda donde pueda.
function focus(root) {
  let cur = root, trail = [root];
  for (const id of path) {
    const nxt = cur.kids.find(k => k.id === id);
    if (!nxt) break;
    cur = nxt; trail.push(cur);
  }
  cur.open = true;
  return {node: cur, trail};
}

// ── disposición ────────────────────────────────────────────────────────────
function layout(root) {
  let row = 0, maxDepth = 0;
  (function place(nd, depth) {
    nd.depth = depth; nd.x = PADX + depth * COLW;
    maxDepth = Math.max(maxDepth, depth);
    const kids = nd.open ? nd.kids : [];
    if (kids.length) {
      kids.forEach(k => place(k, depth + 1));
      nd.y = (kids[0].y + kids[kids.length - 1].y) / 2;
    } else {
      nd.y = PADY + row * ROWH; row++;
    }
  })(root, 0);
  return {rows: row, noteX: PADX + (maxDepth + 1) * COLW - 34};
}

function flatten(root) {
  const out = [];
  (function walk(nd) { out.push(nd); if (nd.open) nd.kids.forEach(walk); })(root);
  return out;
}

// ── dibujo ─────────────────────────────────────────────────────────────────
function draw(root) {
  const {rows, noteX} = layout(root);
  const nodes = flatten(root);
  const h = Math.max(140, PADY + rows * ROWH + 20);
  const w = noteX + 300;
  let s = '<svg viewBox="0 0 ' + w + ' ' + h + '" role="img" aria-label="' +
          xml(VIEWS[view].label) + '">';

  for (const nd of nodes) {
    if (!nd.open) continue;
    for (const k of nd.kids) {
      const x1 = nd.x + BOXW, y1 = nd.y + BOXH / 2, x2 = k.x, y2 = k.y + BOXH / 2;
      const c = (x2 - x1) / 2;
      s += '<path class="edge e-' + k.cls + '" d="M' + x1 + ' ' + y1 +
           ' C' + (x1 + c) + ' ' + y1 + ' ' + (x2 - c) + ' ' + y2 + ' ' + x2 + ' ' + y2 + '"/>';
    }
  }

  for (const nd of nodes) {
    const has = nd.kids.length > 0;
    s += '<g class="node' + (nd.id === selId ? ' sel' : '') + '" data-id="' + xml(nd.id) +
         '" tabindex="0" role="button">';
    s += '<rect class="box b-' + nd.cls + '" x="' + nd.x + '" y="' + nd.y +
         '" width="' + BOXW + '" height="' + BOXH + '" rx="8"/>';
    const ty = nd.sub ? nd.y + 19 : nd.y + BOXH / 2 + 5;
    s += '<text class="t" x="' + (nd.x + 13) + '" y="' + ty + '">' + xml(cut(nd.label, 24)) + '</text>';
    if (nd.sub)
      s += '<text class="s" x="' + (nd.x + 13) + '" y="' + (nd.y + 34) + '">' +
           xml(cut(nd.sub, 30)) + '</text>';
    if (has)
      s += '<text class="caret" x="' + (nd.x + BOXW - 13) + '" y="' + (nd.y + BOXH / 2 + 5) +
           '">' + (nd.open ? '−' : '+') + '</text>';
    s += '</g>';

    if (nd.note)
      s += '<text class="note" x="' + noteX + '" y="' + (nd.y + BOXH / 2 + 5) + '">' +
           xml(cut(nd.note, 46)) + '</text>';
  }
  return s + '</svg>';
}

// ── panel ──────────────────────────────────────────────────────────────────
function showPanel(nd) {
  $('p-title').textContent = nd.label;
  $('p-empty').hidden = true;
  const body = $('p-body'); body.hidden = false;
  const d = nd.detail || {};
  let h = '';

  if (nd.kids.length)
    h += '<button class="drill" data-drill="' + xml(nd.id) + '">Drill into ' +
         xml(nd.label) + ' →</button>';

  if (d.rows && d.rows.length) {
    h += '<dl class="detail">';
    for (const [k, v] of d.rows) h += '<dt>' + xml(k) + '</dt><dd>' + xml(v) + '</dd>';
    h += '</dl>';
  }
  if (d.steps && d.steps.length)
    h += '<div class="steps"><h3>What happens when you call it</h3><ol>' +
         d.steps.map(x => '<li>' + xml(x) + '</li>').join('') + '</ol></div>';
  if (d.note)
    h += '<div class="persist ' + d.note.cls + '"><h3>' + xml(d.note.title) + '</h3><p>' +
         xml(d.note.text) + '</p></div>';
  if (d.chips && d.chips.length)
    h += '<div class="chips">' + d.chips.map(c => '<span class="chip">' + xml(c) + '</span>').join('') +
         '</div>';

  body.innerHTML = h || '<p class="empty">No further detail.</p>';
}

// ── pintar todo ────────────────────────────────────────────────────────────
function render() {
  const root = buildTree();
  const {node, trail} = focus(root);

  $('tabs').innerHTML = Object.entries(VIEWS).map(([k, v]) =>
    '<button class="tab' + (k === view ? ' on' : '') + '" data-view="' + k + '">' +
    xml(v.label) + '</button>').join('');
  $('viewhint').textContent = VIEWS[view].hint;

  $('crumbs').innerHTML = trail.length < 2 ? '' :
    trail.map((t, i) => '<button class="crumb" data-up="' + i + '">' + xml(t.label) + '</button>')
         .join('<span class="sep">›</span>');

  $('map').innerHTML = draw(node);
  $('legend').innerHTML = VIEWS[view].legend
    .map(([c, t]) => '<span class="k ' + c + '"></span> ' + xml(t)).join('');

  const sel = flatten(node).find(x => x.id === selId);
  if (sel) showPanel(sel);
}

// ── interacción ────────────────────────────────────────────────────────────
document.addEventListener('click', e => {
  const tab = e.target.closest('[data-view]');
  if (tab) { view = tab.dataset.view; path = []; selId = null;
             $('p-body').hidden = true; $('p-empty').hidden = false;
             $('p-title').textContent = 'Choose a node'; return render(); }

  const up = e.target.closest('[data-up]');
  if (up) { path = path.slice(0, +up.dataset.up); return render(); }

  const drill = e.target.closest('[data-drill]');
  if (drill) {
    const id = drill.dataset.drill;
    const {node} = focus(buildTree());
    if (node.id !== id && node.kids.some(k => k.id === id)) path = [...path, id];
    return render();
  }

  const g = e.target.closest('.node');
  if (!g) return;
  const id = g.dataset.id;
  const {node} = focus(buildTree());
  const nd = flatten(node).find(x => x.id === id);
  if (!nd) return;
  selId = id;
  if (nd.kids.length) {
    const set = openSet();
    // La primera interacción fija el conjunto: hasta entonces manda lo que
    // trajera la vista por defecto.
    if (!set.size) flatten(node).forEach(x => { if (x.open) set.add(x.id); });
    set.has(id) ? set.delete(id) : set.add(id);
  }
  render();
});

document.addEventListener('keydown', e => {
  if (e.key !== 'Enter' && e.key !== ' ') return;
  const g = document.activeElement && document.activeElement.closest('.node');
  if (!g) return;
  e.preventDefault(); g.dispatchEvent(new MouseEvent('click', {bubbles: true}));
});

render();
`
