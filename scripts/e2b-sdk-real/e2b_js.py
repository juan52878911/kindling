# Prueba el shim con el SDK real de E2B (Python): SDK -> kling sbx e2b -> gateway falso.
import os, sys, subprocess, time, socket, traceback
AQUI = os.path.dirname(os.path.abspath(__file__))
RAIZ = os.path.abspath(os.path.join(AQUI, "../.."))
sys.path.insert(0, os.path.join(RAIZ, "sdks/python/tests"))
import servidor_falso
from servidor_falso import ServidorFalso, TOKEN
from urllib.parse import unquote
_orig = servidor_falso.Manejador._despachar
def _decod(self):
    # Como el frontal real (Go decodifica %2F en URL.Path); solo en este arnés.
    self.path = unquote(self.path.split("?",1)[0]) + (("?"+self.path.split("?",1)[1]) if "?" in self.path else "")
    return _orig(self)
servidor_falso.Manejador.protocol_version = "HTTP/1.1"
_orig_leer = servidor_falso.Manejador._cuerpo
def _leer_chunked(self):
    if "chunked" in (self.headers.get("Transfer-Encoding") or "").lower():
        out = b""
        while True:
            n = int(self.rfile.readline().split(b";")[0].strip() or b"0", 16)
            if n == 0:
                self.rfile.readline(); return out
            out += self.rfile.read(n); self.rfile.readline()
    return _orig_leer(self)
servidor_falso.Manejador._cuerpo = _leer_chunked
_orig_files = servidor_falso.Manejador._files
def _files_log(self, m, sid, q, cuerpo):
    return _orig_files(self, m, sid, q, cuerpo)
servidor_falso.Manejador._files = _files_log
servidor_falso.Manejador.do_GET = servidor_falso.Manejador.do_POST = servidor_falso.Manejador.do_PUT = servidor_falso.Manejador.do_DELETE = _decod

def espera(port):
    for _ in range(100):
        try: socket.create_connection(("127.0.0.1", port), 0.2).close(); return
        except OSError: time.sleep(0.1)
    raise SystemExit("shim no arrancó en :%d" % port)

with ServidorFalso() as gw:
    env = dict(os.environ, KLING_SANDBOX_URL=gw.url, KLING_SANDBOX_TOKEN=TOKEN, KLING_E2B_KEY="clave-e2b-prueba", HOME=os.path.join(AQUI, ".home"))
    os.makedirs(env["HOME"], exist_ok=True)
    log = open(os.path.join(AQUI, "shim.log"), "w")
    shim = subprocess.Popen([os.environ.get("KLING_SANDBOX_BIN", os.path.join(AQUI, "kling-sandbox")), "sbx", "e2b", "-listen", "127.0.0.1:3000", "-envd-listen", "127.0.0.1:49983"], env=env, stdout=log, stderr=log)
    try:
        espera(3000); espera(49983)
        modo=os.environ.get("MODO","url")
        if modo=="debug": os.environ.update(E2B_DEBUG="true")
        else: os.environ.update(E2B_SANDBOX_URL="http://127.0.0.1:3000")
        os.environ.update(E2B_API_KEY="clave-e2b-prueba", E2B_API_URL="http://127.0.0.1:3000")
        os.environ["PATH"]=os.environ["PATH"]
        r=subprocess.run(["node","e2b_real.mjs"],cwd=os.path.join(AQUI, "js"),capture_output=True,text=True,timeout=120)
        print(r.stdout); print(r.stderr[-1500:], file=sys.stderr)
    finally:
        shim.terminate(); shim.wait(5)
