"""Servidor falso de la API /v1 que sigue ext/sandbox/api/openapi.yaml.

Solo para tests: guarda sandboxes y ficheros en memoria y deja ganchos para
provocar un 429, trozos NDJSON partidos y demás.
"""

import base64
import json
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

TOKEN = "tok-secreto-de-prueba"
TOKEN_USUARIO = "tok-sin-admin-de-prueba"


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
        self.procesos = {}  # (id, pid) -> dict
        self.salida_proc = b"hola\nmundo\n"
        self.sesiones = {}  # (id, sid) -> dict
        self.tenants = {}  # nombre -> dict (con "keys": [ficha])
        self.claves = {}  # texto en claro -> (tenant, id)
        self.env_de_creacion = None
        self.codigo_pedido = None
        self.interrumpidos = []
        self.fallos_503 = 0  # cuántas peticiones a /renew devolver 503 con Retry-After
        self.endpoints = {}  # (id, puerto) -> dict
        self.contextos = {}  # (id, cid) -> dict
        self.fork_cuota = 0  # con >0, un fork de más copias da 429
        self.lote_falla_desde = 0  # en un lote, los índices >= esto fallan (500)
        self.conexiones = 0  # conexiones TCP aceptadas


class Manejador(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def setup(self):
        super().setup()
        with self.server.cerrojo_conexiones:
            self.server.estado.conexiones += 1

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
        if self.headers.get("Authorization") not in ("Bearer " + TOKEN, "Bearer " + TOKEN_USUARIO):
            return self._err(401, "invalid token", "unauthorized",
                             {"WWW-Authenticate": 'Bearer realm="kindling-sandbox"'})
        p = u.path
        m = self.command
        if p == "/v1/tenants" or p.startswith("/v1/tenants/"):
            if self.headers.get("Authorization") != "Bearer " + TOKEN:
                return self._err(403, "managing tenants and keys needs an admin token", "forbidden")
            return self._tenants(m, p[len("/v1/tenants"):].strip("/"), cuerpo)
        if p == "/v1/sandboxes":
            if m == "GET":
                lista = sorted(st.sandboxes.values(), key=lambda x: x["id"])
                if q.get("state"):
                    lista = [x for x in lista if x["state"] == q["state"]]
                for par in parse_qs(u.query).get("label", []):
                    k, _, v = par.partition("=")
                    lista = [x for x in lista if x["labels"].get(k) == v]
                extra = {"X-Kindling-Partial": "h2: down"}
                if q.get("cursor"):
                    lista = [x for x in lista if x["id"] > q["cursor"]]
                if q.get("limit") and len(lista) > int(q["limit"]):
                    lista = lista[:int(q["limit"])]
                    extra["X-Kindling-Next-Cursor"] = lista[-1]["id"]
                return self._json(200, lista, extra)
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
                st.env_de_creacion = req.get("env")
                st.sandboxes[sid] = sb
                return self._json(201, sb, {"Location": "/v1/sandboxes/" + sid})
        if p == "/v1/volumes" or p.startswith("/v1/volumes/"):
            return self._volumenes(m, p[len("/v1/volumes"):].strip("/"), cuerpo)
        if p.startswith("/v1/sandboxes/"):
            resto = p[len("/v1/sandboxes/"):]
            mp = re.match(r"^(.+?)/(processes|sessions)((?:/.*)?)$", resto)
            if mp:
                if mp.group(1) not in st.sandboxes:
                    return self._err(404, "sandbox %s not found" % mp.group(1), "not_found")
                if mp.group(2) == "processes":
                    return self._procesos(m, mp.group(1), mp.group(3).strip("/"), q, cuerpo)
                return self._sesiones(m, mp.group(1), mp.group(3).strip("/"), cuerpo)
            re_ep = re.match(r"^(.+?)/endpoints/(\d+)$", resto)
            re_cx = re.match(r"^(.+?)/code/contexts/([0-9a-f]+)(/execution)?$", resto)
            if re_ep or re_cx:
                return self._anidados(m, re_ep, re_cx)
            for suf in ("/renew", "/exec", "/files", "/pause", "/resume", "/egress", "/endpoints", "/code",
                        "/files/move", "/files/mkdir", "/files/search", "/files/chmod", "/files/replace",
                        "/code/contexts", "/metadata", "/fork", "/metrics"):
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
                if st.fallos_503 > 0:
                    st.fallos_503 -= 1
                    return self._err(503, "busy", "service_unavailable", {"Retry-After": "2"})
                req = json.loads(cuerpo) if cuerpo else {}
                if req.get("ttl_seconds", 1) < 0:
                    return self._err(400, "bad ttl", "bad_request")
                sb["ttl_seconds"] = req.get("ttl_seconds", 600)
                return self._json(200, sb)
            if accion == "exec" and m == "POST":
                return self._exec(q, json.loads(cuerpo))
            if accion in ("pause", "resume") and m == "POST":
                sb["state"] = "paused" if accion == "pause" else "running"
                return self._json(200, sb)
            if accion == "egress" and m == "PATCH":
                req = json.loads(cuerpo)
                sb["egress"] = req.get("egress", sb.get("egress", ""))
                return self._json(200, {"egress": sb["egress"], "allow_domains": req.get("allow_domains") or [],
                                        "applied": "live", "narrowed": False, "connections_closed": False})
            if accion == "endpoints":
                return self._endpoints(m, sid, json.loads(cuerpo or b"{}"))
            if accion == "code" and m == "POST":
                return self._codigo(sid, json.loads(cuerpo))
            if accion == "code/contexts":
                return self._contextos(m, sid, json.loads(cuerpo or b"{}"))
            if accion == "metadata" and m == "PATCH":
                req = json.loads(cuerpo)
                for k in req.get("remove") or []:
                    sb["labels"].pop(k, None)
                for k, v in (req.get("labels") or {}).items():
                    if k in ("tenant", "kind") or k.startswith("kling."):
                        return self._err(400, "label %r is reserved" % k, "bad_request")
                    sb["labels"][k] = v
                return self._json(200, sb)
            if accion == "fork" and m == "POST":
                req = json.loads(cuerpo or b"{}")
                if st.fork_cuota and req.get("count", 1) > st.fork_cuota:
                    return self._err(429, "sandbox quota exceeded", "too_many_requests", {"Retry-After": "7"})
                copias = []
                for _ in range(max(req.get("count", 1), 1)):
                    st.n += 1
                    c = dict(sb, id="h1/m-%d" % st.n, ttl_seconds=req.get("ttl_seconds", sb["ttl_seconds"]))
                    c["labels"] = dict(sb["labels"])
                    st.sandboxes[c["id"]] = c
                    copias.append(c)
                return self._json(201, {"sandboxes": copias})
            if accion == "metrics" and m == "GET":
                return self._json(200, {"id": sid, "state": sb["state"], "memory_mib": 123,
                                        "memory_kind": "pss", "cpu_seconds": 4.5, "mem_limit_mib": 512})
            if accion in ("files/move", "files/mkdir", "files/search", "files/chmod", "files/replace") or accion == "files":
                return self._files(m, sid, q, cuerpo, accion)
        self._err(404, "no route", "not_found")

    def _endpoints(self, m, sid, req):
        st = self.server.estado
        if m == "POST":
            ep = {"url": "https://%d-x.example.test" % req["port"], "port": req["port"],
                  "public": bool(req.get("public")), "expires_at": "2026-01-01T00:00:00Z"}
            if not ep["public"]:
                ep["token"] = "tok-ep"
            st.endpoints[(sid, req["port"])] = ep
            return self._json(201, ep)
        return self._json(200, [v for (s2, _), v in sorted(st.endpoints.items()) if s2 == sid])

    def _anidados(self, m, re_ep, re_cx):
        st = self.server.estado
        if re_ep:
            clave = (re_ep.group(1), int(re_ep.group(2)))
            if m != "DELETE" or clave not in st.endpoints:
                return self._err(404, "no such endpoint", "not_found")
            del st.endpoints[clave]
            self.send_response(204)
            self.end_headers()
            return
        clave = (re_cx.group(1), re_cx.group(2))
        if clave not in st.contextos:
            return self._err(404, "no such context", "not_found")
        if re_cx.group(3):
            st.interrumpidos.append(clave)
            self.send_response(204)
            self.end_headers()
            return
        if m == "GET":
            return self._json(200, st.contextos[clave])
        del st.contextos[clave]
        self.send_response(204)
        self.end_headers()

    def _contextos(self, m, sid, req):
        st = self.server.estado
        if m == "POST":
            cid = "%012x" % (len(st.contextos) + 1)
            c = {"id": cid, "language": req.get("language") or "python", "pid": 100 + len(st.contextos),
                 "cwd": req.get("cwd", ""), "started_at": "2026-01-01T00:00:00Z"}
            st.contextos[(sid, cid)] = c
            return self._json(201, c)
        return self._json(200, [v for (s2, _), v in sorted(st.contextos.items()) if s2 == sid])

    def _codigo(self, sid, req):
        """Flujo NDJSON que acaba siempre en done; 'fallo' en el código = evento error."""
        evs = [{"type": "stdout", "text": "hola\n"}]
        ok = True
        if "fallo" in req["code"]:
            evs.append({"type": "error", "ename": "NameError", "evalue": "fallo", "traceback": ["t1"]})
            ok = False
        else:
            evs.append({"type": "result", "text": "2", "results": {"text/plain": "2"}, "execution_count": 1})
        evs.append({"type": "done", "ok": ok, "duration_ms": 3, "execution_count": 1})
        self.server.estado.codigo_pedido = req
        cuerpo = "".join(json.dumps(e) + "\n" for e in evs).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/x-ndjson")
        self.send_header("Content-Length", str(len(cuerpo)))
        self.end_headers()
        self.wfile.write(cuerpo)

    def _procesos(self, m, sid, resto, q, cuerpo):
        st = self.server.estado
        if not resto:
            if m == "POST":
                req = json.loads(cuerpo or b"{}")
                if not req.get("cmd"):
                    return self._err(400, "cmd is required", "bad_request")
                pid = len([k for k in st.procesos if k[0] == sid]) + 1
                p = {"pid": pid, "cmd": req["cmd"], "state": "running", "output_bytes": 0,
                     "started_at": "2026-01-01T00:00:00Z", "session": req.get("session", "")}
                st.procesos[(sid, pid)] = p
                return self._json(201, p)
            return self._json(200, [v for k, v in st.procesos.items() if k[0] == sid])
        partes = resto.split("/")
        clave = (sid, int(partes[0]))
        if clave not in st.procesos:
            return self._err(404, "no such process", "not_found")
        p = st.procesos[clave]
        sub = partes[1] if len(partes) > 1 else ""
        if sub == "" and m == "GET":
            if q.get("wait"):
                p["state"], p["exit_code"] = "exited", 0
            return self._json(200, p)
        if sub == "" and m == "DELETE":
            del st.procesos[clave]
            self.send_response(204)
            self.end_headers()
            return
        if sub == "signal" and m == "POST":
            req = json.loads(cuerpo or b"{}")
            p["state"], p["signal"] = "exited", req.get("signal") or "TERM"
            return self._json(200, p)
        if sub == "output" and m == "GET":
            off = int(q.get("offset", 0))
            datos = st.salida_proc[off:]
            lineas = [{"stream": "stdout", "data": base64.b64encode(datos[:6]).decode(),
                       "offset": off + len(datos[:6])}] if datos[:6] else []
            if len(datos) > 6:
                lineas.append({"stream": "stdout", "data": base64.b64encode(datos[6:]).decode(),
                               "offset": off + len(datos)})
            fin = {"offset": len(st.salida_proc)}
            if q.get("follow") == "1":
                fin["exit"] = 0
            lineas.append(fin)
            self.send_response(200)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            for l in lineas:
                t = (json.dumps(l) + "\n").encode()
                self.wfile.write(b"%x\r\n%s\r\n" % (len(t), t))
                self.wfile.flush()
            self.wfile.write(b"0\r\n\r\n")
            return
        self._err(405, "no", None)

    def _sesiones(self, m, sid, resto, cuerpo):
        st = self.server.estado
        if not resto and m == "POST":
            req = json.loads(cuerpo or b"{}")
            n = len([k for k in st.sesiones if k[0] == sid]) + 1
            se = {"id": req.get("id") or "s%d" % n, "dir": req.get("dir", ""),
                  "env": req.get("env", []), "execs": 0}
            st.sesiones[(sid, se["id"])] = se
            return self._json(201, se)
        if not resto and m == "GET":
            return self._json(200, [v for k, v in st.sesiones.items() if k[0] == sid])
        if (sid, resto) not in st.sesiones:
            return self._err(404, "no such session", "not_found")
        if m == "GET":
            return self._json(200, st.sesiones[(sid, resto)])
        if m == "DELETE":
            del st.sesiones[(sid, resto)]
            self.send_response(204)
            self.end_headers()
            return
        self._err(405, "no", None)

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

    def _tenants(self, m, resto, cuerpo):
        st = self.server.estado
        partes = resto.split("/") if resto else []
        req = json.loads(cuerpo) if cuerpo else {}
        if not partes:
            if m == "GET":
                return self._json(200, {"tenants": list(st.tenants.values())})
            if m == "POST":
                if req.get("name") in st.tenants:
                    return self._err(409, "tenant already exists", "conflict")
                t = {"id": req["name"], "name": req["name"], "source": "store", "read_only": False,
                     "admin": req.get("admin", False), "limits": req.get("limits") or {},
                     "created": "2026-01-01T00:00:00Z", "keys": [], "campo_nuevo": 1}
                st.tenants[t["id"]] = t
                return self._json(201, t)
        t = st.tenants.get(partes[0]) if partes else None
        if t is None:
            return self._err(404, "tenant not found", "not_found")
        if len(partes) == 1:
            if m == "GET":
                return self._json(200, t)
            if m == "PATCH":
                if "admin" in req:
                    t["admin"] = req["admin"]
                if "limits" in req:
                    t["limits"] = req["limits"]
                return self._json(200, t)
            if m == "DELETE":
                del st.tenants[partes[0]]
                self.send_response(204)
                self.end_headers()
                return
        if len(partes) >= 2 and partes[1] == "keys":
            if len(partes) == 2 and m == "POST":
                return self._clave_nueva(t, req.get("expires_at"))
            kid = partes[2] if len(partes) > 2 else ""
            ficha = next((k for k in t["keys"] if k["id"] == kid), None)
            if len(partes) > 2 and ficha is None:
                return self._err(404, "key not found", "not_found")
            if len(partes) == 3 and m == "DELETE":
                ficha.setdefault("revoked", "2026-01-01T00:00:00Z")
                self.send_response(204)
                self.end_headers()
                return
            if len(partes) == 4 and partes[3] == "rotate" and m == "POST":
                gracia = req.get("grace_minutes", 10)
                if gracia < 0 or gracia > 1440:
                    return self._err(400, "grace period out of range", "bad_request")
                ficha["expires"] = "en %d min" % gracia
                return self._clave_nueva(t, None, vieja=ficha)
        self._err(404, "no route", "not_found")

    def _clave_nueva(self, t, caduca, vieja=None):
        st = self.server.estado
        kid = "%012x" % (len(st.claves) + 1)
        texto = "ksk_%s_secreto%d" % (kid, len(st.claves) + 1)
        ficha = {"id": kid, "prefix": "ksk_" + kid, "created": "2026-01-01T00:00:00Z"}
        if caduca:
            ficha["expires"] = caduca
        t["keys"].append(ficha)
        st.claves[texto] = (t["id"], kid)
        r = dict(ficha, key=texto)
        if vieja is not None:
            r["old_key_id"], r["old_key_expires"] = vieja["id"], vieja["expires"]
        return self._json(201, r)

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

    def _files(self, m, sid, q, cuerpo, accion="files"):
        st = self.server.estado
        if accion != "files":
            return self._files_extra(m, sid, q, cuerpo, accion)
        path = q.get("path")
        if not path:
            return self._err(400, "missing path", "bad_request")
        clave = (sid, path)
        if m == "PUT" and q.get("tar") == "1":
            import io
            import tarfile
            n = 0
            with tarfile.open(fileobj=io.BytesIO(cuerpo)) as tf:
                for ent in tf:
                    if ent.isfile():
                        st.ficheros[(sid, path.rstrip("/") + "/" + ent.name)] = (tf.extractfile(ent).read(), "0644")
                    n += 1
            return self._json(200, {"entries": n, "files": n, "bytes": len(cuerpo)})
        if m == "GET" and q.get("tar") == "1":
            import io
            import tarfile
            buf = io.BytesIO()
            pref = path.rstrip("/") + "/"
            with tarfile.open(fileobj=buf, mode="w") as tf:
                for (s2, p2), (d, _) in sorted(st.ficheros.items()):
                    if s2 == sid and p2.startswith(pref):
                        ti = tarfile.TarInfo(p2[len(pref):])
                        ti.size = len(d)
                        tf.addfile(ti, io.BytesIO(d))
            datos = buf.getvalue()
            self.send_response(200)
            self.send_header("Content-Type", "application/x-tar")
            self.send_header("Content-Length", str(len(datos)))
            self.end_headers()
            self.wfile.write(datos)
            return
        if m == "PUT":
            modo = q.get("mode", "0644")
            st.ficheros[clave] = (cuerpo, modo)
            return self._json(200, {"path": path, "size": len(cuerpo), "mode": modo, "is_dir": False})
        if m == "GET" and q.get("list") == "1":
            pref = path.rstrip("/") + "/"
            ents = [{"name": p[len(pref):], "type": "file", "size": len(v[0]), "mode": v[1],
                     "mod_time": "2026-01-01T00:00:00Z"}
                    for (s2, p), v in sorted(st.ficheros.items())
                    if s2 == sid and p.startswith(pref) and "/" not in p[len(pref):]]
            return self._json(200, {"path": path, "entries": ents, "nuevo": 1})
        if clave not in st.ficheros:
            return self._err(404, "no such file", "not_found")
        datos, modo = st.ficheros[clave]
        if m == "GET" and q.get("stat") == "1":
            return self._json(200, {"path": path, "size": len(datos), "mode": modo, "is_dir": False})
        if m == "GET":
            rango = self.headers.get("Range")
            estado, extra = 200, None
            if rango:
                a, _, b = rango[len("bytes="):].partition("-")
                if a == "":
                    ini, fin = max(len(datos) - int(b), 0), len(datos) - 1
                else:
                    ini, fin = int(a), min(int(b) if b else len(datos) - 1, len(datos) - 1)
                if ini >= len(datos):
                    return self._err(416, "range outside the file", "requested_range_not_satisfiable",
                                     {"Content-Range": "bytes */%d" % len(datos)})
                estado, extra = 206, "bytes %d-%d/%d" % (ini, fin, len(datos))
                datos = datos[ini:fin + 1]
            self.send_response(estado)
            self.send_header("Content-Type", "application/octet-stream")
            if extra:
                self.send_header("Content-Range", extra)
            self.send_header("Content-Length", str(len(datos)))
            self.end_headers()
            self.wfile.write(datos)
            return
        if m == "DELETE":
            del st.ficheros[clave]
            self.send_response(204)
            self.end_headers()
            return

    def _files_extra(self, m, sid, q, cuerpo, accion):
        st = self.server.estado
        if accion == "files/move" and m == "POST":
            req = json.loads(cuerpo)
            o, d = (sid, req["from"]), (sid, req["to"])
            if o not in st.ficheros:
                return self._err(404, "move failed: No such file", "not_found")
            if d in st.ficheros and not req.get("overwrite"):
                return self._err(409, "already exists", "conflict")
            st.ficheros[d] = st.ficheros.pop(o)
            return self._json(200, req)
        if accion == "files/chmod" and m == "POST":
            req = json.loads(cuerpo)
            clave = (sid, req["path"])
            if clave not in st.ficheros:
                return self._err(404, "chmod failed: No such file", "not_found")
            if not re.match(r"^[0-7]{3,4}$", req["mode"]):
                return self._err(400, "mode must be octal", "bad_request")
            datos, _ = st.ficheros[clave]
            st.ficheros[clave] = (datos, "0" + req["mode"].lstrip("0") if len(req["mode"]) == 3 else req["mode"])
            return self._json(200, {"path": req["path"], "size": len(datos), "mode": st.ficheros[clave][1]})
        if accion == "files/replace" and m == "POST":
            req = json.loads(cuerpo)
            clave = (sid, req["path"])
            if clave not in st.ficheros:
                return self._err(404, "no such file", "not_found")
            datos, modo = st.ficheros[clave]
            texto = datos.decode()
            n = texto.count(req["old"])
            if n == 0 or (n > 1 and not req.get("all")):
                return self._err(409, "old appears %d times" % n, "conflict")
            nuevo = texto.replace(req["old"], req["new"])
            st.ficheros[clave] = (nuevo.encode(), modo)
            return self._json(200, {"path": req["path"], "replacements": n, "size": len(nuevo)})
        if accion == "files/mkdir" and m == "POST":
            req = json.loads(cuerpo)
            return self._json(201, {"path": req["path"], "size": 0, "mode": "0755", "is_dir": True})
        if accion == "files/search" and m == "GET":
            import fnmatch
            hits = [p for (s2, p), _ in sorted(st.ficheros.items())
                    if s2 == sid and p.startswith(q["path"]) and fnmatch.fnmatch(p.rsplit("/", 1)[-1], q["glob"])]
            return self._json(200, {"path": q["path"], "glob": q["glob"], "matches": hits, "truncated": False})
        return self._err(405, "method not allowed", "method_not_allowed")

    do_GET = do_POST = do_PUT = do_DELETE = do_PATCH = _despachar


class ServidorFalso:
    def __init__(self):
        self.estado = Estado()
        self.httpd = ThreadingHTTPServer(("127.0.0.1", 0), Manejador)
        self.httpd.estado = self.estado
        self.httpd.cerrojo_conexiones = threading.Lock()
        self.httpd.daemon_threads = True
        self.url = "http://127.0.0.1:%d" % self.httpd.server_address[1]
        self._t = threading.Thread(target=lambda: self.httpd.serve_forever(poll_interval=0.01), daemon=True)

    def __enter__(self):
        self._t.start()
        return self

    def __exit__(self, *a):
        self.httpd.shutdown()
        self.httpd.server_close()
