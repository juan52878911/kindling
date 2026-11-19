"""Transporte HTTP y rutas de la API /v1 (solo urllib.parse + http.client)."""

import http.client
import json
import os
import socket
import ssl
from email.utils import parsedate_to_datetime
from typing import Any, Dict, Iterator, List, Optional
from urllib.parse import quote, urlencode, urlsplit

from .errors import ConfigError, TransportError, error_para
from .models import ExecEvent, ExecResult, FileStat, Graph, SandboxInfo, Volume

ENV_URL = "KLING_SANDBOX_URL"
ENV_TOKEN = "KLING_SANDBOX_TOKEN"
URL_POR_DEFECTO = "http://localhost:8088"


def _retry_after(valor: Optional[str]) -> Optional[float]:
    if not valor:
        return None
    try:
        return max(0.0, float(valor))
    except ValueError:
        pass
    try:  # forma de fecha HTTP
        from datetime import datetime, timezone
        d = parsedate_to_datetime(valor)
        if d.tzinfo is None:
            d = d.replace(tzinfo=timezone.utc)
        return max(0.0, (d - datetime.now(timezone.utc)).total_seconds())
    except (TypeError, ValueError):
        return None


class Client:
    """Conexión al gateway. Una conexión TCP por petición (simple y sin estado)."""

    def __init__(self, url: Optional[str] = None, token: Optional[str] = None,
                 timeout: Optional[float] = 60.0):
        url = url or os.environ.get(ENV_URL) or URL_POR_DEFECTO
        partes = urlsplit(url)
        if partes.scheme not in ("http", "https") or not partes.hostname:
            raise ConfigError("URL del gateway inválida: %r" % url)
        self._https = partes.scheme == "https"
        self._host = partes.hostname
        self._port = partes.port
        self._prefijo = partes.path.rstrip("/")
        self.url = "%s://%s%s" % (partes.scheme, partes.netloc.rpartition("@")[2], self._prefijo)
        self._token = token if token is not None else os.environ.get(ENV_TOKEN)
        self.timeout = timeout

    def __repr__(self) -> str:
        return "Client(url=%r, token=%s)" % (self.url, "<oculto>" if self._token else "None")

    __str__ = __repr__

    # ---- bajo nivel

    def _conectar(self, timeout):
        if self._https:
            return http.client.HTTPSConnection(self._host, self._port, timeout=timeout,
                                               context=ssl.create_default_context())
        return http.client.HTTPConnection(self._host, self._port, timeout=timeout)

    def _limpia(self, texto: str) -> str:
        if self._token and self._token in texto:
            texto = texto.replace(self._token, "<oculto>")
        return texto

    def _abrir(self, metodo: str, ruta: str, query: Optional[Dict[str, str]] = None,
               cuerpo: Optional[bytes] = None, content_type: Optional[str] = None,
               timeout: Any = "defecto", auth: bool = True):
        """Devuelve (conn, resp) con el estado ya validado: si no es 2xx, lanza."""
        if timeout == "defecto":
            timeout = self.timeout
        destino = self._prefijo + quote(ruta, safe="/")
        if query:
            destino += "?" + urlencode(query)
        cab = {"Accept": "application/json, application/x-ndjson, application/octet-stream"}
        if auth and self._token:
            cab["Authorization"] = "Bearer " + self._token
        if content_type:
            cab["Content-Type"] = content_type
        conn = self._conectar(timeout)
        try:
            conn.request(metodo, destino, body=cuerpo, headers=cab)
            resp = conn.getresponse()
        except (OSError, http.client.HTTPException) as e:
            conn.close()
            raise TransportError(self._limpia("%s %s: %s" % (metodo, ruta, e))) from None
        if resp.status >= 300:
            try:
                datos = resp.read()
            except (OSError, http.client.HTTPException):
                datos = b""
            finally:
                conn.close()
            msg, code = "", None
            try:
                j = json.loads(datos)
                msg, code = str(j.get("message", "")), j.get("code")
            except (ValueError, AttributeError):
                msg = datos.decode("utf-8", "replace").strip()[:500]
            raise error_para(resp.status, self._limpia(msg or resp.reason or ""), code,
                             _retry_after(resp.getheader("Retry-After")))
        return conn, resp

    def _leer(self, conn, resp, ruta: str) -> bytes:
        try:
            return resp.read()
        except (OSError, http.client.HTTPException) as e:
            raise TransportError(self._limpia("%s: %s" % (ruta, e))) from None
        finally:
            conn.close()

    def _json(self, metodo: str, ruta: str, query=None, cuerpo: Optional[dict] = None,
              timeout: Any = "defecto", auth: bool = True):
        datos = json.dumps(cuerpo).encode() if cuerpo is not None else None
        conn, resp = self._abrir(metodo, ruta, query, datos,
                                 "application/json" if datos is not None else None, timeout, auth)
        crudo = self._leer(conn, resp, ruta)
        if resp.status == 204 or not crudo:
            return None
        try:
            return json.loads(crudo)
        except ValueError:
            raise TransportError("respuesta no JSON de %s" % ruta) from None

    # ---- ops

    def health(self) -> bool:
        return bool((self._json("GET", "/v1/health", auth=False) or {}).get("ok"))

    def hosts(self) -> List[dict]:
        return self._json("GET", "/v1/hosts") or []

    def templates(self) -> List[dict]:
        return self._json("GET", "/v1/templates") or []

    def metrics(self) -> str:
        conn, resp = self._abrir("GET", "/v1/metrics")
        return self._leer(conn, resp, "/v1/metrics").decode("utf-8", "replace")

    # ---- sandboxes

    def create_sandbox(self, **peticion) -> SandboxInfo:
        cuerpo = {k: v for k, v in peticion.items() if v is not None}
        return SandboxInfo.from_json(self._json("POST", "/v1/sandboxes", cuerpo=cuerpo))

    def create_sandboxes(self, count: int, atomic: bool = False, **peticion):
        """Crea un lote (`count`). Devuelve (lista de SandboxInfo, errores).

        Cada error es {"index", "status", "error"} de un sandbox que no salió
        (solo sin `atomic`: con `atomic` un fallo lanza la excepción de la API
        y no queda nada creado)."""
        cuerpo = {k: v for k, v in peticion.items() if v is not None}
        cuerpo["count"] = count
        if atomic:
            cuerpo["atomic"] = True
        j = self._json("POST", "/v1/sandboxes", cuerpo=cuerpo) or {}
        return [SandboxInfo.from_json(x) for x in j.get("sandboxes") or []], j.get("errors") or []

    def get_sandbox(self, id: str) -> SandboxInfo:
        return SandboxInfo.from_json(self._json("GET", "/v1/sandboxes/" + id))

    def list_sandboxes(self):
        conn, resp = self._abrir("GET", "/v1/sandboxes")
        parcial = resp.getheader("X-Kindling-Partial")
        crudo = self._leer(conn, resp, "/v1/sandboxes")
        try:
            lista = [SandboxInfo.from_json(x) for x in json.loads(crudo or b"[]") or []]
        except ValueError:
            raise TransportError("respuesta no JSON de /v1/sandboxes") from None
        return lista, parcial

    def delete_sandbox(self, id: str) -> None:
        self._json("DELETE", "/v1/sandboxes/" + id)

    def renew_sandbox(self, id: str, ttl_seconds: Optional[int] = None) -> SandboxInfo:
        cuerpo = {"ttl_seconds": ttl_seconds} if ttl_seconds is not None else None
        return SandboxInfo.from_json(self._json("POST", "/v1/sandboxes/%s/renew" % id, cuerpo=cuerpo))

    # ---- exec

    @staticmethod
    def _plazo_exec(peticion: dict, timeout: Any):
        # Sin tope de comando, la lectura puede esperar indefinidamente.
        t = peticion.get("timeout_seconds")
        return t + 30 if t else None

    def exec_wait(self, id: str, peticion: dict) -> ExecResult:
        j = self._json("POST", "/v1/sandboxes/%s/exec" % id, {"wait": "1"}, peticion,
                       timeout=self._plazo_exec(peticion, None))
        return ExecResult.from_json(j)

    def exec_stream(self, id: str, peticion: dict) -> Iterator[ExecEvent]:
        ruta = "/v1/sandboxes/%s/exec" % id
        conn, resp = self._abrir("POST", ruta, None, json.dumps(peticion).encode(),
                                 "application/json", self._plazo_exec(peticion, None))
        return self._eventos(conn, resp, ruta)

    def _eventos(self, conn, resp, ruta: str) -> Iterator[ExecEvent]:
        try:
            while True:
                try:
                    linea = resp.readline()
                except (OSError, http.client.HTTPException) as e:
                    raise TransportError(self._limpia("%s: flujo cortado: %s" % (ruta, e))) from None
                if not linea:
                    return
                linea = linea.strip()
                if not linea:
                    continue
                try:
                    yield ExecEvent.from_json(json.loads(linea))
                except ValueError:
                    raise TransportError("%s: línea NDJSON inválida" % ruta) from None
        finally:
            conn.close()

    # ---- ficheros

    def read_file(self, id: str, path: str) -> bytes:
        ruta = "/v1/sandboxes/%s/files" % id
        conn, resp = self._abrir("GET", ruta, {"path": path}, timeout=None)
        return self._leer(conn, resp, ruta)

    def stat_file(self, id: str, path: str) -> FileStat:
        return FileStat.from_json(self._json(
            "GET", "/v1/sandboxes/%s/files" % id, {"path": path, "stat": "1"}))

    def write_file(self, id: str, path: str, datos: bytes, mode: Optional[int] = None,
                   mkdir: bool = False) -> FileStat:
        q = {"path": path}
        if mode is not None:
            q["mode"] = "%04o" % mode
        if mkdir:
            q["mkdir"] = "1"
        ruta = "/v1/sandboxes/%s/files" % id
        conn, resp = self._abrir("PUT", ruta, q, bytes(datos), "application/octet-stream", timeout=None)
        return FileStat.from_json(json.loads(self._leer(conn, resp, ruta) or b"{}"))

    def delete_file(self, id: str, path: str) -> None:
        self._json("DELETE", "/v1/sandboxes/%s/files" % id, {"path": path})

    # ---- volúmenes

    def create_volume(self, name: str, size_mib: Optional[int] = None,
                      clone_from: Optional[str] = None, host: Optional[str] = None) -> Volume:
        cuerpo = {k: v for k, v in dict(name=name, size_mib=size_mib, clone_from=clone_from,
                                        host=host).items() if v is not None}
        return Volume.from_json(self._json("POST", "/v1/volumes", cuerpo=cuerpo))

    def get_volume(self, name: str) -> Volume:
        return Volume.from_json(self._json("GET", "/v1/volumes/" + name))

    def list_volumes(self) -> List[Volume]:
        return [Volume.from_json(x) for x in self._json("GET", "/v1/volumes") or []]

    def delete_volume(self, name: str) -> None:
        self._json("DELETE", "/v1/volumes/" + name)

    # ---- grafos

    def create_graph(self, template: str) -> Graph:
        return Graph.from_json(self._json("POST", "/v1/graphs", cuerpo={"template": template}))

    def get_graph(self, id: str) -> Graph:
        return Graph.from_json(self._json("GET", "/v1/graphs/" + id))

    def list_graphs(self) -> List[Graph]:
        return [Graph.from_json(x) for x in self._json("GET", "/v1/graphs") or []]

    def delete_graph(self, id: str) -> None:
        self._json("DELETE", "/v1/graphs/" + id)

