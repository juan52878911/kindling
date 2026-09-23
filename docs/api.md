# API del daemon

El daemon escucha en un socket Unix (`/run/kling.sock` por defecto) y nunca en un
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

## Rutas

### Daemon

| Ruta | Qué hace |
|---|---|
| `GET /info` | versión, raíz, KVM, máquinas, firecracker, capacidades |
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
| `POST /images` | construye. Con `builder`, lo hace el ejecutable de root `/usr/local/lib/kindling/builders/<builder>` (o `$KLING_BUILDERS_DIR`) con el `spec` de la petición; sin `builder`, el empaquetado de servidores MCP de siempre |
| `GET /images/{name}/recipe` | cómo se construyó |
| `GET /images/{name}/files?path=/p[&max=N]` | el contenido de un fichero de dentro (1 MiB por defecto, hasta 64) |
| `GET /images/{name}/files?path=/p&stat=1` | `{exists, size, sha256}` |
| `PUT /images/{name}/files` | pone un fichero dentro (`path`, `mode`, `content_b64` hasta 8 MiB o `from_host` relativo a `/usr/local/lib/kindling`, `create`); se niega si la imagen está en uso |
| `DELETE /images/{name}` | la borra si nada la usa |

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

**Compatibilidad (v0.5).** Una petición sin `path` es de un cliente v0.4 y recibe
los valores de entonces: `/mcp`, la cabecera `Accept` de MCP y `Mcp-Session-Id`
de vuelta. Se retira en v0.6.

## Rutas de v0.4 que se mantienen hasta v0.6

| Ruta | Ahora |
|---|---|
| `PUT /snapshots/{name}/catalog` | escribe la anotación `mcp.tools` |
| `PUT /snapshots/{name}/health` | escribe la anotación `mcp.health` |
| `GET/PUT /links`, `DELETE /links/{name}` | leen y escriben `store/mcp/links`; al arrancar, el daemon migra el `links.json` antiguo y lo deja como `links.json.migrated` |
| `POST /images/refresh-bridge` | pone el puente al día en las imágenes (lo mismo que `PUT /images/{name}/files` con `from_host`) |
| `GET /images/{name}/capabilities` | lee `/etc/kling/capabilities.json` de la imagen |

Mientras duren, el snapshot sigue devolviendo `tools`, `tools_at`, `health`,
`health_at` y `health_err`, rellenados a partir de las anotaciones, para que un
CLI anterior los lea y bajar de versión el daemon no pierda nada.
