#!/usr/bin/env python3
"""wall.py — todas las pantallas de los teléfonos a la vez, en una web.

Corre donde corre kindling (el CT de Proxmox, o el Mac con KLING_HOST del
daemon privado) y solo usa el CLI `kling`: lista las máquinas con
`kling ps -json`, captura con `kling exec <m> -- android-sh 'screencap -p'`
y toca con `uidump tap` (o `input tap` si no hay uidump). Sin dependencias:
biblioteca estándar de Python 3.

    python3 wall.py [-port 8765] [-match '^phone-'] [-par 6]

Escucha SOLO en 127.0.0.1: tocar un teléfono es controlarlo, y adb/exec no
tienen más autenticación que llegar hasta aquí. Desde otra máquina, con un
túnel: `ssh -L 8765:127.0.0.1:8765 phones` (x86_64/wall-mac.sh lo hace).
"""
import argparse
import json
import re
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse

ap = argparse.ArgumentParser()
ap.add_argument("-port", type=int, default=8765)
ap.add_argument("-match", default="", help="regex de nombres de máquina (vacío = todas las que corren)")
ap.add_argument("-par", type=int, default=6, help="capturas simultáneas como mucho")
ap.add_argument("-kling", default="kling")
args = ap.parse_args()

NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$")
MATCH = re.compile(args.match) if args.match else None
SEM = threading.Semaphore(args.par)


def kling(*a, timeout=20, stdin=None):
    return subprocess.run([args.kling, *a], capture_output=True, timeout=timeout, input=stdin)


def phones():
    r = kling("ps", "-json", timeout=10)
    if r.returncode != 0:
        raise RuntimeError(r.stderr.decode(errors="replace").strip() or "kling ps failed")
    out = []
    for m in json.loads(r.stdout or b"[]"):
        name = m.get("name", "")
        if m.get("state") != "running" or not NAME_RE.match(name):
            continue
        if MATCH and not MATCH.search(name):
            continue
        out.append({"name": name, "ip": m.get("ip", ""), "health": _health.get(name)})
    out.sort(key=lambda p: [int(t) if t.isdigit() else t for t in re.split(r"(\d+)", p["name"])])
    return out


# Salud: una pantalla puede verse normal con Android muerto por dentro (visto
# en vz: system_server caído tras horas y la captura congelada o negra). Un
# hilo mira cada HEALTH_EVERY s que system_server vive y el arranque terminó.
HEALTH_EVERY = 15
_health = {}  # name -> {"ok": bool, "why": str, "t": float}


def _check(name):
    with SEM:
        r = kling("exec", "-timeout", "8s", name, "--", "android-sh",
                  "pidof system_server >/dev/null && getprop sys.boot_completed", timeout=12)
    out = (r.stdout or b"").decode(errors="replace").strip()
    if r.returncode == 0 and out == "1":
        return {"ok": True, "why": "", "t": time.time()}
    why = "system_server is not running" if r.returncode != 0 else "boot not completed"
    err = (r.stderr or b"").decode(errors="replace").strip()
    if "not running" in err or "Android is not running" in err:
        why = "Android is not running"
    return {"ok": False, "why": why, "t": time.time()}


def _health_loop():
    while True:
        try:
            names = [p["name"] for p in phones()]
            for n in list(_health):
                if n not in names:
                    _health.pop(n, None)
            ts = [threading.Thread(target=lambda n=n: _health.__setitem__(n, _check(n)), daemon=True) for n in names]
            for t in ts:
                t.start()
            for t in ts:
                t.join(20)
        except Exception:  # noqa: BLE001 — el siguiente ciclo lo reintenta
            pass
        time.sleep(HEALTH_EVERY)


# Una captura por teléfono a la vez: si varias pestañas piden la misma
# pantalla, esperan a la que está en curso en vez de lanzar otra.
_cache = {}  # name -> (t, png)
_locks = {}
_locks_mu = threading.Lock()


def shot(name, max_age=0.25):
    with _locks_mu:
        lk = _locks.setdefault(name, threading.Lock())
    with lk:
        c = _cache.get(name)
        if c and time.time() - c[0] < max_age:
            return c[1]
        with SEM:
            r = kling("exec", "-timeout", "15s", name, "--", "android-sh", "screencap -p", timeout=20)
        png = r.stdout
        if r.returncode != 0 or not png.startswith(b"\x89PNG"):
            raise RuntimeError((r.stderr or b"").decode(errors="replace").strip()[-200:] or "screencap failed")
        _cache[name] = (time.time(), png)
        return png


def android(name, uid, fallback):
    """uid: argumentos de `uidump` (servidor residente, ~15 ms); si no está o
    falla, fallback: comando de Android por android-sh (`input`, más lento)."""
    if uid:
        r = kling("exec", "-timeout", "10s", name, "--", "uidump", *uid, timeout=15)
        if r.returncode == 0:
            return True
    r = kling("exec", "-timeout", "10s", name, "--", "android-sh", fallback, timeout=15)
    return r.returncode == 0


KEYS = {"HOME": "KEYCODE_HOME", "BACK": "KEYCODE_BACK", "RECENTS": "KEYCODE_APP_SWITCH", "POWER": "KEYCODE_WAKEUP"}


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def send(self, code, body, ctype="application/json", extra=None):
        if isinstance(body, (dict, list)):
            body = json.dumps(body).encode()
        elif isinstance(body, str):
            body = body.encode()
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        u = urlparse(self.path)
        try:
            if u.path == "/":
                return self.send(200, PAGE, "text/html; charset=utf-8")
            if u.path == "/api/phones":
                return self.send(200, phones())
            m = re.match(r"^/shot/([^/]+)$", u.path)
            if m and NAME_RE.match(m.group(1)):
                t0 = time.time()
                png = shot(m.group(1))
                return self.send(200, png, "image/png", {"X-Capture-Ms": str(int((time.time() - t0) * 1000))})
            self.send(404, {"error": "not found"})
        except Exception as e:  # noqa: BLE001 — la página muestra el motivo
            self.send(502, {"error": str(e)})

    def do_POST(self):
        u = urlparse(self.path)
        q = {k: v[0] for k, v in parse_qs(u.query).items()}
        m = re.match(r"^/(tap|swipe|key)/([^/]+)$", u.path)
        if not m or not NAME_RE.match(m.group(2)):
            return self.send(404, {"error": "not found"})
        act, name = m.groups()
        try:
            if act == "tap":
                x, y = int(float(q["x"])), int(float(q["y"]))
                ok = android(name, ["tap", str(x), str(y)], "input tap %d %d" % (x, y))
            elif act == "swipe":
                v = [str(int(float(q[k]))) for k in ("x1", "y1", "x2", "y2")] + [str(int(float(q.get("ms", 250))))]
                ok = android(name, ["swipe", *v], "input swipe " + " ".join(v))
            else:
                code = KEYS.get(q.get("k", ""))
                if not code:
                    return self.send(400, {"error": "unknown key"})
                ok = android(name, None, "input keyevent " + code)
            _cache.pop(name, None)
            return self.send(200 if ok else 502, {"ok": ok})
        except (KeyError, ValueError):
            return self.send(400, {"error": "bad parameters"})


PAGE = r"""<!doctype html>
<html lang="es"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Muro de teléfonos</title>
<style>
:root{--bg:#f4f4f2;--card:#fff;--fg:#1b1b1b;--mut:#6b6b6b;--line:#dcdcd8;--acc:#1f6feb;--bad:#c62828}
@media (prefers-color-scheme:dark){:root{--bg:#121212;--card:#1d1d1d;--fg:#ececec;--mut:#9a9a9a;--line:#2e2e2e;--acc:#58a6ff;--bad:#ef5350}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:14px/1.4 system-ui,-apple-system,sans-serif}
header{position:sticky;top:0;z-index:2;display:flex;flex-wrap:wrap;gap:12px;align-items:center;padding:10px 16px;background:var(--bg);border-bottom:1px solid var(--line)}
header h1{font-size:16px;margin:0 8px 0 0}header label{color:var(--mut);display:flex;gap:6px;align-items:center}
input,select,button{font:inherit;color:inherit;background:var(--card);border:1px solid var(--line);border-radius:6px;padding:4px 8px}
button{cursor:pointer}#stat{margin-left:auto;color:var(--mut)}
main{display:grid;gap:12px;padding:16px;grid-template-columns:repeat(auto-fill,minmax(var(--w,200px),1fr))}
.c{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:8px;display:flex;flex-direction:column;gap:6px}
.t{display:flex;justify-content:space-between;gap:6px;font-size:12px}.t b{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.t span{color:var(--mut);font-variant-numeric:tabular-nums}
.s{position:relative;aspect-ratio:9/16;background:#000;border-radius:6px;overflow:hidden;touch-action:none;user-select:none}
.s img{width:100%;height:100%;object-fit:contain;display:block;cursor:crosshair}
.s .e{position:absolute;inset:0;display:none;align-items:center;justify-content:center;padding:8px;text-align:center;color:#fff;background:rgba(160,20,20,.75);font-size:12px}
.c.bad{border-color:var(--bad);box-shadow:0 0 0 1px var(--bad)}.t .h{color:var(--bad);font-weight:600}
.k{display:flex;gap:4px}.k button{flex:1;padding:2px 0}
.empty{grid-column:1/-1;color:var(--mut);text-align:center;padding:48px 0}
</style></head><body>
<header><h1>Muro de teléfonos</h1>
<label>Refresco <select id="iv"><option value="0">continuo</option><option value="500">0,5 s</option><option value="1000" selected>1 s</option><option value="3000">3 s</option><option value="-1">pausa</option></select></label>
<label>Tamaño <input id="sz" type="range" min="140" max="420" value="200"></label>
<label>Filtro <input id="fl" placeholder="phone-" size="10"></label>
<span id="stat">…</span></header>
<main id="g"></main>
<script>
const g=document.getElementById('g'),iv=document.getElementById('iv'),sz=document.getElementById('sz'),fl=document.getElementById('fl'),stat=document.getElementById('stat');
const cards=new Map();
function pref(k,d){try{return localStorage.getItem(k)??d}catch(e){return d}}
function save(k,v){try{localStorage.setItem(k,v)}catch(e){}}
iv.value=pref('iv','1000');sz.value=pref('sz','200');fl.value=pref('fl','');
const applySz=()=>g.style.setProperty('--w',sz.value+'px');applySz();
sz.oninput=()=>{applySz();save('sz',sz.value)};iv.onchange=()=>{save('iv',iv.value);cards.forEach(c=>c.kick())};fl.oninput=()=>{save('fl',fl.value);sync()};
async function post(u){try{await fetch(u,{method:'POST'})}catch(e){}}
function card(p){
  const el=document.createElement('div');el.className='c';
  el.innerHTML=`<div class="t"><b></b><span>…</span></div><div class="s"><img alt=""><div class="e"></div></div><div class="k"><button data-k="BACK">◀</button><button data-k="HOME">●</button><button data-k="RECENTS">■</button></div>`;
  el.querySelector('b').textContent=p.name;el.querySelector('b').title=p.ip||'';
  const img=el.querySelector('img'),lat=el.querySelector('.t span'),err=el.querySelector('.e');
  let busy=false,timer=null,alive=true,url=null;
  async function tick(){
    if(!alive||busy)return;const w=+iv.value;if(w<0)return;busy=true;const t0=performance.now();
    try{const r=await fetch('/shot/'+encodeURIComponent(p.name));if(!r.ok)throw new Error((await r.json()).error||r.status);
      const b=await r.blob();const u=URL.createObjectURL(b);img.onload=()=>{if(url)URL.revokeObjectURL(url);url=u};img.src=u;
      err.style.display='none';lat.textContent=Math.round(performance.now()-t0)+' ms';
    }catch(e){err.textContent=String(e.message||e);err.style.display='flex';lat.textContent='error'}
    busy=false;if(alive&&+iv.value>=0)timer=setTimeout(tick,+iv.value);
  }
  function kick(){clearTimeout(timer);if(!busy)tick()}
  // tocar y deslizar: coordenadas de la imagen real (naturalWidth/Height)
  let down=null;const pt=ev=>{const r=img.getBoundingClientRect(),nw=img.naturalWidth||720,nh=img.naturalHeight||1280,s=Math.min(r.width/nw,r.height/nh),ox=(r.width-nw*s)/2,oy=(r.height-nh*s)/2;
    return{x:Math.round((ev.clientX-r.left-ox)/s),y:Math.round((ev.clientY-r.top-oy)/s),nw,nh}};
  img.onpointerdown=ev=>{down={...pt(ev),t:performance.now()};ev.preventDefault()};
  img.onpointerup=async ev=>{if(!down)return;const u=pt(ev),d=down;down=null;if(u.x<0||u.y<0||u.x>u.nw||u.y>u.nh)return;
    const n=encodeURIComponent(p.name);
    if(Math.hypot(u.x-d.x,u.y-d.y)<12)await post(`/tap/${n}?x=${u.x}&y=${u.y}`);
    else await post(`/swipe/${n}?x1=${d.x}&y1=${d.y}&x2=${u.x}&y2=${u.y}&ms=${Math.round(Math.min(Math.max(performance.now()-d.t,80),1000))}`);
    kick()};
  el.querySelectorAll('.k button').forEach(b=>b.onclick=async()=>{await post(`/key/${encodeURIComponent(p.name)}?k=${b.dataset.k}`);kick()});
  function health(h){const bad=h&&h.ok===false;el.classList.toggle('bad',bad);
    let s=el.querySelector('.t .h');if(bad){if(!s){s=document.createElement('span');s.className='h';el.querySelector('.t').appendChild(s)}s.textContent='⚠ '+h.why;s.title=h.why}else if(s)s.remove()}
  tick();return{el,kick,health,stop(){alive=false;clearTimeout(timer);if(url)URL.revokeObjectURL(url)}};
}
async function sync(){
  let ps;try{ps=await (await fetch('/api/phones')).json();if(ps.error)throw new Error(ps.error)}catch(e){stat.textContent='sin conexión: '+(e.message||e);return}
  const f=fl.value.trim();if(f)ps=ps.filter(p=>p.name.includes(f));
  const want=new Set(ps.map(p=>p.name));
  for(const [n,c] of cards)if(!want.has(n)){c.stop();c.el.remove();cards.delete(n)}
  for(const p of ps)if(!cards.has(p.name)){const c=card(p);cards.set(p.name,c)}
  for(const p of ps)cards.get(p.name).health(p.health);
  const malos=ps.filter(p=>p.health&&p.health.ok===false).length;
  g.replaceChildren(...ps.map(p=>cards.get(p.name).el));
  if(!ps.length)g.innerHTML='<div class="empty">No hay teléfonos corriendo.</div>';
  stat.textContent=ps.length+' teléfono'+(ps.length==1?'':'s')+(malos?' · '+malos+' con Android caído':'');
}
sync();setInterval(sync,5000);
</script></body></html>"""

if __name__ == "__main__":
    threading.Thread(target=_health_loop, daemon=True).start()
    srv = ThreadingHTTPServer(("127.0.0.1", args.port), H)
    print("wall: http://127.0.0.1:%d/" % args.port, flush=True)
    srv.serve_forever()
