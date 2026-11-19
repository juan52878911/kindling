# Shim compatible con E2B

`kling sbx e2b` escucha en un puerto y traduce el protocolo de [E2B](https://e2b.dev)
a la API `/v1` del gateway de sandboxes ([sandbox-api.md](sandbox-api.md)). Un SDK de E2B
(Python o JS) puede así crear sandboxes, ejecutar comandos y mover ficheros contra kindling
cambiando solo variables de entorno.

La especificación que sigue el shim es la pública de E2B, copiada en
`ext/sandbox/internal/e2b/spec/` (plano de control `openapi.yml`; envd: `envd.yaml` y los
`.proto` de `process` y `filesystem`).

> **Estado.** Verificado con un test de protocolo que reproduce, byte a byte, las peticiones
> que la especificación describe para create → `Process/Start` → files write/read → kill, contra
> un gateway simulado. **No se ha probado con el SDK real** (la máquina de desarrollo no tiene
> red): la forma exacta de las peticiones del SDK (cabeceras, formato JSON de Connect) sale de
> la especificación y del protocolo Connect, no de ejecutar el SDK.

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

El shim enruta envd por el host `<puerto>-<sandboxID>.<dominio>` o, si no, por la cabecera
`E2b-Sandbox-Id`; en los dos casos exige `X-Access-Token` (el `envdAccessToken` de create).

**Pruebas locales (sin DNS ni TLS).** Con `E2B_API_URL` y `E2B_SANDBOX_URL` el SDK manda todo,
control y envd, al mismo listener, identificando el sandbox solo por cabeceras:

```sh
kling sbx e2b -listen 127.0.0.1:3000
export E2B_API_KEY=<token del inquilino>
export E2B_API_URL=http://127.0.0.1:3000 E2B_SANDBOX_URL=http://127.0.0.1:3000
```

**En serio (dominio + TLS).** Necesita DNS comodín hacia el shim y TLS que el cliente acepte.

```sh
export E2B_API_KEY=<token del inquilino>
export E2B_DOMAIN=e2b.example.test        # *.e2b.example.test → la máquina del shim
export E2B_API_URL=https://api.e2b.example.test
# Certificado propio: SSL_CERT_FILE=ca.pem (Python), NODE_EXTRA_CA_CERTS=ca.pem (JS)
```

**Modo debug (`E2B_DEBUG=true`).** Con él el SDK no llama al plano de control: usa el id fijo
`debug_sandbox_id`, no manda `X-Access-Token` y habla con `http://localhost:49983`. Se soporta
con una bandera explícita, sobre un sandbox del gateway que ya existe:

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
| `POST /sandboxes`, `POST /v2/sandboxes` | Soportada | `POST /v1/sandboxes`: `templateID`→`template`, `timeout`→`ttl_seconds`, `metadata`→`labels`, `allow_internet_access:false`→`egress:none`. Se ignoran `autoPause`, `network`, `envVars`, `volumeMounts`, `mcp`, `secure`. |
| `GET /sandboxes`, `GET /v2/sandboxes` | Soportada | `GET /v1/sandboxes`, con el filtro `metadata`. Sin paginación: devuelve todo. |
| `GET /sandboxes/{id}` | Soportada | `GET /v1/sandboxes/{id}`. `diskSizeMB` es 0. |
| `DELETE /sandboxes/{id}` | Soportada | `DELETE /v1/sandboxes/{id}`. |
| `POST /sandboxes/{id}/timeout` | Soportada | `POST …/renew` con `ttl_seconds`. |
| `POST /sandboxes/{id}/refreshes` | Soportada | `POST …/renew` (60 s si no hay `duration`). |
| `POST /sandboxes/{id}/connect`, `/v2/…` | Soportada | Devuelve el sandbox vivo; con `timeout`, lo renueva. No reanuda nada: no hay pausa. |
| `GET /health` | Soportada | 200 (control) o 204 (envd). |
| `…/pause`, `…/resume`, `…/fork`, `…/snapshots`, `/snapshots` | **501** | El gateway no pausa ni snapshotea sandboxes de inquilino. |
| `…/metrics`, `/sandboxes/metrics`, `…/logs`, `/teams/…` | **501** | El gateway no expone métricas ni logs por sandbox (solo `/v1/metrics` global). |
| `PUT …/network` | **501** | La red se fija al crear (`egress`); el gateway no la cambia en caliente. |
| `/templates`, `/v2/templates`, `/v3/templates`, builds, tags | **501** | Las plantillas se gestionan con `kling sbx template`, no con el flujo de builds de E2B. |
| `/nodes`, `/admin/…`, `/unstable/…` | **501** | Administración de la plataforma E2B; no aplica. |

### envd (por sandbox)

| Operación | Estado | Traducción / motivo |
|---|---|---|
| `process.Process/Start` | Soportada | `POST /v1/sandboxes/{id}/exec` (NDJSON) → eventos `start`, `data` (`stdout`/`stderr`, base64) y `end` (`exitCode`, `status: "exit status N"`). `cmd`+`args`→`cmd`, `cwd`→`dir`, `envs`→`env`; `Connect-Timeout-Ms`→`timeout_seconds`. |
| `process.Process/List` | Soportada | Procesos que el shim tiene en marcha en ese sandbox. |
| `process.Process/SendSignal` | Soportada | Corta el flujo de exec (el gateway mata el comando); `SIGTERM` y `SIGKILL` son equivalentes, el fin sale con 143 o 137. |
| `process.Process/CloseStdin` | Soportada | No-op: los comandos arrancan con stdin vacío. |
| `process.Process/Start` con `pty` | **501** | El exec del gateway no tiene terminal; usa `kling sbx shell`. |
| `process.Process/SendInput`, `StreamInput`, `Update` | **501** | El gateway acepta stdin solo entero al arrancar, no en vivo. |
| `process.Process/Connect` | **501** | Un comando vive lo que dure su flujo: no se puede reenganchar. Un comando en segundo plano de E2B (`background`) muere si el cliente cierra la conexión. |
| `filesystem.Filesystem/Stat` | Soportada | `GET …/files?stat=1`. |
| `…/ListDir` | Soportada | `find -printf` en el sandbox (necesita GNU findutils en la imagen). |
| `…/MakeDir`, `…/Remove`, `…/Move` | Soportada | `mkdir -p`, `rm -rf`, `mv` por exec. `MakeDir` sobre un directorio existente da `already_exists`; `Remove` de `/` se rechaza. |
| `…/WatchDir`, `CreateWatcher`, `GetWatcherEvents`, `RemoveWatcher` | **501** | El gateway no tiene notificaciones de ficheros. |
| `GET /files?path=` | Soportada | `GET /v1/sandboxes/{id}/files`. Las rutas relativas cuelgan de `-home`. |
| `POST /files?path=` | Soportada | `PUT …/files?mkdir=1`; acepta `multipart/form-data` (campo `file`) y `application/octet-stream`. Tope de 64 MiB del gateway. Se ignoran `X-Metadata-*`, `username` y las firmas. |
| `POST /files/compose` | **501** | Sin equivalente en el gateway. |
| `GET /envs`, `POST /init`, `/health` | Soportada | Respuestas vacías. |

Todo lo no listado responde `404`; lo listado como **501** devuelve el error de E2B
(`{"code":501,"message":…}`) en el plano de control o un error Connect `unimplemented` en las RPC.

## Límites conocidos

- El usuario (`Authorization: Basic` o `username`) se ignora: los comandos y ficheros van con
  el usuario por defecto del sandbox.
- El `endAt` y el estado salen del gateway; `paused` solo aparece si el gateway lo informa.
- Los `Keepalive-Ping-Interval` del cliente no se atienden: el shim no emite eventos `keepalive`.
