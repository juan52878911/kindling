// Habitación de la demo: dibuja el estado que manda el servidor (SSE), manda
// órdenes y enseña la traza de cada decisión. Todo el texto que viene del
// usuario o del LLM entra con textContent, nunca como HTML.
"use strict";

const I18N = {
  es: {
    skip: "Ir a las órdenes", title: "Habitación",
    subtitle: "Órdenes de voz decididas por una cascada de modelos: microsegundos cuando basta, un LLM solo cuando hace falta.",
    reset: "Reiniciar", room: "La habitación", live: "en directo", offline: "sin conexión",
    say: "Di una orden", cmdLabel: "Orden", send: "Enviar", placeholder: "aquí hace frío",
    trace: "Cómo se decidió", deciding: "decidiendo…",
    traceEmpty: "Elige una orden y verás qué capa la decide, con qué confianza y en cuánto tiempo.",
    jsonActions: "Acción JSON", rawLLM: "Respuesta del LLM", numbers: "Números", history: "Últimas órdenes",
    stateText: "Estado de los dispositivos",
    foot: "Plantillas y JEV corren en el proceso; el codificador y VON, en microVMs de kindling que se despiertan con la orden y se congelan al quedarse ociosas.",
    groups: { direct: "Directas · plantillas", paraphrase: "Con otras palabras · JEV", indirect: "Indirectas y varias a la vez · capas 3–4", oos: "Fuera de la habitación" },
    layers: { template: "Plantillas", jev: "JEV", encoder: "Codificador", von: "VON (LLM)", none: "Nadie" },
    lstatus: { on: "activa", unavailable: "no disponible", off: "apagada", forced: "forzada" },
    st: { answered: "decidió", escalated: "escala", unavailable: "no disponible", disabled: "apagada por su evaluación",
      skipped: "saltada", notreached: "no hizo falta", error: "error", nomatch: "sin coincidencia" },
    nothing: "No hago nada.", nothingWhy: "Ninguna capa disponible supo qué hacer con esa orden.",
    llmSays: "VON dice", conf: "p", rooms: { living_room: "Salón", kitchen: "Cocina", hallway: "Pasillo", bedroom: "Dormitorio" },
    dev: { light: "Luz", thermostat: "Termostato", blinds: "Persiana", tv: "Tele", speaker: "Altavoz", lock: "Puerta", alarm: "Alarma", fan: "Ventilador", plug: "Enchufe" },
    on: "encendida", off: "apagada", locked: "cerrada con llave", unlocked: "abierta", armed: "activada", disarmed: "desactivada",
    playing: "reproduciendo", paused: "en pausa", muted: "silencio", open: "abierta",
    s: { total: "Órdenes", p50: "p50 total", p50l: "p50", wakes: "Despertares VON", replicas: "Réplicas VON", vonmem: "Memoria VON", mem: "Memoria del proceso" },
    busy: "Hay otra orden decidiéndose; espera un momento.", error: "Error",
  },
  en: {
    skip: "Skip to commands", title: "Room",
    subtitle: "Voice commands decided by a cascade of models: microseconds when that is enough, an LLM only when it is needed.",
    reset: "Reset", room: "The room", live: "live", offline: "offline",
    say: "Say a command", cmdLabel: "Command", send: "Send", placeholder: "it's freezing in here",
    trace: "How it was decided", deciding: "deciding…",
    traceEmpty: "Pick a command to see which layer decides it, how confident it is and how long it takes.",
    jsonActions: "JSON action", rawLLM: "LLM answer", numbers: "Numbers", history: "Recent commands",
    stateText: "Device state",
    foot: "Templates and JEV run in-process; the encoder and VON run in kindling microVMs that wake up with the command and freeze when idle.",
    groups: { direct: "Direct · templates", paraphrase: "In other words · JEV", indirect: "Indirect and several at once · layers 3–4", oos: "Not for this room" },
    layers: { template: "Templates", jev: "JEV", encoder: "Encoder", von: "VON (LLM)", none: "Nobody" },
    lstatus: { on: "on", unavailable: "unavailable", off: "off", forced: "forced" },
    st: { answered: "decided", escalated: "escalates", unavailable: "unavailable", disabled: "off by its eval",
      skipped: "skipped", notreached: "not needed", error: "error", nomatch: "no match" },
    nothing: "Doing nothing.", nothingWhy: "No available layer knew what to do with that command.",
    llmSays: "VON says", conf: "p", rooms: { living_room: "Living room", kitchen: "Kitchen", hallway: "Hallway", bedroom: "Bedroom" },
    dev: { light: "Light", thermostat: "Thermostat", blinds: "Blinds", tv: "TV", speaker: "Speaker", lock: "Door", alarm: "Alarm", fan: "Fan", plug: "Plug" },
    on: "on", off: "off", locked: "locked", unlocked: "unlocked", armed: "armed", disarmed: "disarmed",
    playing: "playing", paused: "paused", muted: "muted", open: "open",
    s: { total: "Commands", p50: "p50 total", p50l: "p50", wakes: "VON wake-ups", replicas: "VON replicas", vonmem: "VON memory", mem: "Process memory" },
    busy: "Another command is being decided; wait a moment.", error: "Error",
  },
};

const LAYERS = ["template", "jev", "encoder", "von"];
const COLORS = {
  white: "#fff4d6", warm: "#ffc978", cool: "#cfe6ff", red: "#ef4444", orange: "#fb923c", yellow: "#facc15", green: "#4ade80",
  blue: "#60a5fa", purple: "#a78bfa", pink: "#f472b6", cyan: "#22d3ee", brown: "#b45309", black: "#555",
};

let lang = "es";
let data = { state: null, layers: [], presets: [], stats: null };
let lastResp = null;
const history = [];
const $ = (id) => document.getElementById(id);
const t = () => I18N[lang];

function load(key) { try { return localStorage.getItem(key); } catch (_) { return null; } }
function save(key, v) { try { localStorage.setItem(key, v); } catch (_) { /* sin almacenamiento: da igual */ } }

function el(tag, attrs, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (k === "class") e.className = v;
    else if (k === "text") e.textContent = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v);
  }
  for (const k of kids) if (k != null) e.append(k);
  return e;
}

function fmtLat(us) {
  if (!us && us !== 0) return "—";
  if (us < 1000) return (us < 10 ? us.toFixed(1) : Math.round(us)) + " µs";
  if (us < 1e6) return (us < 1e4 ? (us / 1000).toFixed(1) : Math.round(us / 1000)) + " ms";
  return (us / 1e6).toFixed(2) + " s";
}

// ── idioma ────────────────────────────────────────────────────────────────
function setLang(l) {
  lang = l === "en" ? "en" : "es";
  save("room.lang", lang);
  document.documentElement.lang = lang;
  document.querySelectorAll("[data-i18n]").forEach((n) => {
    const v = t()[n.dataset.i18n];
    if (typeof v === "string") n.textContent = v;
  });
  document.querySelectorAll(".lang button").forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.lang === lang)));
  $("cmd-input").placeholder = t().placeholder;
  for (const [k, v] of Object.entries(t().rooms)) { const n = $("n-" + k); if (n) n.textContent = v; }
  document.title = "kindling · " + t().title;
  renderLayers(); renderPresets();
  if (data.state) renderState(data.state, []);
  if (data.stats) renderStats(data.stats);
  if (lastResp) renderTrace(lastResp);
  renderHistory();
}

// ── cabecera ──────────────────────────────────────────────────────────────
function renderLayers() {
  const ul = $("layers"); ul.replaceChildren();
  for (const L of data.layers) {
    const li = el("li", { "data-layer": L.name, title: L.detail || "" }, el("b", { text: t().layers[L.name] || L.name }), " " + (t().lstatus[L.status] || L.status));
    if (L.status !== "on" && L.status !== "forced") li.classList.add("off");
    ul.append(li);
  }
}

function renderPresets() {
  const box = $("presets"); box.replaceChildren();
  const groups = { direct: "template", paraphrase: "jev", indirect: "von", oos: "none" };
  for (const [g, layer] of Object.entries(groups)) {
    const items = data.presets.filter((p) => p.lang === lang && p.group === g);
    if (!items.length) continue;
    const chips = el("div", { class: "chips" });
    for (const p of items) chips.append(el("button", { type: "button", onclick: () => send(p.text, p.lang), text: p.text }));
    box.append(el("div", { class: "pgroup", "data-layer": layer }, el("h3", { text: t().groups[g] }), chips));
  }
}

// ── plano ─────────────────────────────────────────────────────────────────
function renderState(s, changed) {
  data.state = s;
  for (const L of s.lights) {
    const col = COLORS[L.color] || COLORS.warm;
    const b = L.on ? L.brightness : 0;
    const glow = $("glow-" + L.area);
    const g = document.querySelector("#g-" + L.area + " .gs0");
    if (g) g.style.stopColor = col;
    const g1 = document.querySelector("#g-" + L.area + " .gs1");
    if (g1) g1.style.stopColor = col;
    if (glow) glow.style.opacity = L.on ? (0.2 + 0.65 * b / 100).toFixed(2) : "0";
    const bulb = $("bulb-" + L.area);
    if (bulb) {
      bulb.classList.toggle("on", L.on);
      bulb.style.fill = L.on ? col : "";
      bulb.style.filter = L.on ? `drop-shadow(0 0 ${4 + b / 8}px ${col})` : "";
    }
  }
  for (const B of s.blinds) {
    const r = $("blind-" + B.area), tx = $("blind-" + B.area + "-t"), sun = $("sun-" + B.area);
    const closed = (100 - B.position) / 100;
    if (B.area === "living_room") r.setAttribute("height", String(200 * closed));
    else r.setAttribute("width", String(160 * closed));
    tx.textContent = B.position + "%";
    if (sun) sun.style.opacity = (B.position / 100 * 0.9).toFixed(2);
  }
  const th = s.thermostat;
  $("thermo-cur").textContent = th.current.toFixed(1) + "°";
  $("thermo-tgt").textContent = (th.on ? "→ " + th.target.toFixed(1) + "°" : t().off);
  const arc = $("thermo-arc");
  const pct = Math.max(0, Math.min(100, (th.target - 10) / 20 * 100));
  arc.style.strokeDasharray = th.on ? `${pct} 100` : "0 100";
  arc.classList.remove("heat", "cool", "idle");
  if (th.on) arc.classList.add(th.current < th.target - 0.05 ? "heat" : th.current > th.target + 0.05 ? "cool" : "idle");
  $("tv-screen").classList.toggle("on", s.tv.on);
  $("tv-screen").classList.toggle("playing", s.tv.on && s.tv.playing);
  $("tv-beam").classList.toggle("on", s.tv.on);
  $("tv-t").textContent = s.tv.on ? "TV · " + (s.tv.muted ? "🔇" : s.tv.volume + "%") : "TV";
  $("spk-waves").classList.toggle("on", s.speaker.on && s.speaker.playing && !s.speaker.muted);
  $("spk-t").textContent = s.speaker.on ? (s.speaker.muted ? "🔇" : s.speaker.volume + "%") : t().off;
  document.querySelector('[data-dev="plug"] .plug').classList.toggle("on", s.plug);
  const fan = $("fan-blades");
  fan.classList.toggle("on", s.fan > 0);
  fan.style.setProperty("--spin", (2.2 - 1.9 * s.fan / 100).toFixed(2) + "s");
  $("fan-t").textContent = s.fan + "%";
  document.querySelector('[data-dev="lock"]').classList.toggle("unlocked", !s.locked);
  $("door").classList.toggle("open", !s.locked);
  document.querySelector('[data-dev="alarm"]').classList.toggle("armed", s.armed);
  for (const id of changed || []) {
    const n = document.querySelector(`[data-dev="${CSS.escape(id)}"]`);
    if (n) { n.classList.remove("pulse"); void n.getBoundingClientRect(); n.classList.add("pulse"); }
  }
  renderDevList(s, changed || []);
}

function renderDevList(s, changed) {
  const T = t(), ul = $("devlist"), rows = [];
  const onoff = (b) => (b ? T.on : T.off);
  for (const L of s.lights) rows.push(["light." + L.area, `${T.dev.light} · ${T.rooms[L.area]}`, L.on ? `${L.brightness}% · ${L.color}` : T.off]);
  rows.push(["thermostat", T.dev.thermostat, `${s.thermostat.current.toFixed(1)}° → ${s.thermostat.target.toFixed(1)}° · ${onoff(s.thermostat.on)}`]);
  for (const B of s.blinds) rows.push(["blind." + B.area, `${T.dev.blinds} · ${T.rooms[B.area]}`, B.position + "% " + T.open]);
  const pl = (p) => (p.on ? `${p.playing ? T.playing : T.paused} · ${p.muted ? T.muted : p.volume + "%"}` : T.off);
  rows.push(["tv", T.dev.tv, pl(s.tv)], ["speaker", T.dev.speaker, pl(s.speaker)]);
  rows.push(["lock", T.dev.lock, s.locked ? T.locked : T.unlocked], ["alarm", T.dev.alarm, s.armed ? T.armed : T.disarmed]);
  rows.push(["fan", T.dev.fan, s.fan ? s.fan + "%" : T.off], ["plug", T.dev.plug, onoff(s.plug)]);
  ul.replaceChildren(...rows.map(([id, k, v]) => el("li", { class: changed.includes(id) ? "flash" : "" }, el("span", { text: k }), el("span", { text: v }))));
}

// ── traza ─────────────────────────────────────────────────────────────────
function nodes(tr) {
  const by = {};
  for (const L of LAYERS) by[L] = { status: "notreached" };
  const steps = tr.steps || [];
  const first = steps[0];
  if (first) {
    if (first.layer === "template") by.template = first;
    else {
      by.template = { status: "nomatch" };
      by.jev = first.layer === "none" ? { status: "unavailable" } : first;
      if (first.layer === "none") by.jev.latency_us = first.latency_us;
    }
  }
  for (const s of steps.slice(1)) by[s.layer] = s;
  return by;
}

function renderTrace(resp) {
  lastResp = resp;
  const T = t(), tr = resp.trace;
  $("trace-empty").hidden = true; $("trace").hidden = false;
  $("t-text").textContent = tr.text;
  $("t-lang").textContent = tr.lang;
  const by = nodes(tr), pipe = $("pipe");
  pipe.replaceChildren();
  for (const L of LAYERS) {
    const n = by[L];
    const li = el("li", { class: n.status, "data-layer": L },
      el("span", { class: "ln", text: T.layers[L] }),
      el("span", { class: "st", text: T.st[n.status] || n.status }));
    if (n.latency_us) li.append(el("span", { class: "lat", text: fmtLat(n.latency_us) }));
    const bits = [];
    if (n.intent) bits.push(n.intent);
    if (n.prob) bits.push(T.conf + "=" + n.prob.toFixed(2));
    if (n.reason) bits.push(n.reason);
    if (n.error) bits.push(n.error);
    if (bits.length) li.append(el("span", { class: "pr", text: bits.join(" · "), title: bits.join(" · ") }));
    pipe.append(li);
  }
  const reply = $("t-reply"); reply.replaceChildren(); reply.className = "reply";
  const says = (resp.effects || []).map((e) => (lang === "en" ? e.say_en : e.say_es)).filter(Boolean);
  const fin = tr.final || {};
  if (says.length) {
    reply.classList.add("done");
    reply.append(says.join(" "));
  } else {
    reply.classList.add("nothing");
    reply.append(fin.reply || T.nothing);
    if (!fin.reply) reply.append(el("small", { text: T.nothingWhy }));
  }
  if (says.length && fin.reply && fin.layer === "von") reply.append(el("small", { text: `${T.llmSays}: “${fin.reply}”` }));
  reply.append(el("small", { text: `${T.layers[tr.decided_by] || T.layers.none} · ${fmtLat(tr.total_us)}` }));
  $("t-json").textContent = JSON.stringify(tr.actions, null, 1).replace(/\n\s*/g, (m) => (m.length > 2 ? "\n  " : "\n"));
  const raw = fin.llm && fin.llm.raw;
  $("raw-wrap").hidden = !raw;
  if (raw) {
    const l = fin.llm;
    $("t-raw").textContent = raw + `\n\n// prompt ${l.prompt_tokens} tok (${l.cached_tokens} cached) · ${l.prompt_ms.toFixed(0)} ms · ${l.completion_tokens} tok · ${l.predicted_ms.toFixed(0)} ms`;
  }
}

// ── números ───────────────────────────────────────────────────────────────
function renderStats(st) {
  data.stats = st;
  const T = t();
  const bars = $("bars"); bars.replaceChildren();
  for (const L of [...LAYERS, "none"]) {
    const n = (st.by_layer || {})[L] || 0;
    if (n) { const s = el("span", { "data-layer": L, title: `${T.layers[L]}: ${n}` }); s.style.flexGrow = String(n); bars.append(s); }
  }
  const dl = $("stats"); dl.replaceChildren();
  const item = (label, value, layer, small) => {
    const d = el("div", layer ? { "data-layer": layer } : {}, el("dt", {}, layer ? el("i") : null, label), el("dd", { text: value }));
    if (small) d.lastChild.append(" ", el("small", { text: small }));
    dl.append(d);
  };
  item(T.s.total, String(st.total || 0), null);
  item(T.s.p50, fmtLat(st.p50_us), null);
  for (const L of LAYERS) {
    const c = (st.by_layer || {})[L];
    if (c) item(`${T.layers[L]} · ${T.s.p50l}`, fmtLat((st.p50_by_layer_us || {})[L]), L, "×" + c);
  }
  const x = st.extra;
  if (x) {
    item(T.s.wakes, String((x.thaws || 0) + (x.restores || 0)), "von", `${x.thaws || 0} thaw · ${x.restores || 0} restore${x.wake_ms_avg ? " · " + Math.round(x.wake_ms_avg) + " ms" : ""}`);
    item(T.s.replicas, String(x.running || 0), "von", `+${x.warm || 0} frozen`);
    if (x.mem_mib) item(T.s.vonmem, x.mem_mib + " MiB", "von");
  }
  item(T.s.mem, Math.round(st.go_sys_mib || 0) + " MiB", null, `heap ${(st.go_heap_mib || 0).toFixed(1)}`);
}

function renderHistory() {
  $("history").replaceChildren(...history.map((h) =>
    el("li", {}, el("span", { class: "tx", text: h.text, title: h.text }),
      el("span", { class: "badge", "data-layer": h.layer, text: (t().layers[h.layer] || h.layer).split(" ")[0] }),
      el("span", { class: "ms", text: fmtLat(h.us) }))));
}

function onDecision(resp) {
  renderTrace(resp);
  renderState(resp.state, (resp.effects || []).flatMap((e) => e.changed || []));
  const tr = resp.trace;
  if (!history.length || history[0].stamp !== tr) {
    history.unshift({ text: tr.text, layer: tr.decided_by, us: tr.total_us, stamp: tr });
    history.length = Math.min(history.length, 30);
    renderHistory();
  }
}

// ── órdenes ───────────────────────────────────────────────────────────────
let inflight = false;
function setBusy(b) {
  inflight = b;
  $("busy").hidden = !b;
  $("send").disabled = b;
  document.querySelectorAll(".chips button").forEach((x) => (x.disabled = b));
}

async function send(text, l) {
  text = (text || "").trim();
  if (!text || inflight) return;
  $("cmd-input").value = text;
  setBusy(true);
  try {
    const r = await fetch("api/command", {
      method: "POST", headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ text, lang: l || "auto" }),
    });
    const body = await r.json().catch(() => ({}));
    if (!r.ok) throw new Error(r.status === 429 ? t().busy : body.error || r.statusText);
    suppress = body.trace;
    onDecision(body);
  } catch (e) {
    $("trace-empty").hidden = false; $("trace").hidden = true;
    $("trace-empty").textContent = `${t().error}: ${e.message}`;
  } finally {
    setBusy(false);
  }
}

// El evento SSE de la propia orden llega a la vez que la respuesta: se ignora
// el que coincide para no pintarla dos veces en el historial.
let suppress = null;

function connect() {
  const es = new EventSource("api/events");
  es.onopen = () => { $("live").classList.add("on"); $("live").textContent = t().live; };
  es.onerror = () => { $("live").classList.remove("on"); $("live").textContent = t().offline; };
  es.addEventListener("state", (ev) => renderState(JSON.parse(ev.data), []));
  es.addEventListener("stats", (ev) => renderStats(JSON.parse(ev.data)));
  es.addEventListener("decision", (ev) => {
    const d = JSON.parse(ev.data);
    if (suppress && suppress.text === d.trace.text && suppress.total_us === d.trace.total_us) { suppress = null; return; }
    if (!inflight) onDecision(d);
  });
}

async function init() {
  const r = await fetch("api/state");
  data = await r.json();
  document.querySelectorAll(".lang button").forEach((b) => b.addEventListener("click", () => setLang(b.dataset.lang)));
  $("cmd-form").addEventListener("submit", (e) => { e.preventDefault(); send($("cmd-input").value, "auto"); });
  $("reset").addEventListener("click", async () => {
    const r = await fetch("api/reset", { method: "POST", headers: { "Content-Type": "application/json" }, body: "{}" });
    if (r.ok) renderState((await r.json()).state, []);
  });
  const qs = new URLSearchParams(location.search).get("lang");
  setLang(qs || load("room.lang") || (navigator.language || "es").slice(0, 2));
  renderStats(data.stats);
  connect();
}

init();
