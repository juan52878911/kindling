"""Transporte HTTP y rutas de la API /v1 (solo urllib.parse + http.client)."""

import http.client
import json
import os
import select
import socket
import ssl
import threading
import time
from email.utils import parsedate_to_datetime
from typing import Any, Dict, Iterator, List, Optional
from urllib.parse import quote, urlencode, urlsplit

from .errors import ApiError, ConfigError, TransportError, error_para
from .models import (CodeContext, CodeEvent, EgressPolicy, Endpoint, SearchResult, TarResult, ExecEvent, ExecResult, FileList, FileStat, Graph, ProcessEvent, ProcessInfo, ReplaceResult,
                     SandboxInfo, SandboxMetrics, Session, Volume)

ENV_URL = "KLING_SANDBOX_URL"
ENV_TOKEN = "KLING_SANDBOX_TOKEN"
URL_POR_DEFECTO = "http://127.0.0.1:8090"


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


# Errores de una conexión reutilizada que el servidor cerró justo al usarla (por
# inactividad). Solo se repiten los métodos idempotentes: un POST que pudo
# llegar a procesarse no se manda dos veces.
_CONEXION_CADUCADA = (http.client.RemoteDisconnected, ConnectionResetError, BrokenPipeError,
                      ConnectionAbortedError)
_IDEMPOTENTES = ("GET", "HEAD", "PUT", "DELETE", "OPTIONS")


def _viva(conn) -> bool:
    """Una conexión ociosa que el servidor ya cerró se lee como legible (EOF):
    esa no se reutiliza."""
    if conn.sock is None:
        return False
    try:
        legible, _, _ = select.select([conn.sock], [], [], 0)
    except (OSError, ValueError):
        return False
    return not legible


class Client:
    """Conexión al gateway. Cada hilo guarda una conexión abierta (keep-alive) y
    la reutiliza mientras el servidor la mantenga; con HTTPS, el contexto TLS
    se carga una vez por cliente."""

    def __init__(self, url: Optional[str] = None, token: Optional[str] = None,
                 timeout: Optional[float] = 60.0, retries: int = 0, max_retry_wait: float = 30.0):
        """`retries` > 0 reintenta (hasta ese número de veces) un 429 o 503,
        esperando el `Retry-After` del servidor (1 s si no lo trae; nunca más
        de `max_retry_wait`). Por defecto no reintenta nada."""
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
        self.retries = max(0, int(retries))
        self.max_retry_wait = max_retry_wait
        self._dormir = time.sleep
        # Un solo contexto TLS por cliente: crearlo carga el almacén de CAs.
        self._tls = ssl.create_default_context() if self._https else None
        self._local = threading.local()
        # Todas las conexiones guardadas, de cualquier hilo: close() las cierra
        # aunque se guardaran en hilos de un ejecutor (la capa asíncrona).
        self._cerrojo = threading.Lock()
        self._ociosas: Dict[int, Any] = {}

    def close(self) -> None:
        """Cierra las conexiones guardadas (de todos los hilos)."""
        with self._cerrojo:
            ociosas, self._ociosas = self._ociosas, {}
        for conn, pid in ociosas.values():
            if pid == os.getpid():
                conn.close()

    def __del__(self):
        try:
            self.close()
        except Exception:  # al recoger el objeto no hay a quién avisar
            pass

    def __repr__(self) -> str:
        return "Client(url=%r, token=%s)" % (self.url, "<oculto>" if self._token else "None")

    __str__ = __repr__

    # ---- bajo nivel

    def _conectar(self, timeout, reutilizar: bool = True):
        """Devuelve (conn, reutilizada). La guardada en el hilo se toma (y deja
        de estar guardada) solo si es de este proceso: tras un fork, la del
        padre no se comparte."""
        conn = getattr(self._local, "conn", None) if reutilizar else None
        if conn is not None:
            self._local.conn = None
            with self._cerrojo:
                guardada = self._ociosas.pop(id(conn), None)
            if guardada is not None and guardada[1] == os.getpid():
                if _viva(conn):
                    conn.timeout = timeout
                    conn.sock.settimeout(timeout)
                    return conn, True
                conn.close()
        if self._https:
            return http.client.HTTPSConnection(self._host, self._port, timeout=timeout,
                                               context=self._tls), False
        return http.client.HTTPConnection(self._host, self._port, timeout=timeout), False

    def _devolver(self, conn, resp) -> None:
        """Guarda conn para la siguiente petición de este hilo si la respuesta
        se leyó entera y el servidor no pidió cerrar; si no, la cierra."""
        if resp.will_close or not resp.isclosed():
            conn.close()
            return
        vieja = getattr(self._local, "conn", None)
        self._local.conn = conn
        with self._cerrojo:
            self._ociosas[id(conn)] = (conn, os.getpid())
            if vieja is not None and vieja is not conn:
                self._ociosas.pop(id(vieja), None)
        if vieja is not None and vieja is not conn:
            vieja.close()

    def _limpia(self, texto: str) -> str:
        if self._token and self._token in texto:
            texto = texto.replace(self._token, "<oculto>")
        return texto

    def _abrir(self, metodo: str, ruta: str, query=None, cuerpo=None, content_type: Optional[str] = None,
               timeout: Any = "defecto", auth: bool = True, cabeceras: Optional[Dict[str, str]] = None):
        """Devuelve (conn, resp) con el estado ya validado: si no es 2xx, lanza.
        Con `retries`, un 429/503 se reintenta tras su `Retry-After`."""
        intento = 0
        while True:
            try:
                return self._abrir_una(metodo, ruta, query, cuerpo, content_type, timeout, auth, cabeceras)
            except ApiError as e:
                if e.status not in (429, 503) or intento >= self.retries:
                    raise
                if hasattr(cuerpo, "seek"):  # un cuerpo en flujo hay que rebobinarlo
                    cuerpo.seek(0)
                intento += 1
                espera = e.retry_after if e.retry_after is not None else 1.0
                self._dormir(min(max(espera, 0.0), self.max_retry_wait))

    def _abrir_una(self, metodo, ruta, query, cuerpo, content_type, timeout, auth, cabeceras):
        if timeout == "defecto":
            timeout = self.timeout
        destino = self._prefijo + quote(ruta, safe="/")
        if query:
            destino += "?" + urlencode(query, doseq=True)
        cab = {"Accept": "application/json, application/x-ndjson, application/octet-stream"}
        if auth and self._token:
            cab["Authorization"] = "Bearer " + self._token
        if content_type:
            cab["Content-Type"] = content_type
        if cabeceras:
            cab.update(cabeceras)
        # Un cuerpo en flujo no se puede repetir: va siempre por una conexión nueva.
        repetible = cuerpo is None or isinstance(cuerpo, (bytes, bytearray))
        conn, reutilizada = self._conectar(timeout, repetible)
        while True:
            try:
                conn.request(metodo, destino, body=cuerpo, headers=cab)
                resp = conn.getresponse()
                break
            except _CONEXION_CADUCADA as e:
                conn.close()
                if not reutilizada or metodo not in _IDEMPOTENTES:
                    raise TransportError(self._limpia("%s %s: %s" % (metodo, ruta, e))) from None
                conn, reutilizada = self._conectar(timeout, False)
            except (OSError, http.client.HTTPException) as e:
                conn.close()
                raise TransportError(self._limpia("%s %s: %s" % (metodo, ruta, e))) from None
        if resp.status >= 300:
            try:
                datos = resp.read()
            except (OSError, http.client.HTTPException):
                datos = b""
                conn.close()
            else:
                self._devolver(conn, resp)
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
            datos = resp.read()
        except (OSError, http.client.HTTPException) as e:
            conn.close()
            raise TransportError(self._limpia("%s: %s" % (ruta, e))) from None
        self._devolver(conn, resp)
        return datos

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

    def list_sandboxes(self, state: Optional[str] = None, label: Optional[Any] = None,
                       limit: Optional[int] = None, cursor: Optional[str] = None):
        """Devuelve (lista, parcial, siguiente): `parcial` es X-Kindling-Partial y
        `siguiente`, el X-Kindling-Next-Cursor (None en la última página).
        `label` es un dict {clave: valor} o una lista de "clave=valor"."""
        q: Dict[str, Any] = {}
        if state:
            q["state"] = state
        if label:
            q["label"] = ["%s=%s" % kv for kv in label.items()] if isinstance(label, dict) else list(label)
        if limit:
            q["limit"] = str(int(limit))
        if cursor:
            q["cursor"] = cursor
        conn, resp = self._abrir("GET", "/v1/sandboxes", q or None)
        parcial = resp.getheader("X-Kindling-Partial")
        siguiente = resp.getheader("X-Kindling-Next-Cursor")
        crudo = self._leer(conn, resp, "/v1/sandboxes")
        try:
            lista = [SandboxInfo.from_json(x) for x in json.loads(crudo or b"[]") or []]
        except ValueError:
            raise TransportError("respuesta no JSON de /v1/sandboxes") from None
        return lista, parcial, siguiente

    def delete_sandbox(self, id: str) -> None:
        self._json("DELETE", "/v1/sandboxes/" + id)

    def renew_sandbox(self, id: str, ttl_seconds: Optional[int] = None) -> SandboxInfo:
        cuerpo = {"ttl_seconds": ttl_seconds} if ttl_seconds is not None else None
        return SandboxInfo.from_json(self._json("POST", "/v1/sandboxes/%s/renew" % id, cuerpo=cuerpo))

    def pause_sandbox(self, id: str) -> SandboxInfo:
        return SandboxInfo.from_json(self._json("POST", "/v1/sandboxes/%s/pause" % id))

    def resume_sandbox(self, id: str) -> SandboxInfo:
        return SandboxInfo.from_json(self._json("POST", "/v1/sandboxes/%s/resume" % id))

    def set_egress(self, id: str, egress: Optional[str] = None,
                   allow_domains: Optional[List[str]] = None) -> EgressPolicy:
        cuerpo: Dict[str, Any] = {}
        if egress is not None:
            cuerpo["egress"] = egress
        if allow_domains is not None:
            cuerpo["allow_domains"] = list(allow_domains)
        return EgressPolicy.from_json(self._json("PATCH", "/v1/sandboxes/%s/egress" % id, cuerpo=cuerpo))

    def patch_metadata(self, id: str, labels: Optional[Dict[str, str]] = None,
                       remove: Optional[List[str]] = None) -> SandboxInfo:
        cuerpo: Dict[str, Any] = {}
        if labels:
            cuerpo["labels"] = dict(labels)
        if remove:
            cuerpo["remove"] = list(remove)
        return SandboxInfo.from_json(self._json("PATCH", "/v1/sandboxes/%s/metadata" % id, cuerpo=cuerpo))

    def fork_sandbox(self, id: str, count: Optional[int] = None,
                     ttl_seconds: Optional[int] = None) -> List[SandboxInfo]:
        cuerpo: Dict[str, Any] = {}
        if count is not None:
            cuerpo["count"] = int(count)
        if ttl_seconds is not None:
            cuerpo["ttl_seconds"] = int(ttl_seconds)
        r = self._json("POST", "/v1/sandboxes/%s/fork" % id, cuerpo=cuerpo) or {}
        return [SandboxInfo.from_json(x) for x in r.get("sandboxes") or []]

    def sandbox_metrics(self, id: str) -> SandboxMetrics:
        return SandboxMetrics.from_json(self._json("GET", "/v1/sandboxes/%s/metrics" % id))

    # ---- endpoints

    def create_endpoint(self, id: str, port: int, ttl_seconds: Optional[int] = None,
                        public: bool = False) -> Endpoint:
        cuerpo: Dict[str, Any] = {"port": int(port)}
        if ttl_seconds is not None:
            cuerpo["ttl_seconds"] = int(ttl_seconds)
        if public:
            cuerpo["public"] = True
        return Endpoint.from_json(self._json("POST", "/v1/sandboxes/%s/endpoints" % id, cuerpo=cuerpo))

    def list_endpoints(self, id: str) -> List[Endpoint]:
        return [Endpoint.from_json(x) for x in self._json("GET", "/v1/sandboxes/%s/endpoints" % id) or []]

    def revoke_endpoint(self, id: str, port: int) -> None:
        self._json("DELETE", "/v1/sandboxes/%s/endpoints/%d" % (id, int(port)))

    # ---- código

    def run_code(self, id: str, code: str, language: Optional[str] = None,
                 context_id: Optional[str] = None,
                 timeout_seconds: Optional[int] = None) -> Iterator[CodeEvent]:
        """Iterador de CodeEvent (NDJSON); el último es siempre `done`."""
        cuerpo: Dict[str, Any] = {"code": code}
        if language:
            cuerpo["language"] = language
        if context_id:
            cuerpo["context_id"] = context_id
        if timeout_seconds:
            cuerpo["timeout_seconds"] = int(timeout_seconds)
        ruta = "/v1/sandboxes/%s/code" % id
        # La lectura espera al plazo de la ejecución (60 s por defecto) más un margen.
        conn, resp = self._abrir("POST", ruta, None, json.dumps(cuerpo).encode(), "application/json",
                                 (timeout_seconds or 60) + 30)
        return self._eventos(conn, resp, ruta, CodeEvent)

    def create_code_context(self, id: str, language: Optional[str] = None,
                            cwd: Optional[str] = None) -> CodeContext:
        cuerpo = {k: v for k, v in dict(language=language, cwd=cwd).items() if v}
        return CodeContext.from_json(self._json("POST", "/v1/sandboxes/%s/code/contexts" % id, cuerpo=cuerpo))

    def list_code_contexts(self, id: str) -> List[CodeContext]:
        return [CodeContext.from_json(x) for x in
                self._json("GET", "/v1/sandboxes/%s/code/contexts" % id) or []]

    def get_code_context(self, id: str, cid: str) -> CodeContext:
        return CodeContext.from_json(self._json("GET", "/v1/sandboxes/%s/code/contexts/%s" % (id, cid)))

    def delete_code_context(self, id: str, cid: str) -> None:
        self._json("DELETE", "/v1/sandboxes/%s/code/contexts/%s" % (id, cid))

    def interrupt_code(self, id: str, cid: str) -> None:
        self._json("DELETE", "/v1/sandboxes/%s/code/contexts/%s/execution" % (id, cid))

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

    def _eventos(self, conn, resp, ruta: str, tipo=ExecEvent) -> Iterator[Any]:
        # Solo un flujo leído hasta el final deja la conexión reutilizable; uno
        # que se abandona a medias (o que falla) la cierra.
        entero = False
        try:
            while True:
                try:
                    linea = resp.readline()
                except (OSError, http.client.HTTPException) as e:
                    raise TransportError(self._limpia("%s: flujo cortado: %s" % (ruta, e))) from None
                if not linea:
                    entero = True
                    return
                linea = linea.strip()
                if not linea:
                    continue
                try:
                    yield tipo.from_json(json.loads(linea))
                except ValueError:
                    raise TransportError("%s: línea NDJSON inválida" % ruta) from None
        finally:
            if entero:
                self._devolver(conn, resp)
            else:
                conn.close()

    # ---- procesos de fondo y sesiones

    def start_process(self, id: str, peticion: dict) -> ProcessInfo:
        return ProcessInfo.from_json(self._json("POST", "/v1/sandboxes/%s/processes" % id, None, peticion))

    def list_processes(self, id: str) -> List[ProcessInfo]:
        return [ProcessInfo.from_json(p) for p in
                self._json("GET", "/v1/sandboxes/%s/processes" % id) or []]

    def get_process(self, id: str, pid: int, wait: Optional[int] = None) -> ProcessInfo:
        q = {"wait": str(int(wait))} if wait else None
        # Con wait la lectura espera hasta ese tiempo: el timeout del cliente lo cubre.
        t = (wait + 30) if wait else "defecto"
        return ProcessInfo.from_json(self._json(
            "GET", "/v1/sandboxes/%s/processes/%d" % (id, pid), q, timeout=t))

    def signal_process(self, id: str, pid: int, signal: Optional[str] = None) -> ProcessInfo:
        return ProcessInfo.from_json(self._json(
            "POST", "/v1/sandboxes/%s/processes/%d/signal" % (id, pid), None,
            {"signal": signal} if signal else {}))

    def delete_process(self, id: str, pid: int) -> None:
        self._json("DELETE", "/v1/sandboxes/%s/processes/%d" % (id, pid))

    def process_output(self, id: str, pid: int, offset: int = 0,
                       follow: bool = False) -> Iterator[ProcessEvent]:
        """Iterador de ProcessEvent. El último evento lleva el `offset` desde el
        que continuar y, si el proceso terminó, `exit`. Con `follow` la lectura
        no tiene tope de tiempo: sigue hasta que el proceso acaba."""
        ruta = "/v1/sandboxes/%s/processes/%d/output" % (id, pid)
        q = {"offset": str(int(offset))}
        if follow:
            q["follow"] = "1"
        conn, resp = self._abrir("GET", ruta, q, timeout=None if follow else "defecto")
        return self._eventos(conn, resp, ruta, ProcessEvent)

    def create_session(self, id: str, peticion: dict) -> Session:
        return Session.from_json(self._json("POST", "/v1/sandboxes/%s/sessions" % id, None, peticion))

    def list_sessions(self, id: str) -> List[Session]:
        return [Session.from_json(x) for x in self._json("GET", "/v1/sandboxes/%s/sessions" % id) or []]

    def get_session(self, id: str, sid: str) -> Session:
        return Session.from_json(self._json("GET", "/v1/sandboxes/%s/sessions/%s" % (id, sid)))

    def delete_session(self, id: str, sid: str) -> None:
        self._json("DELETE", "/v1/sandboxes/%s/sessions/%s" % (id, sid))

    # ---- ficheros

    def read_file(self, id: str, path: str, range: Optional[Any] = None) -> bytes:
        """`range`: (inicio, fin) inclusivos, (inicio, None) hasta el final, o una
        cadena `bytes=a-b` tal cual (también `bytes=-n` para los últimos n)."""
        ruta = "/v1/sandboxes/%s/files" % id
        cab = None
        if range is not None:
            if isinstance(range, str):
                cab = {"Range": range}
            else:
                a, b = range
                cab = {"Range": "bytes=%d-%s" % (a, "" if b is None else int(b))}
        conn, resp = self._abrir("GET", ruta, {"path": path}, timeout=None, cabeceras=cab)
        return self._leer(conn, resp, ruta)

    def move_file(self, id: str, origen: str, destino: str, overwrite: bool = False) -> None:
        cuerpo: Dict[str, Any] = {"from": origen, "to": destino}
        if overwrite:
            cuerpo["overwrite"] = True
        self._json("POST", "/v1/sandboxes/%s/files/move" % id, cuerpo=cuerpo)

    def mkdir_file(self, id: str, path: str) -> FileStat:
        return FileStat.from_json(self._json("POST", "/v1/sandboxes/%s/files/mkdir" % id, cuerpo={"path": path}))

    def chmod_file(self, id: str, path: str, mode: str, recursive: bool = False) -> FileStat:
        cuerpo: Dict[str, Any] = {"path": path, "mode": mode}
        if recursive:
            cuerpo["recursive"] = True
        return FileStat.from_json(self._json("POST", "/v1/sandboxes/%s/files/chmod" % id, cuerpo=cuerpo))

    def replace_in_file(self, id: str, path: str, old: str, new: str, all: bool = False) -> ReplaceResult:
        cuerpo: Dict[str, Any] = {"path": path, "old": old, "new": new}
        if all:
            cuerpo["all"] = True
        return ReplaceResult.from_json(self._json("POST", "/v1/sandboxes/%s/files/replace" % id, cuerpo=cuerpo))

    def search_files(self, id: str, path: str, glob: str) -> SearchResult:
        return SearchResult.from_json(self._json(
            "GET", "/v1/sandboxes/%s/files/search" % id, {"path": path, "glob": glob}))

    def upload_tar(self, id: str, path: str, datos: bytes, mkdir: bool = False) -> TarResult:
        q = {"path": path, "tar": "1"}
        if mkdir:
            q["mkdir"] = "1"
        ruta = "/v1/sandboxes/%s/files" % id
        conn, resp = self._abrir("PUT", ruta, q, bytes(datos), "application/x-tar", timeout=None)
        return TarResult.from_json(json.loads(self._leer(conn, resp, ruta) or b"{}"))

    def download_tar(self, id: str, path: str) -> bytes:
        ruta = "/v1/sandboxes/%s/files" % id
        conn, resp = self._abrir("GET", ruta, {"path": path, "tar": "1"}, timeout=None)
        return self._leer(conn, resp, ruta)

    def stat_file(self, id: str, path: str) -> FileStat:
        return FileStat.from_json(self._json(
            "GET", "/v1/sandboxes/%s/files" % id, {"path": path, "stat": "1"}))

    def list_files(self, id: str, path: str) -> FileList:
        return FileList.from_json(self._json(
            "GET", "/v1/sandboxes/%s/files" % id, {"path": path, "list": "1"}))

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

