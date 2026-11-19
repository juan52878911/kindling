"""Servidor falso de la API /v1 que sigue ext/sandbox/api/openapi.yaml.

Solo para tests: guarda sandboxes y ficheros en memoria y deja ganchos para
provocar un 429, trozos NDJSON partidos y demás.
"""

import base64
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

TOKEN = "tok-secreto-de-prueba"


class Estado:
    def __init__(self):
        self.sandboxes = {}
        self.ficheros = {}  # (id, path) -> (bytes, modo)
        self.peticiones = []  # (metodo, ruta, query, cabeceras, cuerpo)
        self.n = 0
        self.rate_limit_en_create = 0  # cuántos POST /v1/sandboxes devolver 429
        self.trozos_exec = None  # lista de bytes a escribir tal cual en el flujo
        self.error_exec_tras_200 = False
        self.volumenes = {}
        self.lote_falla_desde = 0  # en un lote, los índices >= esto fallan (500)


class Manejador(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    # ---- utilidades
    def _json(self, code, obj, extra=None):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(b)

    def _err(self, code, msg, cod=None, extra=None):
        o = {"message": msg}
        if cod:
            o["code"] = cod
        self._json(code, o, extra)

    def _cuerpo(self):
        n = int(self.headers.get("Content-Length") or 0)
        return self.rfile.read(n) if n else b""

    def _despachar(self):
        st = self.server.estado
        u = urlsplit(self.path)
        q = {k: v[0] for k, v in parse_qs(u.query).items()}
        cuerpo = self._cuerpo()
        st.peticiones.append((self.command, u.path, q, dict(self.headers), cuerpo))
        if u.path == "/v1/health":
            return self._json(200, {"ok": True})
        if self.headers.get("Authorization") != "Bearer " + TOKEN:
            return self._err(401, "invalid token", "unauthorized",
                             {"WWW-Authenticate": 'Bearer realm="kindling-sandbox"'})
        p = u.path
        m = self.command
        if p == "/v1/sandboxes":
            if m == "GET":
                return self._json(200, list(st.sandboxes.values()),
                                  {"X-Kindling-Partial": "h2: down"})
            if m == "POST":
                if st.rate_limit_en_create > 0:
                    st.rate_limit_en_create -= 1
                    return self._err(429, "sandbox quota exceeded", None, {"Retry-After": "7"})
                req = json.loads(cuerpo or b"{}")
                if "count" in req:
                    return self._lote(req)
                st.n += 1
                sid = "h1/m-%d" % st.n
                sb = {"id": sid, "host": "h1", "template": req.get("template", ""),
                      "state": "running", "ttl_seconds": req.get("ttl_seconds", 600),
                      "labels": req.get("labels") or {}, "campo_nuevo": 1}
                st.sandboxes[sid] = sb
                return self._json(201, sb, {"Location": "/v1/sandboxes/" + sid})
        if p == "/v1/volumes" or p.startswith("/v1/volumes/"):
            return self._volumenes(m, p[len("/v1/volumes"):].strip("/"), cuerpo)
        if p.startswith("/v1/sandboxes/"):
            resto = p[len("/v1/sandboxes/"):]
            for suf in ("/renew", "/exec", "/files"):
                if resto.endswith(suf):
                    sid, accion = resto[:-len(suf)], suf[1:]
                    break
            else:
                sid, accion = resto, ""
            if sid not in st.sandboxes:
                return self._err(404, "sandbox %s not found" % sid, "not_found")
            sb = st.sandboxes[sid]
            if accion == "" and m == "GET":
                return self._json(200, sb)
            if accion == "" and m == "DELETE":
                del st.sandboxes[sid]
                self.send_response(204)
                self.end_headers()
                return
            if accion == "renew" and m == "POST":
                req = json.loads(cuerpo) if cuerpo else {}
                if req.get("ttl_seconds", 1) < 0:
                    return self._err(400, "bad ttl", "bad_request")
                sb["ttl_seconds"] = req.get("ttl_seconds", 600)
                return self._json(200, sb)
            if accion == "exec" and m == "POST":
                return self._exec(q, json.loads(cuerpo))
            if accion == "files":
                return self._files(m, sid, q, cuerpo)
        self._err(404, "no route", "not_found")

    def _lote(self, req):
        st = self.server.estado
        ok, errores = [], []
        for i in range(req["count"]):
            if st.lote_falla_desde and i >= st.lote_falla_desde:
                errores.append({"index": i, "status": 500, "error": "boom"})
                continue
            st.n += 1
            sid = "h1/m-%d" % st.n
            st.sandboxes[sid] = {"id": sid, "host": "h1", "state": "running"}
            ok.append(st.sandboxes[sid])
        if req.get("atomic") and errores:
            for sb in ok:
                del st.sandboxes[sb["id"]]
            return self._err(500, "boom", None)
        out = {"sandboxes": ok}
        if errores:
            out["errors"] = errores
        return self._json(207 if errores else 201, out)

    def _volumenes(self, m, nombre, cuerpo):
        st = self.server.estado
        if not nombre and m == "GET":
            return self._json(200, list(st.volumenes.values()))
        if not nombre and m == "POST":
            req = json.loads(cuerpo)
            if req["name"] in st.volumenes:
                return self._err(409, "exists", None)
            v = {"name": req["name"], "host": req.get("host", "h1"),
                 "size_mib": req.get("size_mib", 64), "used_bytes": 0, "in_use": False}
            st.volumenes[v["name"]] = v
            return self._json(201, v)
        if nombre not in st.volumenes:
            return self._err(404, "no volume %r" % nombre, "not_found")
        if m == "GET":
            return self._json(200, st.volumenes[nombre])
        if m == "DELETE":
            del st.volumenes[nombre]
            self.send_response(204)
            self.end_headers()
            return
        self._err(405, "no", None)

    def _exec(self, q, req):
        st = self.server.estado
        if not req.get("cmd"):
            return self._err(400, "missing cmd", "bad_request")
        out = ("eco:" + " ".join(req["cmd"]) + "\n").encode()
        if "stdin" in req:
            out += base64.b64decode(req["stdin"])
        if q.get("wait") == "1":
            return self._json(200, {"exit_code": 3, "stdout": base64.b64encode(out).decode(),
                                    "stderr": base64.b64encode(b"err\n").decode(),
                                    "duration_ms": 5, "truncated": False, "timed_out": False})
        if st.trozos_exec is None:
            lineas = [{"stream": "stdout", "data": base64.b64encode(out).decode()},
                      {"stream": "stderr", "data": base64.b64encode(b"err\n").decode()},
                      {"exit": 3, "duration_ms": 5}]
            if st.error_exec_tras_200:
                lineas[-1] = {"error": "flujo cortado"}
            trozos = [(json.dumps(l) + "\n").encode() for l in lineas]
        else:
            trozos = st.trozos_exec
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for t in trozos:
            self.wfile.write(b"%x\r\n%s\r\n" % (len(t), t))
            self.wfile.flush()
            time.sleep(0.02)
        self.wfile.write(b"0\r\n\r\n")

    def _files(self, m, sid, q, cuerpo):
        st = self.server.estado
        path = q.get("path")
        if not path:
            return self._err(400, "missing path", "bad_request")
        clave = (sid, path)
        if m == "PUT":
            modo = q.get("mode", "0644")
            st.ficheros[clave] = (cuerpo, modo)
            return self._json(200, {"path": path, "size": len(cuerpo), "mode": modo, "is_dir": False})
        if clave not in st.ficheros:
            return self._err(404, "no such file", "not_found")
        datos, modo = st.ficheros[clave]
        if m == "GET" and q.get("stat") == "1":
            return self._json(200, {"path": path, "size": len(datos), "mode": modo, "is_dir": False})
        if m == "GET":
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(datos)))
            self.end_headers()
            self.wfile.write(datos)
            return
        if m == "DELETE":
            del st.ficheros[clave]
            self.send_response(204)
            self.end_headers()
            return

    do_GET = do_POST = do_PUT = do_DELETE = _despachar


class ServidorFalso:
    def __init__(self):
        self.estado = Estado()
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Manejador)
        self.httpd.estado = self.estado
        self.httpd.daemon_threads = True
        self.url = "http://127.0.0.1:%d" % self.httpd.server_address[1]
        self._t = threading.Thread(target=lambda: self.httpd.serve_forever(poll_interval=0.01), daemon=True)

    def __enter__(self):
        self._t.start()
        return self

    def __exit__(self, *a):
        self.httpd.shutdown()
        self.httpd.server_close()
