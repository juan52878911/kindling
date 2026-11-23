# Shim compatible con OpenSandbox

`kling sbx osb` sirve la API de [OpenSandbox](https://github.com/alibaba/OpenSandbox) (OSB)
**1.2.1** delante del gateway de sandboxes: el plano de control `sandbox-lifecycle` bajo `/v1` y
el execd de cada sandbox. Los SDK oficiales de OSB (Python, JavaScript, Java/Kotlin, Go, C#), su
MCP y sus ejemplos funcionan contra microVMs de kindling cambiando el dominio y la clave.

> **Lo que hay que configurar: el mapa imagen → plantilla.** OSB arranca contenedores desde una
> imagen (`image.uri`); kindling arranca microVMs desde una **plantilla** ya construida
> (`kling sbx template`). El shim traduce cada `image.uri` con `-image-map uri=plantilla`
> (repetible; se prueba la uri exacta y luego sin la etiqueta: `python:3.12` → `python`) y, si no
> está, con `-default-template`. Sin ninguna de las dos, crear con esa imagen es un **422** que
> lista las plantillas que hay. `templateId` en la petición elige la plantilla directamente.

Versión fijada: la spec es la de la etiqueta `release-1.2.1` (copiada en
`ext/sandbox/internal/osb/testdata/`, con su NOTICE) y el SDK de Python que se prueba es
`opensandbox==1.2.1`. OSB ya tiene 1.3.0-rc: lo que cambie ahí no está cubierto.

## Arranque

```bash
kling sbx osb -listen 127.0.0.1:8080 -gateway https://gateway:8090 \
  -image-map python:3.12=py -image-map node:22=node -default-template base
```

```python
from datetime import timedelta
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.sync.sandbox import SandboxSync

cfg = ConnectionConfigSync(domain="http://127.0.0.1:8080", api_key="<token del inquilino>")
sb = SandboxSync.create("python:3.12", connection_config=cfg, timeout=timedelta(minutes=10))
print(sb.commands.run("echo hola").logs.stdout[0].text)
sb.kill()
```

| Opción | Qué hace |
|---|---|
| `-listen A` | Dirección de la API de OSB (el `domain` del SDK). Por defecto `127.0.0.1:8080`. |
| `-gateway URL` (`-url`) | Gateway; por defecto `$KLING_SANDBOX_URL` o `sandbox.url`. `-ca` añade una CA. |
| `-image-map uri=plantilla` | Traducción de imágenes (repetible). |
| `-default-template T` | Plantilla de las imágenes sin traducción. |
| `-public host[:puerto]` | Lo que se anuncia en `endpoints/{port}`; por defecto el `Host` con el que llegó el SDK. |
| `-ingress-domain D` | El `-ingress-domain` del gateway: los puertos que no son de execd se reenvían ahí. Sin él, 501. |
| `-tls-cert F -tls-key F` | HTTPS (el SDK necesita entonces `protocol="https"` o un `domain` con `https://`). |

**Clave.** La `OPEN-SANDBOX-API-KEY` que acepta el shim es el token del inquilino del gateway,
salvo que se fije otra en `KLING_OSB_KEY` (así el SDK no lleva el token real). Sin clave válida,
`401` con `ErrorResponse{code, message}`. El shim nunca escribe la clave en sus logs.

## Ids y estados

El id de OSB es el id del gateway (`host/máquina`) en base32 minúsculas sin relleno, la misma
codificación que la entrada HTTP y el shim de E2B: no lleva barra y es estable sin estado.

| Gateway | OSB |
|---|---|
| `running` | `Running` |
| `paused`, `frozen` | `Paused` |
| `created` | `Pending` |
| `stopped` | `Terminated` |
| `failed` | `Failed` |
| otro | `Pending` |

`frozen` es un sandbox que el TTL congeló: se despierta solo al usarlo, pero para OSB está en
pausa.

## Plano de control (`/v1`)

| Operación OSB | Traducción |
|---|---|
| `POST /sandboxes` | `POST /v1/sandboxes`, **202** con `Location`. `image.uri` → plantilla (arriba); `timeout` (≥ 60) → `ttl_seconds`, `null` deja el TTL por defecto del gateway (no "sin caducidad"); `resourceLimits.cpu` (`500m`, `2`) → `vcpus` redondeando hacia arriba; `resourceLimits.memory` (`512Mi`, `2Gi`, `1G`) → `mem_mib`; `metadata` → etiquetas; `env` → `env`; `networkPolicy` → `egress` (ver red). Un `entrypoint` distinto de `tail -f /dev/null` se lanza como proceso de fondo tras crear (si falla, el sandbox se borra). `resourceRequests`, `secureAccess`, `image.auth` y el resto de `extensions` se ignoran. |
| `GET /sandboxes` | Recorre `GET /v1/sandboxes` por `limit`/`cursor` (páginas de 1000) y pagina con `page`/`pageSize` (20 por defecto, 1000 como mucho); `totalItems` es exacto. `metadata=k%3Dv` va al gateway como `label`; `state` (repetible) filtra por el estado de OSB. |
| `GET /sandboxes/{sandboxId}` | `GET /v1/sandboxes/{id}`. `image.uri` es la del mapa que lleva a su plantilla (o la plantilla); `entrypoint` es el que se pidió a este shim. |
| `DELETE /sandboxes/{sandboxId}` | `DELETE /v1/sandboxes/{id}`, 204. |
| `PATCH /sandboxes/{sandboxId}/metadata` | `PATCH /v1/sandboxes/{id}/metadata` (`null` quita la clave). Un gateway sin esa ruta da 501. |
| `POST /sandboxes/{sandboxId}/pause` | `POST …/pause`, 202. |
| `POST /sandboxes/{sandboxId}/resume` | `POST …/resume`, 202. |
| `POST /sandboxes/{sandboxId}/renew-expiration` | `expiresAt` absoluto → `POST …/renew` con los segundos que faltan; devuelve el `expiresAt` del gateway. En el pasado, 400. |
| `GET /sandboxes/{sandboxId}/endpoints/{port}` | `{endpoint: "<host del shim>/sandboxes/<id>/port/<port>", headers: {X-EXECD-ACCESS-TOKEN}}`, sin esquema, como el servidor de OSB. `expires` (endpoints firmados) es 501. |
| `GET/PUT/PATCH/DELETE /sandboxes/{sandboxId}/networkpolicy` | Lee la política del sandbox y la cambia con `PATCH …/egress` (ver red). |
| `POST /metrics/events` | Las métricas de cliente del SDK se aceptan (204) y se tiran. |

Errores: `code` sigue la forma de OSB (`SANDBOX::NOT_FOUND`, `SANDBOX::INVALID_PARAMETER`,
`SANDBOX::NOT_IMPLEMENTED`, `SANDBOX::UNPROCESSABLE`…) y el `message` lleva el del gateway con su
code entre paréntesis.

### Red

Solo se traduce lo que el gateway sabe aplicar: **reglas `allow` por dominio**, sin puertos.

| Política OSB | Gateway |
|---|---|
| `defaultAction: deny` (o ausente) sin reglas | `egress: none` |
| `defaultAction: deny` + reglas `allow` por dominio | `egress: allowlist` con esos dominios |
| `defaultAction: allow` | `egress: internet` |
| Reglas `deny`, targets IP/CIDR o `ports` | **501** con el motivo |

`PATCH` añade reglas (o sustituye la del mismo target) y `DELETE` quita targets; los dos parten
de la política actual del sandbox. El sidecar de egress del SDK (puerto 18080, `GET/PATCH/DELETE
/policy`) se sirve con la misma traducción, así que `get_egress_policy` y `patch_egress_rules`
funcionan.

## execd (puerto 44772)

El SDK habla con `<host del shim>/sandboxes/<id>/port/44772/...` con la cabecera
`X-EXECD-ACCESS-TOKEN` que devolvió `endpoints`: un HMAC del id con la clave, sin estado, que no
vale para otro sandbox. También vale la `OPEN-SANDBOX-API-KEY`.

| Operación execd | Traducción |
|---|---|
| `GET /ping` | 200 si el sandbox existe y está `running`/`frozen`; 503 si está en pausa. |
| `POST /command` | `POST …/exec` (NDJSON) → **SSE evento a evento**: `init` (id), `stdout`, `stderr` (sin partir runas UTF-8), `execution_complete` con `exit_code: 0`, o `error` `CommandExecError` con `exit_code` si sale distinto de 0, como el execd real. `command` corre con `sh -c`; `argv` va tal cual; `cwd`, `envs`, `timeout` (ms → s, hacia arriba) y `uid`/`gid` (→ `user`). Un evento `ping` cada 3 s en un comando callado. Con `background: true`, `POST …/processes` y el id es `p<pid>`. |
| `DELETE /command?id=` | Primer plano: corta el exec. Fondo: `POST …/processes/{pid}/signal` (TERM). |
| `GET /command/status/{id}` | Fondo: `GET …/processes/{pid}`. Primer plano: lo que el shim recuerda (los últimos 256). |
| `GET /command/{id}/logs` | La salida desde la línea `cursor`, con `EXECD-COMMANDS-TAIL-CURSOR`. Fondo: `…/processes/{pid}/output`; primer plano: el primer MiB guardado. |
| `POST /session`, `POST /session/{sessionId}/run`, `DELETE /session/{sessionId}` | Sesiones del gateway (`POST …/sessions` con id `osb-…` y `cwd`); `run` es un exec con `session`. |
| `POST /code/context`, `GET/DELETE /code/contexts`, `GET/DELETE /code/contexts/{context_id}` | `…/code/contexts` del gateway (Python, bash y JavaScript). `DELETE /code/contexts?language=` borra los de ese lenguaje. |
| `POST /code` | `POST …/code` → SSE: `init`, `stdout`/`stderr`, `execution_count` + `result` (`text/plain` pasa a `text`, como execd), `error` (`ename`, `evalue`, `traceback`) y `execution_complete`. Sin `context.id` usa un contexto por defecto por sandbox y lenguaje que persiste entre llamadas. |
| `DELETE /code?id=` | `DELETE …/code/contexts/{id}/execution` (SIGINT). |
| `GET /files/info` | `files?stat=1` por cada `path`. `owner`/`group` vacíos y `created_at` = `modified_at` (el gateway no los da). `mode` en dígitos octales (`644`). |
| `POST /files/upload` | El multipart del SDK (`metadata` + `file`) sube cada fichero en streaming con `PUT files?mode=&mkdir=1`; `owner`/`group` con `chown`. |
| `GET /files/download` | Entero, con `Range` (un rango: `a-b`, `a-`, `-n` → 206 / 416) o por líneas (`offset`/`limit`, 1-based). |
| `POST /files/mv`, `GET /files/search`, `DELETE /files` | `files/move`, `files/search` (el `pattern` se aplica al nombre, como `find -name`; hasta 1000) y `DELETE files`. |
| `POST /files/permissions` | `chmod`/`chown` por exec, sin shell. |
| `POST /files/replace` | Lee, sustituye y escribe conservando el modo (hasta 64 MiB); devuelve `replacedCount`. |
| `GET /directories/list`, `POST /directories`, `DELETE /directories` | `files?list=1` (con `depth`, hasta 16 y 10 000 entradas), `files/mkdir` + `chmod`, y `rm -rf` sin shell (nunca `/`). |
| `GET /metrics` | Lee `/proc` del invitado (dos muestras de `/proc/stat` a 200 ms). |

**Otros puertos.** `endpoints/{port}` de cualquier otro puerto apunta también al shim; con
`-ingress-domain` el shim lo reenvía a la entrada HTTP del gateway con `Host:
<puerto>-<id>.<dominio>` y el token del inquilino (el invitado no ve ni el token de execd ni la
clave). Sin `-ingress-domain`, 501.

## Operaciones que contestan 501

Lo que la spec 1.2.1 tiene y el gateway no: el shim lo reconoce y contesta 501 con `code` y motivo.
`ext/sandbox/internal/osb/contrato_test.go` comprueba que esta lista y `NoImplementadas` coinciden.

| Operaciones | Motivo |
|---|---|
| `POST /sandboxes/{sandboxId}/credential-vault`, `GET /sandboxes/{sandboxId}/credential-vault`, `PATCH /sandboxes/{sandboxId}/credential-vault`, `DELETE /sandboxes/{sandboxId}/credential-vault`, `GET /sandboxes/{sandboxId}/credential-vault/credentials`, `GET /sandboxes/{sandboxId}/credential-vault/credentials/{credentialName}`, `GET /sandboxes/{sandboxId}/credential-vault/bindings`, `GET /sandboxes/{sandboxId}/credential-vault/bindings/{bindingName}` | Sin bóveda de credenciales: secretos por `env` o sesiones. |
| `GET /snapshots`, `GET /snapshots/{snapshotId}`, `DELETE /snapshots/{snapshotId}`, `POST /sandboxes/{sandboxId}/snapshots` | El gateway no expone snapshots por sandbox: se construye una plantilla. |
| `POST /templates`, `GET /templates`, `GET /templates/{templateId}`, `DELETE /templates/{templateId}` | Las plantillas de kindling se construyen con `kling sbx template` y se eligen con `-image-map`. |
| `GET /sandboxes/{sandboxId}/diagnostics/context`, `GET /sandboxes/{sandboxId}/diagnostics/logs`, `GET /sandboxes/{sandboxId}/diagnostics/events` | Sin API de diagnóstico (spec `diagnostic-api`, fuera de las dos copiadas). |
| `GET /metrics/watch` | Sin métricas en streaming: se consulta `GET /metrics`. |
| `GET /v1/filesystem/{uid}/{gid}/files/info`, `DELETE /v1/filesystem/{uid}/{gid}/files`, `POST /v1/filesystem/{uid}/{gid}/files/permissions`, `POST /v1/filesystem/{uid}/{gid}/files/mv`, `GET /v1/filesystem/{uid}/{gid}/files/search`, `POST /v1/filesystem/{uid}/{gid}/files/replace`, `POST /v1/filesystem/{uid}/{gid}/files/upload`, `GET /v1/filesystem/{uid}/{gid}/files/download`, `GET /v1/filesystem/{uid}/{gid}/directories/list`, `POST /v1/filesystem/{uid}/{gid}/directories`, `DELETE /v1/filesystem/{uid}/{gid}/directories` | El gateway no opera ficheros como otro uid/gid. |
| `POST /v1/isolated/session`, `GET /v1/isolated/sessions`, `GET /v1/isolated/capabilities`, `GET /v1/isolated/session/{sessionId}`, `DELETE /v1/isolated/session/{sessionId}`, `POST /v1/isolated/session/{sessionId}/run`, `GET /v1/isolated/session/{sessionId}/runs/{runId}`, `GET /v1/isolated/session/{sessionId}/runs/{runId}/logs`, `GET /v1/isolated/session/{sessionId}/diff`, `POST /v1/isolated/session/{sessionId}/commit`, `GET /v1/isolated/session/{sessionId}/files/info`, `GET /v1/isolated/session/{sessionId}/files/download`, `POST /v1/isolated/session/{sessionId}/files/upload`, `DELETE /v1/isolated/session/{sessionId}/files`, `POST /v1/isolated/session/{sessionId}/files/mv`, `POST /v1/isolated/session/{sessionId}/files/permissions`, `POST /v1/isolated/session/{sessionId}/files/replace`, `GET /v1/isolated/session/{sessionId}/files/search`, `GET /v1/isolated/session/{sessionId}/directories/list`, `POST /v1/isolated/session/{sessionId}/directories`, `DELETE /v1/isolated/session/{sessionId}/directories` | Sin sesiones aisladas: cada sandbox de kindling ya es su propia microVM. |

También son 501 en `POST /sandboxes`: `snapshotId`, `extensions.poolRef`, `platform.os` distinto
de `linux`, cualquier `resourceLimits` de GPU, `volumes`, `credentialProxy` y `lifecycle`.

De las 90 operaciones de las dos specs, **41 están implementadas** (14 del plano de control y 27
de execd) y 49 contestan 501.

## Cómo se verifica

- `ext/sandbox/internal/osb`: traducción contra un gateway falso (`httptest`) y el **contrato**,
  que recorre las 90 operaciones de las specs copiadas.
- `ext/sandbox/internal/frontal/contrato_osb_test.go`: el **SDK real** (`opensandbox==1.2.1`,
  `scripts/osb-sdk-real/osb_real.py`) contra este shim delante del frontal real y su daemon
  falso, que ejecuta órdenes y ficheros en la máquina del test: crear, `commands.run` (salida 0 y
  3), escribir y leer, info/replace/mv/search/list/delete de ficheros, comando de fondo con estado
  y logs, sesión, listado, política de red, renovar, pausar, reanudar y borrar. Corre en el job
  `osb` de `.github/workflows/sdks.yml`; en local se salta si `python3` (o `KLING_OSB_PYTHON`) no
  tiene el paquete.
- Contra un gateway vivo: `scripts/osb-sdk-real/README.md`.

Ver también: [e2b.md](e2b.md) (el mismo gateway con el protocolo de E2B) y
[sandbox-api.md](sandbox-api.md) (la API `/v1` que hay debajo).
