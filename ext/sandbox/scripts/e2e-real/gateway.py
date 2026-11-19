# Prueba real de la paridad con OpenSandbox contra un daemon y un gateway de
# verdad (microVMs). Pensada para un daemon privado como el de
# scripts/93-e2e-linux-ci.sh; ver README.md de esta carpeta. Variables:
#   PARIDAD_HOME  carpeta con bins/ (kling, kling-sandbox), src/ (el repo),
#                 venv/ (pip install e2b), node/ y js/ (npm i e2b@2)
#   KLING_HOST, KLING_CONFIG  el daemon de prueba (nunca uno que importe)
# Gateway: SDKs, entrada HTTP/WebSocket, consola, inquilinos, MCP, E2B, auditoría y OTLP.
import json, os, subprocess, sys, threading, time, urllib.request, http.server, socket, tempfile, traceback
H = os.path.expanduser(os.environ.get("PARIDAD_HOME", "~/paridad-e2b"))
SRC = H + "/src"
sys.path.insert(0, SRC + "/sdks/python")
BIN = H + "/bins/kling-sandbox"
ENV = dict(os.environ)
SOCK = os.environ.get("KLING_HOST", "unix:///run/kt-par/kling.sock").replace("unix://", "")
PUERTO = 18099
URL = "http://127.0.0.1:%d" % PUERTO
TOK_ADM, TOK_A = "tok-par-adm-0123456789abcdef", "tok-par-a-0123456789abcdef"
TPL = "par-tpl"
res = []
def ok(n, d=""): res.append(("ok", n, d)); print("  ok    %s %s" % (n, d), flush=True)
def bad(n, esp, obt): res.append(("FALLO", n, "")); print("  FALLO %s | esperaba: %s | obtuvo: %s" % (n, esp, str(obt)[:300]), flush=True)
def paso(n, f):
    try: f()
    except Exception as e: bad(n, "sin excepción", "%s: %s" % (type(e).__name__, e)); traceback.print_exc()

# colector OTLP falso
otlp = []
class C(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0); otlp.append((self.path, self.rfile.read(n))); self.send_response(200); self.end_headers(); self.wfile.write(b"{}")
    def log_message(self, *a): pass
col = http.server.ThreadingHTTPServer(("127.0.0.1", 0), C); threading.Thread(target=col.serve_forever, daemon=True).start()

tmp = tempfile.mkdtemp(prefix="pargw-")
json.dump({"name": TPL, "image": "toolchain", "pool": 1, "steps": [{"cmd": ["true"]}]}, open(tmp + "/tpl.json", "w"))
r = subprocess.run([BIN, "sbx", "template", "apply", "-f", tmp + "/tpl.json"], env=ENV, capture_output=True, text=True)
print("  plantilla:", (r.stdout + r.stderr).strip()[-200:], flush=True)
genv = dict(ENV, KLING_SANDBOX_TENANTS="adm:%s,ana:%s" % (TOK_ADM, TOK_A), KLING_SANDBOX_ADMINS="adm")
log = open(tmp + "/gateway.log", "w")
gw = subprocess.Popen([BIN, "sbx", "gateway", "-listen", "127.0.0.1:%d" % PUERTO, "-image", "toolchain", "-ingress-domain", "e2e.test",
                       "-state-dir", tmp + "/estado", "-audit-dir", tmp + "/audit", "-otlp-endpoint", "http://127.0.0.1:%d" % col.server_address[1],
                       "-otlp-metrics-interval", "2s"], env=genv, stdout=log, stderr=log)
for _ in range(100):
    try: urllib.request.urlopen(URL + "/v1/health", timeout=1); break
    except Exception: time.sleep(0.1)
def req(m, p, tok=TOK_A, body=None, host=None, raw=False):
    rq = urllib.request.Request(URL + p, method=m, data=(json.dumps(body).encode() if body is not None else None))
    if tok: rq.add_header("Authorization", "Bearer " + tok)
    if body is not None: rq.add_header("Content-Type", "application/json")
    if host: rq.add_header("Host", host)
    try:
        with urllib.request.urlopen(rq, timeout=30) as f: d = f.read(); return f.status, (d if raw else (json.loads(d) if d[:1] in (b"{", b"[") else d)), dict(f.headers)
    except urllib.error.HTTPError as e: return e.code, e.read(), dict(e.headers)

from kindling_sandbox import Sandbox, Client, Admin
os.environ.update(KLING_SANDBOX_URL=URL, KLING_SANDBOX_TOKEN=TOK_A)
sb = None
def sdk():
    global sb
    t0 = time.time(); sb = Sandbox.create(template=TPL, ttl=600); ok("SDK Python: create", "%s en %.0f ms" % (sb.id, (time.time() - t0) * 1000))
    r = sb.exec("echo hola; echo err >&2; exit 3"); (ok("SDK: exec") if (r.exit_code, r.stdout.strip()) == (3, "hola") else bad("SDK exec", "3 hola", (r.exit_code, r.stdout)))
    evs = list(sb.exec_stream("for i in 1 2 3; do echo $i; done")); txt = "".join(e.text or "" for e in evs if e.stream == "stdout")
    (ok("SDK: exec_stream") if txt.split() == ["1", "2", "3"] else bad("SDK exec_stream", "1 2 3", txt))
    sb.files.write("/tmp/par/a.txt", "contenido", mkdir=True); (ok("SDK: files write/read") if sb.files.read_text("/tmp/par/a.txt") == "contenido" else bad("SDK files", "contenido", sb.files.read_text("/tmp/par/a.txt")))
    names = [e.name for e in sb.files.list("/tmp/par").entries]; (ok("SDK: files.list") if "a.txt" in names else bad("files.list", "a.txt", names))
    p = sb.processes.start("for i in 1 2; do echo p$i; sleep 1; done; exit 4"); info = sb.processes.get(p.pid, wait=15)
    out = "".join(e.text or "" for e in sb.processes.output(p.pid) if getattr(e, "text", None))
    (ok("SDK: proceso de fondo", "exit %s" % info.exit_code) if info.exit_code == 4 and "p1" in out and "p2" in out else bad("SDK procesos", "exit 4 con p1 p2", (info, out)))
    s = sb.sessions.create(cwd="/tmp"); sb.sessions.exec(s.id, "mkdir -p q && cd q && export Z=9"); r = sb.sessions.exec(s.id, 'echo "$(pwd) $Z"')
    (ok("SDK: sesión") if r.stdout.strip() == "/tmp/q 9" else bad("SDK sesión", "/tmp/q 9", r.stdout))
paso("SDK Python", sdk)

def entrada():
    pid = sb.processes.start("cd /tmp/par && exec python3 -m http.server 8000 --bind 0.0.0.0").pid
    time.sleep(1.5)
    if os.environ.get("PUERTOS_A_MANO") == "1":
        import http.client
        class U(http.client.HTTPConnection):
            def connect(self):
                self.sock = socket.socket(socket.AF_UNIX); self.sock.connect(SOCK)
        nombre = json.loads(subprocess.run([H + "/bins/kling", "ps", "-json"], env=ENV, capture_output=True, text=True).stdout or "[]")
        ref = sb.id.split("/", 1)[1]
        cu = U("x"); cu.request("GET", "/machines/" + ref); m = json.loads(cu.getresponse().read())
        labels = dict(m.get("labels") or {}); labels["kling.ports"] = ",".join(sorted(set(filter(None, (labels.get("kling.ports", "") + ",8000,8001").split(",")))))
        cu = U("x"); cu.request("PUT", "/machines/%s/labels" % ref, json.dumps(labels), {"Content-Type": "application/json"}); print("  (kling.ports a mano: %s)" % cu.getresponse().status, flush=True)
    c, ep, _ = req("POST", "/v1/sandboxes/%s/endpoints" % sb.id, body={"port": 8000})
    if c != 201: return bad("endpoint", 201, (c, ep))
    host = ep["url"].split("//", 1)[1].split("/", 1)[0]; ok("endpoint creado", host)
    c, d, _ = req("GET", "/a.txt", tok=ep["token"], host=host, raw=True)
    (ok("entrada HTTP: el fichero del sandbox llega por <puerto>-<id>.e2e.test") if c == 200 and d == b"contenido" else bad("entrada HTTP", "200 contenido", (c, d[:200])))
    c, d, _ = req("GET", "/a.txt", tok=None, host=host, raw=True); (ok("entrada sin token: %d" % c) if c in (401, 403) else bad("entrada sin token", "401/403", c))
    c, d, _ = req("GET", "/a.txt", tok=TOK_A, host=host, raw=True); (ok("entrada con el token del dueño") if c == 200 else bad("entrada con token del dueño", 200, c))
    # WebSocket por la entrada: el eco de un servidor mínimo en el invitado
    ws = r'''
import socket,hashlib,base64
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(("0.0.0.0",8001));s.listen(1)
c,_=s.accept();h=c.recv(4096).decode()
k=[l.split(":",1)[1].strip() for l in h.split("\r\n") if l.lower().startswith("sec-websocket-key")][0]
a=base64.b64encode(hashlib.sha1((k+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
c.sendall(("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: "+a+"\r\n\r\n").encode())
d=c.recv(4096);c.sendall(d);c.close()
'''
    sb.files.write("/tmp/ws.py", ws); sb.processes.start("python3 /tmp/ws.py"); time.sleep(1)
    c, ep2, _ = req("POST", "/v1/sandboxes/%s/endpoints" % sb.id, body={"port": 8001}); host2 = ep2["url"].split("//", 1)[1].split("/", 1)[0]
    so = socket.create_connection(("127.0.0.1", PUERTO), timeout=10)
    so.sendall(("GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\nAuthorization: Bearer %s\r\n\r\n" % (host2, ep2["token"])).encode())
    resp = so.recv(4096)
    if b" 101 " not in resp.split(b"\r\n")[0]: return bad("WebSocket upgrade", "101", resp[:200])
    marco = b"\x81\x85\x01\x02\x03\x04" + bytes(b ^ m for b, m in zip(b"hello", b"\x01\x02\x03\x04\x01"))
    so.sendall(marco); eco = so.recv(4096); so.close()
    (ok("WebSocket por la entrada: 101 y eco de bytes") if eco == marco else bad("WebSocket eco", marco, eco))
paso("entrada", entrada)

def consola():
    c, d, h = req("GET", "/console/", tok=None, raw=True)
    (ok("consola: /console/ sirve HTML con CSP") if c == 200 and b"<html" in d.lower() and "default-src 'self'" in h.get("Content-Security-Policy", "") else bad("consola", "200 html + CSP", (c, h.get("Content-Security-Policy"), d[:100])))
paso("consola", consola)

def inquilinos():
    adm = Admin(url=URL, token=TOK_ADM); t = adm.create_tenant("bea"); k = adm.create_key(t.id if hasattr(t, "id") else "bea")
    clave = getattr(k, "key", None) or getattr(k, "secret", None)
    c, d, _ = req("GET", "/v1/sandboxes", tok=clave); (ok("inquilino del almacén: su clave entra") if c == 200 else bad("clave nueva", 200, (c, d)))
    adm.revoke_key(t.id if hasattr(t, "id") else "bea", k.id if hasattr(k, "id") else k.key_id)
    c, d, _ = req("GET", "/v1/sandboxes", tok=clave); (ok("clave revocada: %d" % c) if c == 401 else bad("clave revocada", 401, c))
    st = os.stat(tmp + "/estado/tenants.json").st_mode & 0o777; (ok("tenants.json 0600") if st == 0o600 else bad("permisos tenants.json", "0600", oct(st)))
    (ok("la clave no está en claro en el almacén") if clave and clave not in open(tmp + "/estado/tenants.json").read() else bad("almacén", "sin la clave en claro", "aparece"))
paso("inquilinos", inquilinos)

def mcp():
    p = subprocess.Popen([BIN, "sbx", "mcp", "-url", URL], env=dict(ENV, KLING_SANDBOX_TOKEN=TOK_A), stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    def rpc(i, m, params=None):
        p.stdin.write(json.dumps({"jsonrpc": "2.0", "id": i, "method": m, "params": params or {}}) + "\n"); p.stdin.flush(); return json.loads(p.stdout.readline())
    rpc(1, "initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "e2e", "version": "0"}})
    p.stdin.write(json.dumps({"jsonrpc": "2.0", "method": "notifications/initialized"}) + "\n"); p.stdin.flush()
    tools = [t["name"] for t in rpc(2, "tools/list")["result"]["tools"]]
    r = rpc(3, "tools/call", {"name": "sandbox_exec", "arguments": {"id": sb.id, "command": ["echo", "desde-mcp"]}})
    txt = json.dumps(r.get("result", r))
    (ok("MCP: %d herramientas y sandbox_exec en un sandbox real" % len(tools)) if "desde-mcp" in txt else bad("MCP sandbox_exec", "desde-mcp", txt[:300]))
    p.stdin.close(); p.wait(5)
paso("MCP", mcp)

def ts():
    js = """import { Sandbox } from '%s/sdks/typescript/src/index.js';
const sb = await Sandbox.create({ template: '%s', ttl: 300 });
const r = await sb.exec('echo ts-ok');
await sb.files.write('/tmp/t.txt', 'x1');
const f = new TextDecoder().decode(await sb.files.read('/tmp/t.txt'));
await sb.delete();
console.log(JSON.stringify({ out: r.stdout.trim(), code: r.exitCode ?? r.exit_code, f }));""" % (SRC, TPL)
    open(tmp + "/t.mjs", "w").write(js)
    r = subprocess.run([H + "/node/bin/node", tmp + "/t.mjs"], env=dict(os.environ), capture_output=True, text=True, timeout=120)
    (ok("SDK TypeScript: create, exec, files, delete") if '"out":"ts-ok"' in r.stdout and '"f":"x1"' in r.stdout else bad("SDK TS", "ts-ok y x1", (r.stdout + r.stderr)[-400:]))
paso("SDK TS", ts)

def audit():
    c, d, _ = req("GET", "/v1/audit", tok=TOK_ADM, raw=True); txt = d.decode(errors="replace")
    evs = json.loads(txt).get("events", []) if c == 200 else []
    kinds = sorted({e.get("action") for e in evs} - {None})
    (ok("auditoría: %d acciones distintas" % len(kinds), ",".join(kinds)[:160]) if c == 200 and len(kinds) >= 3 else bad("auditoría", ">=3 acciones", (c, txt[:300])))
    (ok("auditoría sin tokens") if TOK_A not in txt and TOK_ADM not in txt else bad("auditoría", "sin tokens", "aparece un token"))
paso("auditoría", audit)

def otel():
    time.sleep(3); paths = {p for p, _ in otlp}; spans = sum(b.count(b'"spanId"') for p, b in otlp if p == "/v1/traces")
    (ok("OTLP: %d spans y métricas" % spans) if "/v1/traces" in paths and "/v1/metrics" in paths and spans > 0 else bad("OTLP", "traces y metrics", paths))
    (ok("OTLP sin tokens") if all(TOK_A.encode() not in b and TOK_ADM.encode() not in b for _, b in otlp) else bad("OTLP", "sin tokens", "aparece un token"))
paso("OTLP", otel)

def e2b():
    py = H + "/venv/bin/python"
    sh = subprocess.Popen([BIN, "sbx", "e2b", "-listen", "127.0.0.1:3000", "-url", URL], env=dict(ENV, KLING_SANDBOX_TOKEN=TOK_A, KLING_E2B_KEY="clave-e2b-real", HOME=tmp), stdout=open(tmp + "/e2b.log", "w"), stderr=subprocess.STDOUT)
    time.sleep(1)
    cod = """
import os
os.environ.update(E2B_API_KEY="clave-e2b-real", E2B_API_URL="http://127.0.0.1:3000", E2B_SANDBOX_URL="http://127.0.0.1:3000")
from e2b import Sandbox
sb = Sandbox.create(template="%s", timeout=300)
r = sb.commands.run("echo e2b-real; uname -m")
sb.files.write("/tmp/e.txt", "e2b-fichero")
f = sb.files.read("/tmp/e.txt")
h = sb.commands.run("sleep 2; echo fondo", background=True)
out = h.wait()
print("RES", r.exit_code, r.stdout.split(), f, out.stdout.strip())
sb.kill()
""" % TPL
    r = subprocess.run([py, "-c", cod], capture_output=True, text=True, timeout=180)
    linea = [l for l in r.stdout.splitlines() if l.startswith("RES")]
    (ok("E2B SDK Python real contra el gateway real", linea[0]) if linea and "e2b-real" in linea[0] and "e2b-fichero" in linea[0] and "fondo" in linea[0] else bad("E2B Python real", "e2b-real, fichero y fondo", (r.stdout + r.stderr)[-500:]))
    js = """process.env.E2B_API_KEY='clave-e2b-real'; process.env.E2B_API_URL='http://127.0.0.1:3000'; process.env.E2B_SANDBOX_URL='http://127.0.0.1:3000';
const { Sandbox } = await import('e2b');
const sb = await Sandbox.create('%s', { timeoutMs: 300000 });
const r = await sb.commands.run('echo e2b-js');
await sb.files.write('/tmp/j.txt', 'js-fichero');
const f = await sb.files.read('/tmp/j.txt');
await sb.kill();
console.log('RES', r.exitCode, r.stdout.trim(), f);""" % TPL
    open(H + "/js/real-gw.mjs", "w").write(js)
    r = subprocess.run([H + "/node/bin/node", H + "/js/real-gw.mjs"], cwd=H + "/js", capture_output=True, text=True, timeout=180)
    linea = [l for l in r.stdout.splitlines() if l.startswith("RES")]
    (ok("E2B SDK JS real contra el gateway real", linea[0]) if linea and "e2b-js" in linea[0] and "js-fichero" in linea[0] else bad("E2B JS real", "e2b-js y fichero", (r.stdout + r.stderr)[-500:]))
    sh.terminate(); sh.wait(5)
paso("E2B", e2b)

try:
    if sb: sb.delete()
except Exception: pass
gw.terminate(); gw.wait(5)
subprocess.run([BIN, "sbx", "template", "rm", TPL], env=ENV, capture_output=True)
f = [r for r in res if r[0] != "ok"]
print("gateway: %d ok · %d fallo(s)  (log: %s/gateway.log)" % (len(res) - len(f), len(f), tmp))
