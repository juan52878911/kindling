"""Tipos de datos de la API. Se toleran campos nuevos (se guardan en `extra`)."""

import base64
from dataclasses import dataclass, field, fields
from typing import Any, Dict, List, Optional


def _b64(valor: Optional[str]) -> bytes:
    return base64.b64decode(valor) if valor else b""


class _Base:
    @classmethod
    def from_json(cls, j: Optional[dict]):
        j = j or {}
        nombres = {f.name for f in fields(cls) if f.name != "extra"}
        obj = cls(**{k: v for k, v in j.items() if k in nombres})
        obj.extra = {k: v for k, v in j.items() if k not in nombres}
        return obj


@dataclass
class SandboxInfo(_Base):
    id: str = ""
    host: str = ""
    template: str = ""
    image: str = ""
    state: str = ""
    vcpus: int = 0
    mem_mib: int = 0
    egress: str = ""
    allow_domains: List[str] = field(default_factory=list)
    on_ttl: str = ""
    ttl_seconds: int = 0
    created_at: str = ""
    last_used_at: str = ""
    expires_at: str = ""
    labels: Dict[str, str] = field(default_factory=dict)
    extra: Dict[str, Any] = field(default_factory=dict)


@dataclass
class FileStat(_Base):
    path: str = ""
    size: int = 0
    mode: str = ""
    is_dir: bool = False
    mod_time: str = ""
    extra: Dict[str, Any] = field(default_factory=dict)


@dataclass
class Graph(_Base):
    id: str = ""
    host: str = ""
    template: str = ""
    state: str = ""
    claimed_at: str = ""
    nodes: Dict[str, dict] = field(default_factory=dict)
    extra: Dict[str, Any] = field(default_factory=dict)


@dataclass
class ExecEvent:
    """Una línea del flujo. `stream` es «stdout»/«stderr» con `data` en bytes;
    el último evento trae `exit` (o `error` si el flujo se cortó tras el 200)."""
    stream: Optional[str] = None
    data: bytes = b""
    exit: Optional[int] = None
    duration_ms: int = 0
    truncated: bool = False
    timed_out: bool = False
    error: Optional[str] = None

    @classmethod
    def from_json(cls, j: dict) -> "ExecEvent":
        return cls(stream=j.get("stream"), data=_b64(j.get("data")), exit=j.get("exit"),
                   duration_ms=j.get("duration_ms", 0), truncated=j.get("truncated", False),
                   timed_out=j.get("timed_out", False), error=j.get("error"))

    @property
    def text(self) -> str:
        return self.data.decode("utf-8", "replace")


@dataclass
class ExecResult:
    exit_code: int = 0
    stdout_bytes: bytes = b""
    stderr_bytes: bytes = b""
    duration_ms: int = 0
    truncated: bool = False
    timed_out: bool = False

    @classmethod
    def from_json(cls, j: Optional[dict]) -> "ExecResult":
        j = j or {}
        return cls(exit_code=j.get("exit_code", 0), stdout_bytes=_b64(j.get("stdout")),
                   stderr_bytes=_b64(j.get("stderr")), duration_ms=j.get("duration_ms", 0),
                   truncated=j.get("truncated", False), timed_out=j.get("timed_out", False))

    @property
    def stdout(self) -> str:
        return self.stdout_bytes.decode("utf-8", "replace")

    @property
    def stderr(self) -> str:
        return self.stderr_bytes.decode("utf-8", "replace")


@dataclass
class Volume(_Base):
    name: str = ""
    host: str = ""
    size_mib: int = 0
    used_bytes: int = 0
    in_use: bool = False
    extra: Dict[str, Any] = field(default_factory=dict)
