# kindling-sandbox (SDK de Python)

Cliente de la API `/v1` del gateway de sandboxes de kindling. Solo biblioteca
estándar (Python ≥ 3.9), sin dependencias. API síncrona y una capa asíncrona
fina (`asyncio.to_thread`, sin aiohttp).

## Instalación local

```sh
pip install ./sdks/python        # o: pip install -e ./sdks/python
# sin instalar: PYTHONPATH=sdks/python python3 tu_script.py
```

Configuración por argumentos (`url=`, `token=`) o por entorno:
`KLING_SANDBOX_URL` (por defecto `http://localhost:8088`) y `KLING_SANDBOX_TOKEN`.
El token no aparece en `repr`, ni en los mensajes de error.

## Ejemplo

```python
from kindling_sandbox import Sandbox

with Sandbox.create(template="python", ttl=600) as sb:   # se borra al salir
    sb.files.write("/tmp/hola.py", "print('hola')\n", mkdir=True)
    r = sb.exec(["python3", "/tmp/hola.py"], timeout=30)
    print(r.exit_code, r.stdout)
    for ev in sb.exec_stream("for i in 1 2 3; do echo $i; sleep 1; done"):
        print(ev.stream, ev.text, ev.exit)
    sb.renew(1200)
```

- `exec(cmd, cwd, env, timeout, stdin, user, max_output_bytes)` → `ExecResult`
  (`stdout`, `stderr`, `exit_code`, `stdout_bytes`…). Un `str` se ejecuta con
  `sh -c`; una lista, tal cual. `env` puede ser dict.
- `exec_stream(...)` → iterador de `ExecEvent`; el último trae `exit` o `error`
  (flujo cortado tras el 200). Los errores previos al flujo son excepciones.
- `Sandbox.list()` (con `.partial` = cabecera `X-Kindling-Partial`),
  `Sandbox.connect(id)`, `sb.refresh()`, `sb.delete()`.
- Asíncrono: `from kindling_sandbox import AsyncSandbox`;
  `async with await AsyncSandbox.create(...) as sb:` y
  `async for ev in await sb.exec_stream(...)`.
- Cliente de bajo nivel: `Client` (`health`, `hosts`, `templates`, `metrics`,
  grafos `create_graph/get_graph/list_graphs/delete_graph`).

## Errores

`ApiError(status, message, code, retry_after)` y sus subclases `BadRequest`,
`Unauthorized`, `Forbidden`, `NotFound`, `Conflict`, `PayloadTooLarge`,
`RateLimited` (429) y `ServerError` (5xx). `retry_after` (segundos) sale de la
cabecera `Retry-After`. `TransportError`: red, plazo o flujo cortado.
Nota: el frontal solo envía `Retry-After` hoy en el 503 de «plantilla
reconstruyéndose»; en el 429 de cuota no lo envía y `retry_after` es `None`.

## Cobertura frente a `ext/sandbox/api/openapi.yaml`

| Operación | Ruta | SDK |
|---|---|---|
| health | `GET /v1/health` | `Client.health()` |
| hosts | `GET /v1/hosts` | `Client.hosts()` |
| metrics | `GET /v1/metrics` | `Client.metrics()` |
| templates | `GET /v1/templates` | `Client.templates()` |
| listar sandboxes | `GET /v1/sandboxes` | `Sandbox.list()` |
| crear | `POST /v1/sandboxes` | `Sandbox.create()` |
| ver | `GET /v1/sandboxes/{id}` | `Sandbox.connect()`, `refresh()` |
| destruir | `DELETE /v1/sandboxes/{id}` | `Sandbox.delete()` |
| renovar | `POST …/renew` | `Sandbox.renew()` |
| exec (flujo NDJSON) | `POST …/exec` | `Sandbox.exec_stream()` |
| exec (`?wait=1`) | `POST …/exec?wait=1` | `Sandbox.exec()` |
| shell interactivo | `POST …/shell` (`kling-shell/1`) | **no implementado** (upgrade a tramas binarias; fuera del alcance de B1) |
| leer / stat | `GET …/files[?stat=1]` | `files.read()`, `files.stat()` |
| escribir | `PUT …/files` (`mode`, `mkdir`) | `files.write()` |
| borrar fichero | `DELETE …/files` | `files.delete()` |
| crear lote | `POST /v1/sandboxes` (`count`, `atomic`) | `Sandbox.create_many()` (`.errors`), `Client.create_sandboxes()` |
| volúmenes | `/v1/volumes`, `/v1/volumes/{id}`; `volumes` al crear | `Client.create/get/list/delete_volume`, `Sandbox.create(volumes=[...])` |
| grafos | `/v1/graphs`, `/v1/graphs/{id}` | `Client.*_graph(s)` (sin capa `Sandbox`) |

## Tests

```sh
python3 -m unittest discover sdks/python/tests
```

Corren contra un servidor falso local (`tests/servidor_falso.py`) que sigue la
spec: NDJSON en trozos partidos a mitad de línea, 429 con `Retry-After`,
fichero binario de ida y vuelta, y comprobación de que el token no se filtra.
