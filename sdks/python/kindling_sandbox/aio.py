"""Capa asíncrona fina: cada llamada del SDK síncrono corre en un hilo (asyncio.to_thread)."""

import asyncio
from typing import Optional

from .client import Client
from .errors import NotFound
from .sandbox import Sandbox, SandboxBatch

_FIN = object()


async def _hilo(fn, *a, **k):
    return await asyncio.to_thread(fn, *a, **k)


class AsyncFiles:
    def __init__(self, files):
        self._f = files

    async def read(self, path):
        return await _hilo(self._f.read, path)

    async def read_text(self, path, encoding="utf-8"):
        return await _hilo(self._f.read_text, path, encoding)

    async def write(self, path, data, mode=None, mkdir=False):
        return await _hilo(self._f.write, path, data, mode, mkdir)

    async def delete(self, path):
        await _hilo(self._f.delete, path)

    async def stat(self, path):
        return await _hilo(self._f.stat, path)


class AsyncProcesses:
    def __init__(self, procs):
        self._p = procs

    async def start(self, *a, **k):
        return await _hilo(self._p.start, *a, **k)

    async def list(self):
        return await _hilo(self._p.list)

    async def get(self, pid, wait=None):
        return await _hilo(self._p.get, pid, wait)

    async def output(self, pid, offset=0, follow=False):
        """Iterador asíncrono: `async for ev in await sb.processes.output(pid, follow=True)`."""
        return _IterAsync(await _hilo(self._p.output, pid, offset, follow))

    async def signal(self, pid, signal=None):
        return await _hilo(self._p.signal, pid, signal)

    async def kill(self, pid):
        return await _hilo(self._p.kill, pid)

    async def delete(self, pid):
        await _hilo(self._p.delete, pid)


class AsyncSessions:
    def __init__(self, sesiones):
        self._s = sesiones

    async def create(self, *a, **k):
        return await _hilo(self._s.create, *a, **k)

    async def list(self):
        return await _hilo(self._s.list)

    async def get(self, sid):
        return await _hilo(self._s.get, sid)

    async def exec(self, sid, cmd, **k):
        return await _hilo(self._s.exec, sid, cmd, **k)

    async def delete(self, sid):
        await _hilo(self._s.delete, sid)


class AsyncSandbox:
    def __init__(self, sb: Sandbox):
        self._sb = sb
        self.files = AsyncFiles(sb.files)
        self.processes = AsyncProcesses(sb.processes)
        self.sessions = AsyncSessions(sb.sessions)

    def __repr__(self):
        return "Async" + repr(self._sb)

    @property
    def id(self):
        return self._sb.id

    @property
    def info(self):
        return self._sb.info

    @classmethod
    async def create(cls, *a, **k) -> "AsyncSandbox":
        return cls(await _hilo(Sandbox.create, *a, **k))

    @classmethod
    async def create_many(cls, *a, **k):
        lote = await _hilo(Sandbox.create_many, *a, **k)
        out = SandboxBatch(cls(s) for s in lote)
        out.errors = lote.errors
        return out

    @classmethod
    async def connect(cls, id, **k) -> "AsyncSandbox":
        return cls(await _hilo(Sandbox.connect, id, **k))

    @classmethod
    async def list(cls, **k):
        return [cls(s) for s in await _hilo(Sandbox.list, **k)]

    async def refresh(self):
        await _hilo(self._sb.refresh)
        return self

    async def renew(self, ttl: Optional[int] = None):
        return await _hilo(self._sb.renew, ttl)

    async def delete(self):
        await _hilo(self._sb.delete)

    async def exec(self, *a, **k):
        return await _hilo(self._sb.exec, *a, **k)

    async def exec_stream(self, *a, **k):
        """Iterador asíncrono: `async for ev in await sb.exec_stream(...)`."""
        it = await _hilo(self._sb.exec_stream, *a, **k)
        return _IterAsync(it)

    async def __aenter__(self):
        return self

    async def __aexit__(self, *exc):
        try:
            await self.delete()
        except NotFound:
            pass


class _IterAsync:
    def __init__(self, it):
        self._it = it

    def __aiter__(self):
        return self

    async def __anext__(self):
        ev = await _hilo(next, self._it, _FIN)
        if ev is _FIN:
            raise StopAsyncIteration
        return ev

    async def aclose(self):
        await _hilo(self._it.close)
