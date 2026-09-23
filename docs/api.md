# API del daemon

El daemon escucha en un socket Unix (`/run/kling.sock` por defecto en Linux,
`~/Library/Application Support/kindling/kling.sock` en macOS) y nunca en un
puerto de red: controlar microVMs equivale a root en su host, así que el único
acceso remoto es SSH (`kling dial-stdio` al otro lado). El cliente Go es
`pkg/api.Client`; todo `kling` —y cualquier extensión— habla con el daemon por
aquí.

Los errores vuelven como `{"message": "..."}` con el código HTTP; el cliente los
convierte en `*api.StatusError`. `507` significa "no cabe en memoria" y es lo que
el planificador usa para desalojar y reintentar.

## Capacidades

`GET /info` incluye `capabilities`. Una extensión comprueba ahí lo que necesita en
vez de deducirlo de la versión. Un daemon anterior no envía la lista.

| Capacidad | Desde | Rutas |
|---|---|---|
| `annotations` | v0.5 | `GET /snapshots/{name}`, `PUT/DELETE /snapshots/{name}/annotations/{key}` |
| `store` | v0.5 | `GET /store/{ns}`, `GET/PUT/DELETE /store/{ns}/{key}` |
| `builders` | v0.5 | `POST /images` con `builder` y `spec` |
| `image-files` | v0.5 | `GET/PUT /images/{name}/files` |
| `resize` | v0.8 | `POST /machines/{ref}/resize`, `mem_max_mib` en `POST /machines` |
| `shell` | v0.7 | `POST /machines/{ref}/shell` (protocolo `kling-shell/1`) |
| `exec` | v0.7 | `POST /machines/{ref}/exec`, `GET/PUT/DELETE /machines/{ref}/files`, `allow_exec` y `on_ttl` en `POST /machines` |
| `sandboxes` | v0.7 | `POST/GET /sandboxes`, `GET/DELETE /sandboxes/{ref}`, `POST /sandboxes/{ref}/renew` |
| `image-blobs` | v0.9 | `GET/HEAD/PUT /images/{name}/blob` |

## Rutas

### Daemon

| Ruta | Qué hace |
|---|---|
| `GET /info` | versión, raíz, KVM, máquinas, versión del VMM (`firecracker`, por historia, también con `vz`), capacidades, `backend` (`firecracker` o `vz`, desde v0.9) y `arch` (GOARCH del host) |
| `GET /events` | flujo NDJSON de eventos (`machine.*`, `snapshot.committed`, `snapshot.annotated`, `store.updated`), con latido cada 30 s |
| `GET /metrics` | métricas Prometheus en texto |
| `GET /procstats` | memoria por microVM (PSS) y del host, en JSON |

### Máquinas

| Ruta | Qué hace |
|---|---|
| `GET /machines` | lista |
| `POST /machines` | crea y arranca (`RunRequest`: imagen o `from` un snapshot, vCPUs, memoria, egress y dominios, TTL, techo de CPU, volúmenes, etiquetas) |
| `GET /machines/{ref}` | una máquina |
| `POST /machines/{ref}/freeze` · `/thaw` · `/stop` | ciclo de vida |
| `POST /machines/{ref}/squeeze` | el globo devuelve al host la memoria libre del invitado |
| `POST /machines/{ref}/mmds` | secretos de sesión por MMDS (≤1 MiB); la máquina deja de poder congelarse |
| `PUT /machines/{ref}/labels` | reetiqueta |
| `POST /machines/{ref}/commit` | congela la máquina como snapshot reutilizable |
| `GET /machines/{ref}/logs?tail=N` | consola serie |
| `POST /machines/{ref}/guest` | reenvía una petición HTTP al invitado (ver abajo) |
| `DELETE /machines/{ref}` | la borra |

### Snapshots

| Ruta | Qué hace |
|---|---|
| `GET /snapshots` | lista |
| `GET /snapshots/{name}` | uno, con disco e instancias vivas |
| `PUT /snapshots/{name}/annotations/{key}` | guarda JSON opaco (≤1 MiB, ≤32 claves, clave `^[a-z0-9][a-z0-9._-]{0,63}$`) |
| `DELETE /snapshots/{name}/annotations/{key}` | lo borra |
| `DELETE /snapshots/{name}` | borra el snapshot |

### Store

Estado de extensiones en el host del daemon: `$KLING_ROOT/store/<ns>/<key>.json`,
JSON opaco de hasta 1 MiB. Mismas reglas de nombre que las anotaciones.

| Ruta | Qué hace |
|---|---|
| `GET /store/{ns}` | `{"keys": [...]}` |
| `GET /store/{ns}/{key}` | el valor, o 404 |
| `PUT /store/{ns}/{key}` | lo guarda (durable) |
| `DELETE /store/{ns}/{key}` | lo borra; no es error que no existiera |

### Imágenes

| Ruta | Qué hace |
|---|---|
| `GET /images` | lista, con receta y snapshots que salen de cada una |
| `POST /images` | construye. Con `builder`, lo hace el ejecutable de root `/usr/local/lib/kindling/builders/<builder>` (o `$KLING_BUILDERS_DIR`) con el `spec` de la petición. `builder` es obligatorio desde v0.6: el núcleo trae `base` y kindling-mcp instala `mcp` |
| `GET /images/{name}/recipe` | cómo se construyó |
| `GET /images/{name}/files?path=/p[&max=N]` | el contenido de un fichero de dentro (1 MiB por defecto, hasta 64) |
| `GET /images/{name}/files?path=/p&stat=1` | `{exists, size, sha256}` |
| `PUT /images/{name}/files` | pone un fichero dentro (`path`, `mode`, `content_b64` hasta 8 MiB o `from_host` relativo a `/usr/local/lib/kindling`, `create`); se niega si la imagen está en uso |
| `DELETE /images/{name}` | la borra si nada la usa |
| `GET /images/{name}/blob[?part=P]` | el fichero de la imagen, en flujo, con `Content-Length`, `X-Kling-Sha256` y `X-Kling-Part`. Sin `part`, el ext4 de una monolítica o la capa de una por capas. `HEAD` da las mismas cabeceras sin cuerpo |
| `PUT /images/{name}/blob?part=P` | recibe una parte en flujo (hasta 16 GiB): temporal, sha256 comprobado si llega `X-Kling-Sha256`, renombrado atómico. `201` si la escribe, `200` con `unchanged` si ya había una idéntica, `409` si la imagen está en uso y el contenido es distinto |

**Partes de un blob.** `part` es `image` (`<name>.ext4`), `layer`
(`<name>.layer.ext4`) o `recipe` (`<name>.recipe.json`). El nombre `vmlinux` está
reservado para el kernel compartido (`part` vacía o `kernel`). "En uso" es lo mismo
que impide borrarla: un dorado o una máquina que no esté parada que la usen, o
capas encima; para el kernel, cualquier máquina que no esté parada. La receta de
una imagen por capas cuenta como la imagen, porque decide su base. Es lo que usa
`kling images copy` (`api.CopyImage`) para llevar una imagen de un daemon Linux a
uno de macOS, donde `POST /images` contesta `501`.

**El protocolo del constructor.** El daemon crea un directorio de trabajo, deja
en él `request.json` y ejecuta `<constructor> <dir>` con `KLING_ROOT`,
`KLING_IMAGE_NAME`, `KLING_BUILD_DIR` y, si se pidieron, `BASE_IMAGE` y `GROW`. El
constructor tiene que dejar `$KLING_ROOT/images/<name>.ext4` o
`<name>.layer.ext4` y salir con 0; su salida vuelve a quien pidió la construcción.
Tiene que ser de root y nadie más puede escribirlo, ni a él ni a su directorio,
porque el daemon lo ejecuta como root. La receta la escribe el daemon, con
permisos `0600` porque el spec puede llevar secretos.

### Volúmenes

| Ruta | Qué hace |
|---|---|
| `GET /volumes` | lista |
| `POST /volumes` | crea (`name`, `size_mib`) |
| `POST /volumes/{name}/populate` | instala paquetes dentro con una microVM de un solo uso |
| `DELETE /volumes/{name}` | lo borra si nada lo usa (409 si no) |

### `POST /machines/{ref}/resize`

`{"mem_mib": 1024}` cambia la memoria de una máquina en marcha sin reiniciarla,
entre 128 MiB y el techo con el que arrancó (`mem_max_mib` en `POST /machines`).
La máquina arranca con el techo y el globo retiene la diferencia; subir es
desinflarlo y bajar, inflarlo. Una máquina sin techo tiene la memoria fija (400).
El techo queda en el snapshot al hacer commit.

### Admisión

`POST /machines` y `POST /sandboxes` rechazan antes de arrancar:

| Código | Causa |
|---|---|
| `507` | no cabe en memoria, o el host está bajo presión (PSI `some avg10` por encima de `KLING_MAX_MEM_PRESSURE`, 20 % por defecto) |
| `503` | queda menos disco que `KLING_MIN_FREE_DISK_MIB` (2 GiB) bajo `$KLING_ROOT`. No es un 507 a propósito: quien recibe un 507 congela para hacer sitio, y congelar escribe en disco |
| `409` | tope de máquinas del daemon (`KLING_MAX_MACHINES`, 256) |

### El proxy y los puertos

`POST /machines/{ref}/guest` solo llega al puerto 8080 del invitado, salvo los
que la máquina declare en la etiqueta `kling.ports` (lista separada por comas),
que un snapshot hereda. Otro puerto es `403`.

## Exec y ficheros

Solo en máquinas creadas con `allow_exec` (`403` si no). Una máquina congelada se
descongela; una recién arrancada se espera hasta 30 s a que su agente escuche.
Guía de uso: [`exec-sandbox.md`](exec-sandbox.md).

### `POST /machines/{ref}/exec`

```json
{
  "cmd": ["sh", "-c", "echo hola; exit 3"],
  "dir": "/tmp",
  "env": ["FOO=bar"],
  "stdin": "aG9sYQo=",
  "timeout_seconds": 300,
  "max_output_bytes": 8388608
}
```

`cmd` es argv, sin shell. `stdin` va en base64, hasta 1 MiB. Plazo por defecto
300 s (máximo 3600); salida por defecto 8 MiB **por flujo** (máximo 64 MiB).

La respuesta es `application/x-ndjson`, una línea por evento según ocurren:

```json
{"stream":"stdout","data":"aG9sYQo="}
{"stream":"stderr","data":"..."}
{"exit":3,"duration_ms":12}
```

El último evento es siempre `exit` (con `truncated` si se cortó la salida y
`timed_out` si lo mató el plazo, código 137) o `error` (no se pudo ejecutar, o el
invitado dejó de contestar). Un código distinto de cero no es un error HTTP. Con
`?wait=1` la respuesta es un único objeto: `exit_code`, `stdout`, `stderr` (base64),
`duration_ms`, `truncated`, `timed_out`.

El daemon no se fía del invitado: recorta cada flujo al tope pedido y garantiza que
hay exactamente un evento final. Cerrar la conexión mata el comando.

### `/machines/{ref}/files?path=/ruta/absoluta`

| Método | Qué hace |
|---|---|
| `GET` | el contenido, en crudo (hasta 256 MiB) |
| `GET` con `&stat=1` | `{path, size, mode, is_dir, mod_time}` |
| `PUT` con `&mode=0755` y opcional `&mkdir=1` | escribe el cuerpo (hasta 64 MiB), atómicamente |
| `DELETE` | borra un fichero o un directorio vacío |

El último componente de la ruta no se sigue si es un enlace simbólico. `501`
significa que el agente de la imagen es anterior a v0.7.

### `POST /machines/{ref}/shell`

La shell interactiva. No es una respuesta en streaming: la petición pide
`Connection: Upgrade` y `Upgrade: kling-shell/1`, el daemon contesta `101` y a
partir de ahí la conexión transporta tramas binarias en los dos sentidos. Sin las
cabeceras de upgrade, `426`.

El cuerpo es `{"cmd": [...], "dir": "...", "env": [...], "term": "xterm-256color",
"rows": 40, "cols": 120}`; todo opcional (sin `cmd`, `/bin/sh -l`).

Cada trama es `tipo (1 byte) | longitud (4 bytes, big-endian) | carga`, con la
carga acotada a 64 KiB:

| Tipo | Sentido | Carga |
|---|---|---|
| `0` data | los dos | bytes de entrada, o salida del pseudoterminal |
| `1` resize | hacia dentro | `rows uint16`, `cols uint16` |
| `2` signal | hacia dentro | número de señal, al grupo en primer plano |
| `3` exit | hacia fuera | código `int32`; siempre la última |
| `4` error | hacia fuera | texto; también la última |
| `5` ping | hacia fuera | vacía, cada 30 s: detecta clientes muertos |

El daemon no reenvía a ciegas: comprueba que el tipo tiene sentido en esa
dirección y descarta lo demás. Un invitado no puede mandarle al cliente tramas de
redimensionado ni de señal.

## Sandboxes

`POST /sandboxes` crea una máquina con `allow_exec`, egress `none` por defecto,
`on_ttl: remove` y la etiqueta `kind=sandbox`, y contesta cuando su agente ya
escucha:

```json
{"image": "toolchain", "ttl_seconds": 600, "egress": "none", "mem_mib": 512}
```

`image` o `from` (un snapshot hecho con `allow_exec`; si no lo es, `409`), no los
dos. TTL por defecto 600 s, máximo 86400. `on_ttl` decide qué pasa al vencer:
`remove` (por defecto) lo destruye y `freeze` lo duerme a coste cero, con el
siguiente exec despertándolo; con `freeze` el TTL cuenta INACTIVIDAD y usar el
sandbox lo reinicia. También admite `name`, `vcpus`,
`cpu_pct`, `allow_domains`, `volumes` y `labels`.

| Ruta | Qué hace |
|---|---|
| `GET /sandboxes` | los vivos |
| `GET /sandboxes/{ref}` | uno |
| `POST /sandboxes/{ref}/renew` | `{"ttl_seconds": N}`: vence N segundos a partir de ahora |
| `DELETE /sandboxes/{ref}` | lo destruye |

Estas rutas solo tocan máquinas con `kind=sandbox`.

### `allow_exec` y `on_ttl` en `POST /machines`

`allow_exec: true` enciende exec y ficheros. Con `from`, las máquinas heredan la
del snapshot, y pedirla sobre uno que no la tiene es `409`. `on_ttl` decide qué pasa
al vencer `ttl_seconds`: `freeze` (por defecto) o `remove`. `commit` graba
`allow_exec` en el snapshot.

## El proxy al invitado

Las IP de los invitados solo existen en la red del host, así que un cliente
remoto habla con ellos a través de `POST /machines/{ref}/guest`:

```json
{
  "port": 8080,
  "path": "/mcp",
  "method": "POST",
  "body": "{...}",
  "headers": {"Accept": "application/json, text/event-stream"},
  "response_headers": ["Mcp-Session-Id", "Content-Type"],
  "max_body_bytes": 8388608,
  "wait_ms": 20000,
  "probe_only": false
}
```

El daemon no añade nada de ningún protocolo: ruta, cabeceras de ida y cuáles
devolver los decide quien llama. La respuesta del invitado se lee entera (8 MiB
por defecto, hasta 64) y **falla** si se pasa, en vez de truncarse. `wait_ms`
espera a que el puerto abra; `probe_only` se conforma con eso.

`path` es obligatorio salvo con `probe_only`: una petición sin él (la de un
cliente v0.4, que contaba con los valores de MCP que el daemon ponía entonces)
recibe `400`.

## Rutas retiradas en v0.6

v0.5 las mantenía como alias; v0.6 las quita. Lo que hacían lo hace ahora
kindling-mcp sobre las rutas genéricas:

| Ruta de v0.4/v0.5 | Ahora |
|---|---|
| `PUT /snapshots/{name}/catalog` | anotación `mcp.tools` (`PUT /snapshots/{name}/annotations/mcp.tools`) |
| `PUT /snapshots/{name}/health` | anotación `mcp.health` |
| `GET/PUT /links`, `DELETE /links/{name}` | `store/mcp/links` |
| `POST /images/refresh-bridge` | `PUT /images/{name}/files` con `from_host: "kling-bridge"` (`kling mcp refresh-bridge`) |
| `GET /images/{name}/capabilities` | `GET /images/{name}/files?path=/etc/kling/capabilities.json` |
| `POST /images` sin `builder` | `builder: "mcp"` |

El snapshot ya no devuelve `tools`, `tools_at`, `health`, `health_at` ni
`health_err`. Los datos no se pierden: al leer un `meta.json` de v0.4, el daemon
los pasa a las anotaciones `mcp.tools` y `mcp.health`, y al arrancar migra un
`links.json` que quede a `store/mcp/links` (dejando el original como
`links.json.migrated`).
