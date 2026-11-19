"""API de alto nivel síncrona: Sandbox, Files."""

from typing import Any, Dict, Iterator, List, Optional, Sequence, Union

from .client import Client
from .models import ExecEvent, ExecResult, FileStat, ProcessEvent, ProcessInfo, SandboxInfo, Session


class SandboxList(list):
    """Lista de Sandbox; `partial` trae la cabecera X-Kindling-Partial (hosts caídos)."""
    partial: Optional[str] = None


class SandboxBatch(list):
    """Lista de Sandbox de un lote; `errors` son los que no salieron (sin `atomic`)."""
    errors: list = []


def _cliente(client, url, token, timeout) -> Client:
    return client if client is not None else Client(url=url, token=token, timeout=timeout)


def _peticion_exec(cmd, cwd, env, timeout, stdin, user, max_output_bytes, session=None) -> dict:
    if isinstance(cmd, str):
        cmd = ["sh", "-c", cmd]  # un string es una línea de shell
    p = {"cmd": list(cmd)}
    if cwd:
        p["dir"] = cwd
    if env:
        p["env"] = ["%s=%s" % kv for kv in env.items()] if isinstance(env, dict) else list(env)
    if stdin is not None:
        import base64
        raw = stdin.encode() if isinstance(stdin, str) else bytes(stdin)
        p["stdin"] = base64.b64encode(raw).decode()
    if timeout:
        p["timeout_seconds"] = int(timeout)
    if max_output_bytes:
        p["max_output_bytes"] = int(max_output_bytes)
    if user:
        p["user"] = user
    if session:
        p["session"] = session
    return p


class Files:
    """Ficheros de un sandbox: `sb.files.read("/etc/hostname")`."""

    def __init__(self, client: Client, id: str):
        self._c = client
        self._id = id

    def read(self, path: str) -> bytes:
        return self._c.read_file(self._id, path)

    def read_text(self, path: str, encoding: str = "utf-8") -> str:
        return self.read(path).decode(encoding)

    def write(self, path: str, data: Union[bytes, bytearray, str], mode: Optional[int] = None,
              mkdir: bool = False) -> FileStat:
        """`mode` es un entero (0o644). `mkdir=True` crea los directorios que falten."""
        if isinstance(data, str):
            data = data.encode()
        return self._c.write_file(self._id, path, data, mode, mkdir)

    def delete(self, path: str) -> None:
        self._c.delete_file(self._id, path)

    def stat(self, path: str) -> FileStat:
        return self._c.stat_file(self._id, path)


class Processes:
    """Procesos de fondo de un sandbox: sobreviven a la conexión del cliente.

    `sb.processes.start("sleep 60")` devuelve un ProcessInfo con el `pid`;
    `output(pid, follow=True)` itera su salida y `kill(pid)` lo termina.
    """

    def __init__(self, client: Client, id: str):
        self._c = client
        self._id = id

    def start(self, cmd: Union[str, Sequence[str]], cwd: Optional[str] = None,
              env: Optional[Union[Dict[str, str], Sequence[str]]] = None,
              timeout: Optional[int] = None, user: Optional[str] = None,
              max_output_bytes: Optional[int] = None, session: Optional[str] = None) -> ProcessInfo:
        """Lanza y vuelve enseguida. Un `str` se ejecuta con `sh -c`."""
        p = _peticion_exec(cmd, cwd, env, timeout, None, user, max_output_bytes, session)
        return self._c.start_process(self._id, p)

    def list(self) -> List[ProcessInfo]:
        return self._c.list_processes(self._id)

    def get(self, pid: int, wait: Optional[int] = None) -> ProcessInfo:
        """Estado. Con `wait` (segundos) espera a que termine; si no, sigue «running»."""
        return self._c.get_process(self._id, pid, wait)

    def output(self, pid: int, offset: int = 0, follow: bool = False) -> Iterator[ProcessEvent]:
        """Iterador de ProcessEvent desde `offset`; con `follow` hasta que termine."""
        return self._c.process_output(self._id, pid, offset, follow)

    def signal(self, pid: int, signal: Optional[str] = None) -> ProcessInfo:
        """Señal al grupo del proceso (por defecto TERM)."""
        return self._c.signal_process(self._id, pid, signal)

    def kill(self, pid: int) -> ProcessInfo:
        return self.signal(pid, "KILL")

    def delete(self, pid: int) -> None:
        """Mata el proceso si sigue vivo y olvida su salida."""
        self._c.delete_process(self._id, pid)


class Sessions:
    """Sesiones de exec: cwd y entorno que persisten entre comandos."""

    def __init__(self, client: Client, id: str):
        self._c = client
        self._id = id

    def create(self, id: Optional[str] = None, cwd: Optional[str] = None,
               env: Optional[Union[Dict[str, str], Sequence[str]]] = None,
               ttl: Optional[int] = None) -> Session:
        p: Dict[str, Any] = {}
        if id:
            p["id"] = id
        if cwd:
            p["dir"] = cwd
        if env:
            p["env"] = ["%s=%s" % kv for kv in env.items()] if isinstance(env, dict) else list(env)
        if ttl:
            p["ttl_seconds"] = int(ttl)
        return self._c.create_session(self._id, p)

    def list(self) -> List[Session]:
        return self._c.list_sessions(self._id)

    def get(self, sid: str) -> Session:
        return self._c.get_session(self._id, sid)

    def exec(self, sid: str, cmd: Union[str, Sequence[str]], cwd: Optional[str] = None,
             env: Optional[Union[Dict[str, str], Sequence[str]]] = None,
             timeout: Optional[int] = None, stdin: Optional[Union[str, bytes]] = None,
             user: Optional[str] = None, max_output_bytes: Optional[int] = None) -> ExecResult:
        """Como Sandbox.exec, dentro de la sesión `sid`."""
        return self._c.exec_wait(
            self._id, _peticion_exec(cmd, cwd, env, timeout, stdin, user, max_output_bytes, sid))

    def delete(self, sid: str) -> None:
        self._c.delete_session(self._id, sid)


class Sandbox:
    def __init__(self, info: SandboxInfo, client: Client):
        self.info = info
        self._client = client
        self.files = Files(client, info.id)
        self.processes = Processes(client, info.id)
        self.sessions = Sessions(client, info.id)

    def __repr__(self) -> str:
        return "Sandbox(id=%r, state=%r)" % (self.info.id, self.info.state)

    @property
    def id(self) -> str:
        return self.info.id

    # ---- ciclo de vida

    @classmethod
    def create(cls, template: Optional[str] = None, ttl: Optional[int] = None, *,
               image: Optional[str] = None, on_ttl: Optional[str] = None,
               egress: Optional[str] = None, allow_domains: Optional[Sequence[str]] = None,
               mem_mib: Optional[int] = None, vcpus: Optional[int] = None,
               labels: Optional[Dict[str, str]] = None,
               volumes: Optional[Sequence[Dict[str, Any]]] = None, url: Optional[str] = None,
               token: Optional[str] = None, timeout: Optional[float] = 60.0,
               client: Optional[Client] = None) -> "Sandbox":
        """`ttl` en segundos. `volumes`: [{"name", "mount", "read_only", "clone_from"}]
        con volúmenes del inquilino (ver Client.create_volume)."""
        c = _cliente(client, url, token, timeout)
        info = c.create_sandbox(template=template, image=image, ttl_seconds=ttl, on_ttl=on_ttl,
                                egress=egress,
                                allow_domains=list(allow_domains) if allow_domains else None,
                                mem_mib=mem_mib, vcpus=vcpus, labels=labels,
                                volumes=[dict(v) for v in volumes] if volumes else None)
        return cls(info, c)

    @classmethod
    def create_many(cls, count: int, template: Optional[str] = None, ttl: Optional[int] = None, *,
                    atomic: bool = False, image: Optional[str] = None,
                    on_ttl: Optional[str] = None, egress: Optional[str] = None,
                    allow_domains: Optional[Sequence[str]] = None,
                    mem_mib: Optional[int] = None, vcpus: Optional[int] = None,
                    labels: Optional[Dict[str, str]] = None,
                    volumes: Optional[Sequence[Dict[str, Any]]] = None,
                    url: Optional[str] = None, token: Optional[str] = None,
                    timeout: Optional[float] = 300.0,
                    client: Optional[Client] = None) -> SandboxBatch:
        """Crea `count` sandboxes iguales de una vez (evaluaciones, RL).

        Con `atomic=True` es todo o nada: si uno falla se borran los demás y se
        lanza la excepción. Sin él, un lote parcial devuelve los que salieron y
        `.errors` dice cuáles no. Un lote solo monta volúmenes `read_only`."""
        c = _cliente(client, url, token, timeout)
        infos, errores = c.create_sandboxes(
            count, atomic=atomic, template=template, image=image, ttl_seconds=ttl, on_ttl=on_ttl,
            egress=egress, allow_domains=list(allow_domains) if allow_domains else None,
            mem_mib=mem_mib, vcpus=vcpus, labels=labels,
            volumes=[dict(v) for v in volumes] if volumes else None)
        out = SandboxBatch(cls(i, c) for i in infos)
        out.errors = errores
        return out

    @classmethod
    def connect(cls, id: str, *, url=None, token=None, timeout: Optional[float] = 60.0,
                client: Optional[Client] = None) -> "Sandbox":
        c = _cliente(client, url, token, timeout)
        return cls(c.get_sandbox(id), c)

    @classmethod
    def list(cls, *, url=None, token=None, timeout: Optional[float] = 60.0,
             client: Optional[Client] = None) -> SandboxList:
        c = _cliente(client, url, token, timeout)
        infos, parcial = c.list_sandboxes()
        out = SandboxList(cls(i, c) for i in infos)
        out.partial = parcial
        return out

    def refresh(self) -> "Sandbox":
        self.info = self._client.get_sandbox(self.id)
        return self

    def renew(self, ttl: Optional[int] = None) -> SandboxInfo:
        """Renueva el TTL (segundos; sin valor, el del sandbox)."""
        self.info = self._client.renew_sandbox(self.id, ttl)
        return self.info

    def delete(self) -> None:
        self._client.delete_sandbox(self.id)

    kill = delete

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc) -> None:
        from .errors import NotFound
        try:
            self.delete()
        except NotFound:
            pass

    # ---- exec

    def exec(self, cmd: Union[str, Sequence[str]], cwd: Optional[str] = None,
             env: Optional[Union[Dict[str, str], Sequence[str]]] = None,
             timeout: Optional[int] = None, stdin: Optional[Union[str, bytes]] = None,
             user: Optional[str] = None, max_output_bytes: Optional[int] = None,
             session: Optional[str] = None) -> ExecResult:
        """Espera al final. Un `str` se ejecuta con `sh -c`; una lista, tal cual.
        `timeout` en segundos (tope del comando en el servidor). `session`: id de
        una sesión (ver `sessions`) cuyo cwd y entorno se usan."""
        return self._client.exec_wait(
            self.id, _peticion_exec(cmd, cwd, env, timeout, stdin, user, max_output_bytes, session))

    def exec_stream(self, cmd: Union[str, Sequence[str]], cwd: Optional[str] = None,
                    env: Optional[Union[Dict[str, str], Sequence[str]]] = None,
                    timeout: Optional[int] = None, stdin: Optional[Union[str, bytes]] = None,
                    user: Optional[str] = None,
                    max_output_bytes: Optional[int] = None,
                    session: Optional[str] = None) -> Iterator[ExecEvent]:
        """Iterador de ExecEvent (NDJSON). El último trae `exit` o `error`.
        La petición se envía ya; los errores previos al flujo saltan aquí."""
        return self._client.exec_stream(
            self.id, _peticion_exec(cmd, cwd, env, timeout, stdin, user, max_output_bytes, session))
