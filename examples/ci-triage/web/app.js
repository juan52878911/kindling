'use strict';
// Todo lo que viene del log o de VON entra con textContent: nada se interpreta
// como HTML.
const $ = (id) => document.getElementById(id);
let current = null;

function el(tag, cls, text) {
  const e = document.createElement(tag);
  if (cls) e.className = cls;
  if (text !== undefined) e.textContent = text;
  return e;
}

$('file').addEventListener('change', async (ev) => {
  const f = ev.target.files[0];
  if (!f) return;
  if (f.size > 8 << 20) { status('file larger than 8 MiB', true); return; }
  $('log').value = await f.text();
});

function status(msg, err) { $('status').textContent = msg; $('status').className = err ? 'err' : ''; }

$('go').addEventListener('click', async () => {
  const text = $('log').value;
  if (!text.trim()) { status('paste a log first', true); return; }
  $('go').disabled = true;
  status('analyzing… (VON may need a moment to wake up)');
  try {
    const r = await fetch('/api/analyze', { method: 'POST', headers: { 'Content-Type': 'text/plain', 'X-CI-Triage': '1' }, body: text });
    const body = await r.json();
    if (!r.ok) throw new Error(body.error || r.statusText);
    current = body;
    render(body);
    status('');
  } catch (e) {
    status(String(e.message || e), true);
  } finally {
    $('go').disabled = false;
  }
});

function render(b) {
  const r = b.result;
  $('result').hidden = false;
  $('category').textContent = r.category;
  const layers = { chispa: 'Chispa, confident', von: 'VON (Chispa was unsure)', 'chispa-unsure': 'Chispa, unsure' };
  $('layer').textContent = layers[r.decided_by] || r.decided_by;
  let why = `Chispa: ${r.chispa_category} p=${r.confidence.toFixed(2)}`;
  if (r.candidates && r.candidates.length > 1) why += ' · candidates ' + r.candidates.map((c) => `${c.label} ${c.prob.toFixed(2)}`).join(', ');
  if (r.von_error) why += ' · VON did not answer: ' + r.von_error;
  $('why').textContent = why;
  const s = $('summary');
  s.replaceChildren();
  if (r.von) {
    s.append(el('div', '', r.von.summary), el('div', '', 'Next step: ' + r.von.next_step));
  }
  const t = r.timing;
  $('timing').textContent = `${r.lines} lines (${r.format}), ${r.lines_classified} scored by Chispa in ${t.lines_ms.toFixed(1)} ms · category ${t.category_ms.toFixed(2)} ms` +
    (t.von_ms ? ` · VON ${Math.round(t.von_ms)} ms` : '') + ` · total ${t.total_ms.toFixed(0)} ms`;

  const sel = $('choice');
  sel.replaceChildren();
  for (const c of b.categories) {
    const o = el('option', '', c);
    o.value = c;
    if (c === r.category) o.selected = true;
    sel.append(o);
  }
  $('saved').textContent = '';

  const inChunk = new Set();
  for (const c of r.chunks || []) for (let n = c.from; n <= c.to; n++) inChunk.add(n);
  const box = $('lines');
  box.replaceChildren();
  let first = null;
  for (const l of b.lines) {
    if (l.gap) box.append(el('div', 'gap', '…'));
    const row = el('div', 'l' + (inChunk.has(l.n) ? ' chunk' : (l.s >= 0.5 ? ' hit' : '')));
    row.append(el('span', 'n', String(l.n)), el('span', 's', l.s ? l.s.toFixed(2) : ''), el('span', 't', l.t));
    box.append(row);
    if (!first && inChunk.has(l.n)) first = row;
  }
  if (first) first.scrollIntoView({ block: 'center' });
}

$('save').addEventListener('click', async () => {
  if (!current) return;
  $('save').disabled = true;
  try {
    const r = await fetch('/api/feedback', { method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: current.id, category: $('choice').value, note: $('note').value }) });
    const body = await r.json();
    if (!r.ok) throw new Error(body.error || r.statusText);
    $('saved').textContent = body.agreed ? 'saved: confirmed' : `saved: corrected to ${body.label}`;
    $('saved').className = '';
  } catch (e) {
    $('saved').textContent = String(e.message || e);
    $('saved').className = 'err';
  } finally {
    $('save').disabled = false;
  }
});
