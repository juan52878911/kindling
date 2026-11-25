# Shim compatible con E2B

`kling sbx e2b` escucha en un puerto y traduce el protocolo de [E2B](https://e2b.dev)
a la API `/v1` del gateway de sandboxes ([sandbox-api.md](sandbox-api.md)). Un SDK de E2B
(Python o JS, y `e2b-code-interpreter`) puede así crear sandboxes, ejecutar comandos y código
y mover ficheros contra kindling cambiando solo variables de entorno.

La especificación que sigue el shim es la pública de E2B, copiada en
`ext/sandbox/internal/e2b/spec/` (plano de control `openapi.yml`; envd: `envd.yaml` y los
`.proto` de `process` y `filesystem`).

> **Estado.** Probado con los SDK oficiales de E2B **`e2b==2.64.0` (Python, `Sandbox`
> síncrono), `e2b@2.64.0` (JS/TS) y `e2b-code-interpreter==2.11.10` (Python)**, en CI
> (`TestContratoE2B`, job `e2b` de `.github/workflows/sdks.yml`; scripts en
> [`scripts/e2b-sdk-real/`](../scripts/e2b-sdk-real/README.md)). El SDK habla con el shim y el
> shim con el gateway real, delante de un daemon falso que ejecuta las órdenes, el código y los
> ficheros en la máquina del test: **sin microVM**. Pasan:
>
> - `Sandbox.create` (con `metadata`, `envs` y `timeout`), `Sandbox.list` por metadata,
>   `get_info`, `is_running`, `set_timeout`, `Sandbox.connect`, `update_network` (dominios en
>   `allow_out` y abrir), `pause` + `Sandbox.connect` (reanuda) y `kill`.
> - `commands.run`: `stdout`/`on_stdout`, exit ≠ 0 como `CommandExitException`/`CommandExitError`
>   con su `stderr`, `envs` y `cwd`; `background=True` con `wait`, `commands.list` + `kill`, y
>   `disconnect` + `commands.connect(pid)`.
> - `files`: `write` y `read` (texto y bytes), `write_files` / `write([...])` con varios ficheros,
>   `exists`, `get_info`, `make_dir`, `rename`, `list(depth=2)` y `remove`.
> - `send_stdin`, `watch_dir` y `pty.create` fallan enseguida con `SandboxException` /
>   `SandboxError` (no se cuelgan).
> - `e2b_code_interpreter.Sandbox.run_code` (ver «Intérprete de código»): `text` del resultado,
>   estado entre llamadas, `logs` y `on_stdout`, `error` con traceback, resultado `html`,
>   `language="bash"`, y `create_code_context(cwd=)` / `list_code_contexts` /
>   `remove_code_context`.
>
> Por modo de conexión: con **`E2B_DOMAIN`** (HTTPS a `api.<dominio>` y a
> `<puerto>-<id>.<dominio>`, certificado verificado con `SSL_CERT_FILE`; en el test un proxy
> CONNECT hace de DNS comodín) pasan todos los pasos de `e2b` y de `e2b-code-interpreter` en
> Python. Con **`E2B_API_URL` + `E2B_SANDBOX_URL`** pasan todos con los tres SDK. Con
> **`E2B_DEBUG=true`**, los pasos de envd de `e2b` en Python.
>
> **Sin probar con el SDK:** el modo con dominio desde JS (`NODE_EXTRA_CA_CERTS`), `AsyncSandbox`
> de Python, el intérprete desde JS y en modo debug, otras versiones de los SDK y una microVM
> real detrás del gateway. Lo que la tabla marca como **501** sigue sin funcionar (stdin en vivo,
> PTY, watchers, builds de plantillas, snapshots/fork, métricas y logs; en el intérprete,
> `envs` por ejecución y reiniciar contextos), y el intérprete no devuelve gráficos (`chart`).

## Arranque

```sh
kling config set sandbox.url https://gateway:8090
kling config set sandbox.token - < token-del-inquilino
kling sbx e2b -listen 127.0.0.1:3000 [-domain e2b.example.test] [-tls-cert c.pem -tls-key k.pem]
```

| Flag | Qué hace |
|---|---|
| `-listen` | Dirección del plano de control y de envd (por defecto `127.0.0.1:3000`). |
| `-envd-listen` | Segunda dirección con el mismo manejador; para `E2B_DEBUG`, `127.0.0.1:49983`. |
| `-debug-sandbox` | Id del gateway al que apunta `E2B_DEBUG` (ver «Modo debug»). |
| `-domain` | Dominio base que el shim anuncia en cada sandbox (el `E2B_DOMAIN` del cliente). |
| `-home` | Directorio que resuelve las rutas relativas del SDK (por defecto `/home/user`). |
| `-tls-cert`, `-tls-key` | Sirve HTTPS; el SDK usa `https` hacia envd fuera de `E2B_DEBUG`. |
| `-url`, `-ca` | Gateway y CA extra para hablar con él, como en el resto de `kling sbx`. |

**La API key de E2B es el token del inquilino del gateway.** El SDK la manda en `X-API-Key`;
el shim la compara en tiempo constante y habla con el gateway con el token del inquilino que
tiene configurado. Si exportas `KLING_E2B_KEY`, esa es la clave que acepta el shim (y el SDK
no necesita nunca el token real). La clave no aparece en logs ni en mensajes de error: si el
gateway o el transporte la repiten, el shim la sustituye por `***`.

Cada sandbox recibe un `envdAccessToken` derivado de la clave (HMAC por sandbox): envd exige
`X-Access-Token` y el token de un sandbox no sirve en otro.

## Apuntar el SDK

**El `sandboxID` que ve E2B no es el id del gateway.** El gateway usa ids `host/ref`, que
rompen rutas (`DELETE /sandboxes/<id>`) y etiquetas DNS. El shim expone el id codificado en
base32 en minúsculas sin relleno (`h1/m-1` → `nbswy…`): determinista, reversible y sin estado,
así que sobrevive a reinicios. Cabe en la etiqueta `<puerto>-<id>` (≤ 63) si el id del gateway
mide ≤ 35 bytes; con ids más largos el host DNS no sirve, pero la cabecera `E2b-Sandbox-Id` y las
rutas siguen funcionando. Un id crudo con `/` se rechaza.

El shim enruta envd y el intérprete por el host `<puerto>-<sandboxID>.<dominio>` o, si no, por
la cabecera `E2b-Sandbox-Id`; envd exige `X-Access-Token` (el `envdAccessToken` de create) y el
intérprete, ese o `E2B-Traffic-Access-Token` (el `trafficAccessToken`, también por sandbox).

**Configuración estable: dominio + TLS.** Es la que usa el SDK sin variables obsoletas:
solo `E2B_DOMAIN` y `E2B_API_KEY`. El SDK va a `https://api.<dominio>` (plano de control) y a
`https://<puerto>-<id>.<dominio>` (envd en 49983, intérprete en 49999), siempre al puerto 443.
Necesita un DNS comodín `*.<dominio>` hacia la máquina del shim y un certificado de
`*.<dominio>` en el que confíe el cliente:

```sh
kling sbx e2b -listen :443 -domain e2b.example.test -tls-cert wildcard.pem -tls-key wildcard.key
export E2B_API_KEY=<token del inquilino>
export E2B_DOMAIN=e2b.example.test        # *.e2b.example.test → la máquina del shim
# Certificado de una CA propia: SSL_CERT_FILE=ca.pem (Python), NODE_EXTRA_CA_CERTS=ca.pem (JS)
```

Así lo prueba CI con el SDK de Python (`e2b` y `e2b-code-interpreter`). Un `<dominio>` de los
de E2B (`e2b.app`, `e2b.dev`…) no sirve: el SDK los trata aparte.

**Atajo de desarrollo: sin DNS ni TLS.** Con `E2B_API_URL` y `E2B_SANDBOX_URL` el SDK manda
todo, control, envd e intérprete, al mismo listener, identificando el sandbox y el puerto por
cabeceras. El SDK marca las dos variables como obsoletas («se quitarán en la próxima versión
mayor»), así que sirve para probar en local, no como configuración estable:

```sh
kling sbx e2b -listen 127.0.0.1:3000
export E2B_API_KEY=<token del inquilino>
export E2B_API_URL=http://127.0.0.1:3000 E2B_SANDBOX_URL=http://127.0.0.1:3000
```

**Modo debug (`E2B_DEBUG=true`), también de desarrollo.** Con él el SDK no llama al plano de
control: usa el id fijo `debug_sandbox_id`, no manda `X-Access-Token` y habla con
`http://localhost:49983`. Se soporta con una bandera explícita, sobre un sandbox del gateway que
ya existe (el intérprete iría a `localhost:49999`, que el shim no escucha en este modo):

```sh
kling sbx e2b -listen 127.0.0.1:3000 -envd-listen 127.0.0.1:49983 -debug-sandbox h1/m-1
export E2B_DEBUG=true E2B_API_KEY=<token del inquilino>
```

`-debug-sandbox` exige `-envd-listen` en una IP de loopback, sin TLS, y avisa en el log al
arrancar. Solo ese listener acepta, **sin token**, las peticiones de envd sin id o con
`debug_sandbox_id`, y solo para ese sandbox; el listener principal y el resto de ids siguen
exigiendo `X-Access-Token`. El plano de control no se usa en este modo: crea y borra el sandbox
con `kling sbx` (o con el SDK sin `E2B_DEBUG`).

```python
from e2b import Sandbox
sbx = Sandbox.create("base")          # POST /sandboxes
print(sbx.commands.run("echo hola").stdout)
sbx.files.write("/home/user/a.txt", "hola")
print(sbx.files.read("/home/user/a.txt"))
sbx.kill()
```

`templateID` se pasa tal cual como plantilla del gateway (`kling sbx template ls`).

## Formato de Connect

Solo se habla Connect con **JSON**: unario = `POST` con `application/json`; streaming de
servidor = `application/connect+json`, con sobres de 5 bytes (1 de flags + 4 de longitud,
big-endian) y un sobre final con flag `0x02` con `{}` o `{"error":{"code","message"}}`.
Un cliente que pida protobuf binario (`application/proto`, `application/connect+proto`)
recibe `415` con un error Connect `unimplemented` que lo explica. Los sobres comprimidos
se rechazan.

## Operaciones

### Plano de control

| Operación E2B | Estado | Traducción / motivo |
|---|---|---|
| `POST /sandboxes`, `POST /v2/sandboxes` | Soportada | `POST /v1/sandboxes`: `templateID`→`template`, `timeout`→`ttl_seconds`, `metadata`→`labels`, `envVars`→`env` (la sesión `default`: la ven los comandos sin sesión), `allow_internet_access:false`→`egress:none`. Se ignoran `autoPause`, `network`, `volumeMounts`, `mcp`, `secure`. |
| `GET /sandboxes`, `GET /v2/sandboxes` | Soportada | `GET /v1/sandboxes`, con el filtro `metadata`. Sin paginación: devuelve todo. |
| `GET /sandboxes/{id}` | Soportada | `GET /v1/sandboxes/{id}`. `diskSizeMB` es 0. |
| `DELETE /sandboxes/{id}` | Soportada | `DELETE /v1/sandboxes/{id}`. |
| `POST /sandboxes/{id}/timeout` | Soportada | `POST …/renew` con `ttl_seconds`. |
| `POST /sandboxes/{id}/refreshes` | Soportada | `POST …/renew` (60 s si no hay `duration`). |
| `POST /sandboxes/{id}/connect`, `/v2/…` | Soportada | Devuelve el sandbox; si está en pausa lo reanuda (`POST …/resume`) y contesta 201, si no 200. Con `timeout`, lo renueva. |
| `POST /sandboxes/{id}/pause` | Soportada | `POST …/pause` del gateway, 204. La memoria se queda en el host: no es un snapshot persistente como el de E2B. |
| `POST /sandboxes/{id}/resume` | Soportada | `POST …/resume` del gateway (y `timeout` → `renew`), 201 con el sandbox. |
| `PUT /sandboxes/{id}/network` | Soportada (parcial) | `PATCH …/egress`: dominios en `allowOut` → `allowlist`; `allow_internet_access:false` o `denyOut:["0.0.0.0/0"]` → `none`; nada → `internet`. IPs o CIDRs en `allowOut`, otro `denyOut`, `rules` y `egressProxy` son **501**. |
| `GET /health` | Soportada | 200 (control) o 204 (envd). |
| `…/fork`, `…/snapshots`, `/snapshots` | **501** | El gateway no snapshotea sandboxes de inquilino. |
| `…/metrics`, `/sandboxes/metrics`, `…/logs`, `/teams/…` | **501** | El gateway no expone métricas ni logs por sandbox (solo `/v1/metrics` global). |
| `/templates`, `/v2/templates`, `/v3/templates`, builds, tags | **501** | Las plantillas se gestionan con `kling sbx template`, no con el flujo de builds de E2B. |
| `/nodes`, `/admin/…`, `/unstable/…` | **501** | Administración de la plataforma E2B; no aplica. |

### envd (por sandbox)

| Operación | Estado | Traducción / motivo |
|---|---|---|
| `process.Process/Start` | Soportada | `POST /v1/sandboxes/{id}/processes` y `GET …/output?follow=1` → eventos `start` (con el pid del sandbox), `data` (`stdout`/`stderr`, base64) y `end` (`exitCode`, `status: "exit status N"` o `"signal: killed"`). `cmd`+`args`→`cmd` (si `cmd` es `/bin/bash` o `bash`, que es lo que lanzan los SDK como `-l -c CMD`, el gateway recibe `/bin/sh -c '…' sh <args>`: usa `/bin/bash` si existe y, si no, `/bin/sh` con los mismos args sin `-l`, para imágenes sin bash; cualquier otro `cmd` va tal cual), `cwd`→`dir`, `envs`→`env`. El comando es un proceso de fondo del sandbox: **sigue vivo si el cliente cierra la conexión** (`commands.run(background=True)`). `Connect-Timeout-Ms` es el plazo de la conexión: al vencer, el stream acaba con `deadline_exceeded` y el comando sigue. Entregado el `end`, el proceso se borra. Con un gateway sin procesos de fondo se usa el exec atado a la petición (`POST …/exec`; ahí el comando muere con la conexión y `Connect-Timeout-Ms`→`timeout_seconds`). |
| `process.Process/List` | Soportada | Procesos de fondo en marcha en ese sandbox (`GET …/processes`), también los lanzados por otra conexión. La `tag` solo se conoce mientras el shim no se reinicie. |
| `process.Process/SendSignal` | Soportada | `POST …/processes/{pid}/signal` con `TERM` o `KILL`; el fin sale con 143 o 137. Solo `SIGTERM` y `SIGKILL`. |
| `process.Process/CloseStdin` | Soportada | No-op: los comandos arrancan con stdin vacío. |
| `process.Process/Start` con `pty` | **501** | El exec del gateway no tiene terminal; usa `kling sbx shell`. |
| `process.Process/SendInput`, `StreamInput`, `Update` | **501** | El gateway acepta stdin solo entero al arrancar, no en vivo. |
| `process.Process/Connect` | Soportada | Vuelve a un proceso ya lanzado (por `pid` o `tag`): `start`, su salida desde el principio (lo que conserve el sandbox) y `end`. Un proceso ya terminado y entregado da `not_found`. |
| `filesystem.Filesystem/Stat` | Soportada | `GET …/files?stat=1`. |
| `…/ListDir` | Soportada | `GET …/files?list=1` del gateway (baja por los subdirectorios hasta `depth`); si el gateway no lo tiene o el listado viene truncado, cae a `find -printf` en el sandbox (necesita GNU findutils, no busybox). |
| `…/MakeDir`, `…/Remove`, `…/Move` | Soportada | `mkdir -p`, `rm -rf`, `mv` por exec. `MakeDir` sobre un directorio existente da `already_exists`; `Remove` de `/` se rechaza. |
| `…/WatchDir`, `CreateWatcher`, `GetWatcherEvents`, `RemoveWatcher` | **501** | El gateway no tiene notificaciones de ficheros. |
| `GET /files?path=` | Soportada | `GET /v1/sandboxes/{id}/files`. Las rutas relativas cuelgan de `-home`. |
| `POST /files?path=` | Soportada | `PUT …/files?mkdir=1`; acepta `multipart/form-data` (campo `file`; sin `path`, cada parte va a la ruta de su `filename`, que es lo que manda `write_files`) y `application/octet-stream`. Tope de 64 MiB del gateway. Se ignoran `X-Metadata-*`, `username` y las firmas. |
| `POST /files/compose` | **501** | Sin equivalente en el gateway. |
| `GET /envs`, `POST /init`, `/health` | Soportada | Respuestas vacías. |

Todo lo no listado responde `404`; lo listado como **501** devuelve el error de E2B
(`{"code":501,"message":…}`) en el plano de control o un error Connect `unimplemented` en las RPC.

### Intérprete de código (puerto 49999)

Lo que llama `e2b_code_interpreter.Sandbox` (y lo que usan smolagents, CrewAI, Agno y
compañía a través de `run_code`), sobre el intérprete del gateway (`/v1/sandboxes/{id}/code`,
[sandbox-api.md](sandbox-api.md#intérprete-de-código)). La plantilla necesita `python3` (y
`bash`/`node` para esos lenguajes); `Sandbox.create()` sin plantilla pide `code-interpreter-v1`,
así que o se llama así una plantilla del gateway o se pasa la que se quiera.

| Operación | Estado | Traducción / motivo |
|---|---|---|
| `POST /execute` | Soportada (texto) | `POST …/code` con `timeout_seconds: 600` (el máximo del gateway; en E2B el plazo lo pone el cliente). Sin `context_id`, un contexto **persistente** por sandbox y lenguaje, como en E2B: las variables siguen entre llamadas. `language`: `python` (por defecto), `js`/`javascript`, `bash`; los demás (`r`, `java`, `ts`…) los rechaza el gateway. Salida NDJSON de E2B: `stdout`/`stderr`, `result` (`text`, `html`, `png`, `jpeg`, `svg`, `markdown`, `latex`, `json`, `pdf`, `javascript` según los tipos MIME que dé el gateway; otros, en `extra`), `error` (`name`, `value`, `traceback`) y `number_of_executions`. |
| `POST /execute` con `env_vars` | **501** | El intérprete del gateway no admite entorno por ejecución; las `envs` de `Sandbox.create` sí las ve. |
| Gráficos (`result.chart`) | No | El gateway no extrae los datos de las figuras: una figura de matplotlib llega como `png`, sin `chart`. |
| `POST /contexts`, `GET /contexts`, `DELETE /contexts/{id}` | Soportada | `…/code/contexts` del gateway (`cwd` relativo cuelga de `-home`). Topes del gateway: 8 contextos por sandbox. |
| `POST /contexts/{id}/restart` | **501** | El gateway no reinicia un contexto conservando el id: bórralo y crea otro. |

Si el cliente corta la conexión a mitad (el `timeout` de `run_code`, o cancelar), el shim
interrumpe la ejecución (`DELETE …/code/contexts/{cid}/execution`, SIGINT): el contexto sigue
vivo, como en E2B. Esto lo cubre un test de Go, no el SDK.

## Límites conocidos

- El usuario (`Authorization: Basic` o `username`) se ignora: los comandos y ficheros van con
  el usuario por defecto del sandbox.
- El `endAt` y el estado salen del gateway; `paused` solo aparece si el gateway lo informa.
- Los `Keepalive-Ping-Interval` del cliente no se atienden: el shim no emite eventos `keepalive`.

El mismo gateway habla también el protocolo de OpenSandbox: [opensandbox.md](opensandbox.md).

Ver también: [sandbox-api.md](sandbox-api.md) (la API nativa `/v1`, con más funciones: lotes,
volúmenes, inquilinos), los SDKs nativos ([Python](../sdks/python/README.md),
[TypeScript](../sdks/typescript/README.md)) y las [recetas](recetas/README.md).
