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
| `guest-resync` | v0.9.1 | ninguna: el daemon resincroniza reloj y entropía del invitado tras cada restauración (ver abajo) |
| `shares-copy` | v0.10 | `POST /shares/uploads`, `shares` con `mode: copy` en `POST /machines` y `POST /sandboxes` |
| `shares-live` | v0.10 | `shares` con `mode: ro\|rw` (directorio del host del daemon, bajo `share_roots`); `share_roots` en `GET /info` |
| `renew` | v0.11 | `POST /machines/{ref}/renew` |
| `pause` | v0.12 | `POST /machines/{ref}/pause` |
| `fork` | v0.15 | `POST /sandboxes/{ref}/fork` |
| `credaudit` | v0.17 | `GET /machines/{ref}/credaudit` |
| `db-attach` | v0.17 | `upstream_machine` y `upstream_owner` en las credenciales postgres de `POST /machines/{ref}/credentials` (Linux y macOS), `DELETE /machines/{ref}/credentials/{env}` |
| `graphs` | v0.17 | `POST/GET /graphs`, `GET/DELETE /graphs/{ref}`, `POST /graphs/{ref}/freeze\|thaw\|snapshot\|fork`; `PUT/DELETE /store/graph/*` reservados (403) |
| `authz` | v0.17 | `authz` en `GET /info`; con una política ([authz.md](authz.md)) cada ruta se autoriza por quien llama: `403` sin rol o fuera de lo suyo, `404` sobre lo ajeno, `401` con un token inválido |
| `cow-grow` | v0.17 | `POST /cow/store/grow` (ver [cow.md](cow.md#hacer-crecer-el-almacén)) |
| `ready` | v0.17 | `GET /machines/{ref}/ready`, `POST /machines/{ref}/hooks`, `wait_ready` en `POST /machines` y `POST /sandboxes`, `skip_ready` en commit y fork, `?force=1` en squeeze, `cpu_pct_default` en `POST /machines` (ver "Listo y ganchos tras restaurar") |
| `machine-env` | sin publicar | `env` en `POST /machines` (`["KEY=valor"]`, ≤ 256, ≤ 32 KiB): el entorno de la máquina, por MMDS al invitado; solo en frío (con `from`, 400). La máquina enseña solo `env_keys`, y un snapshot los hereda (ver [imagenes.md](imagenes.md#el-entorno-es-de-la-máquina)) |
| `disk` | sin publicar | `disk_mib` en `POST /machines`: el disco escribible de la máquina (64 MiB–256 GiB, 512 por defecto; disperso; tiene que caber en el disco libre del host, `503` si no, y `KLING_MAX_DISK_MIB` baja el máximo); con `from` se ignora, la copia hereda el del dorado. `diff_base` en la máquina: una copia con seguimiento de páginas sucias que se congela en diferencial respecto a ese mem.file (ver [imagenes.md](imagenes.md)) |
| `start` | sin publicar | `POST /machines/{ref}/start` (arrancar otra vez, en frío, una máquina parada; `env` en el cuerpo) |

Las credenciales `type: "postgres"` (con `port`, `user`, `database`, `any_database`,
`ca_pem`, `upstream`, `upstream_tls`, `tls_server_name`) en `POST /machines/{ref}/credentials`
y `PUT /snapshots/{name}/credentials` llegaron en v0.17 **sin capacidad propia**: un daemon
que anuncia `db-attach` las entiende.

## Rutas

### Daemon

| Ruta | Qué hace |
|---|---|
| `GET /info` | versión, raíz, KVM, máquinas, versión del VMM (`firecracker`, por historia, también con `vz`), capacidades, `backend` (`firecracker` o `vz`, desde v0.9), `arch` (GOARCH del host), `share_roots` (desde v0.10), `cow` (modo de copia de discos de `run -from`: `setting`, `mode` `reflink`/`store`/`clonefile`/`copy`, `reason`, `pending`, `store` y `clones`; ver [cow.md](cow.md)), `authz` (`enabled`, el `role` de quien pregunta y su `uid`; ver [authz.md](authz.md)) y `tuning` (los ajustes efectivos del daemon que se cambian por entorno, `KLING_MAX_MACHINES`, `KLING_MIN_FREE_DISK_MIB`…, con el valor que aplica de verdad; solo a un admin o sin política; `kling doctor` los enseña). Contesta también a quien no tiene rol, sin contarle máquinas |
| `GET /events` | flujo NDJSON de eventos (`machine.*`, `snapshot.committed`, `snapshot.annotated`, `store.updated`), con latido cada 30 s. Un suscriptor que no lee a tiempo pierde eventos; cuando vuelve a haber sitio recibe antes del siguiente un `events.dropped` con `dropped` (cuántos; a un inquilino solo se le dice que hubo pérdida) |
| `GET /metrics` | métricas Prometheus en texto: máquinas por estado, memoria del host y PSS por microVM; `kling_operations_total{op,result}` (run, thaw, freeze, start; un rechazo o un error de quien llama no es `error`), `kling_admission_rejections_total{code}` (409 tope, 429 cuota de inquilino, 503 disco, 507 memoria), el histograma `kling_operation_duration_ms{kind}` (boot —el arranque en frío de un run o de un start—, restore, thaw, resume, freeze; `thaw` y `resume` son el despertar entero, `wake.total_ms`, no solo `thaw_ms`), `kling_gc_evictions_total`, `kling_orphan_vmms_killed_total`, `kling_events_dropped_total`, `kling_disk_free_mib` y `kling_pending_mib` (memoria de los arranques en curso). Los contadores empiezan en 0 con cada daemon |
| `GET /procstats` | memoria por microVM (PSS) y del host, en JSON |
| `POST /cow/store/grow` | amplía en caliente el almacén de copias de disco (`size_mib`, el tamaño nuevo, o `add_mib`, cuánto añadir); devuelve el `store` como en `GET /info`. Solo admin; capacidad `cow-grow`. Ver [cow.md](cow.md#hacer-crecer-el-almacén) |

### Máquinas

| Ruta | Qué hace |
|---|---|
| `GET /machines` | lista |
| `POST /machines` | crea y arranca (`RunRequest`: imagen o `from` un snapshot, vCPUs, memoria, egress y dominios, TTL, techo de CPU, volúmenes, carpetas compartidas, etiquetas) |
| `GET /machines/{ref}` | una máquina |
| `POST /machines/{ref}/freeze` · `/thaw` · `/stop` | ciclo de vida. Parar conserva el disco de la máquina (su overlay) y borra su volcado. Una parada se recoge sola solo si no pierde nada: la instancia de un servicio (etiqueta `service`) salida de su dorado, pasadas `KLING_STOPPED_RETENTION` (24 h; `0` lo apaga). `404` si la máquina no existe, `409` si su estado no lo admite (congelar, descongelar o pausar una parada; pausar una congelada); igual `pause` y `DELETE /machines/{ref}`. Pedir el estado en el que ya está (descongelar o arrancar una que corre, congelar una congelada, pausar una pausada) no es un error: `200` con la máquina, sin tocarla |
| `POST /machines/{ref}/start` | arranca otra vez una máquina parada, en frío sobre su disco, con su imagen, memoria, volúmenes, salida de red y carpetas (`StartRequest`: `env`, `KEY=valor`; opcional). Exige todas las claves de `env_keys`: el daemon no guarda los valores (`400` con las que faltan). Si falla, la máquina sigue parada con el motivo en `last_error`. `404` y `409` como `freeze`. Capacidad `start` |
| `POST /machines/{ref}/pause` | pausa una máquina en marcha sin volcarla (ver abajo) |
| `POST /machines/{ref}/renew` | reinicia el reloj del TTL (ver abajo) |
| `POST /machines/{ref}/squeeze?force=1` | el globo devuelve al host la memoria libre del invitado. `409` en una copia que comparte memoria con su dorado (`mem_shared`, Firecracker) salvo `force` (ver "Listo y ganchos tras restaurar") |
| `POST /machines/{ref}/mmds` | secretos por MMDS, comunes a todas las sesiones de la máquina (`{"env":{...}}`, ≤1 MiB; el campo `sessions` ya no se usa); la máquina deja de poder congelarse. Un almacén vacío (`{}` o `null`) levanta la marca solo si tras la última inyección corrieron con éxito los ganchos de la imagen (`POST .../hooks?wait=`); vaciar una máquina sin secretos no la marca |
| `GET /machines/{ref}/ready?wait=60s` | ¿está lista según su imagen? (`ReadyResult`: `ready` = `ready`, `waiting`, `failed` o `""` si no hay nada que esperar; `guest` con lo que dijo el agente; `detail` si el agente no contestó o contestó un error, que es `waiting` y no "nada que esperar"; si el daemon no puede mirar la imagen —un host sin `debugfs`— y el agente nunca ha contestado, es `waiting` durante los primeros 30 s desde que arrancó o se restauró, y después `""`, como una imagen sin agente). Sin `wait`, pregunta una vez; un "no listo" es un `200` |
| `POST /machines/{ref}/hooks?wait=60s` | vuelve a lanzar los ganchos tras restaurar de la imagen y, con `wait`, espera a que acaben (`ReadyResult`). `409` si el agente es anterior |
| `POST /machines/{ref}/credentials` | entrega claves al proxy de credenciales (`{"credentials":[{"domain","env","secret","allow"}]}`, ≤256 KiB, hasta 16): el invitado recibe en `env` un marcador que el proxy cambia por la clave solo hacia `http://domain`. `allow` (opcional, hasta 32) limita qué peticiones llevan la clave: `"MÉTODO /ruta"` con método exacto (GET, HEAD, POST, PUT, PATCH, DELETE u OPTIONS), `*` dentro de un segmento y `**` como último segmento para cualquier resto; la ruta de la petición se compara normalizada con `path.Clean`; lo que no casa con ninguna credencial del dominio recibe 403; vacío permite todo. `headers` (hasta 8), `query` y `body` dicen dónde se cambia el marcador: por defecto solo en `Authorization` (también dentro de un `Basic`) y `X-Api-Key`; `headers` añade cabeceras, `query` la query (`?key=`) y `body` el cuerpo, inseguro frente a un proveedor que refleje lo que recibe (ver SECURITY.md). Se fusiona por `env` (repetir una rota la clave, sustituye también su `allow` y conserva el marcador). Exige egress allowlist; la máquina sigue pudiendo congelarse y las claves sobreviven al reinicio del daemon (cifradas en su directorio). `Machine.credential_domains` lista los dominios, y `Machine.credential_any_database` las variables de las credenciales postgres que entran en cualquier base (sin `database`, o de un almacén anterior a que fuese obligatoria: el daemon lo avisa en su log al cargarlas). Con `"type":"postgres"` es una contraseña de base de datos (ver abajo) |
| `DELETE /machines/{ref}/credentials/{env}?upstream_machine=ID` | retira de la máquina la credencial de esa variable (404 si no la tiene). Con `upstream_machine`, solo si va a esa máquina (`kling db detach`). En una máquina viva el proxy deja de conocer el marcador y corta las sesiones de Postgres que lo usaban, y el marcador sale de MMDS; en una congelada o parada solo cambia el almacén. Devuelve la máquina |
| `PUT /machines/{ref}/labels` | reetiqueta. Cambiar `kling.db.owner`, `kling.db.state`, `kling.db.golden` o `kling.ports` corta las sesiones de otras máquinas hacia esta y las suyas hacia otras (modelo A, ver Credenciales de Postgres) |
| `POST /machines/{ref}/commit` | congela la máquina como snapshot reutilizable (`409` si tiene carpetas compartidas). Antes espera a que esté lista según su imagen (`ready_timeout_seconds`, por defecto 120; `409` si no llega; `skip_ready` se lo salta) |
| `GET /machines/{ref}/logs?tail=N` | consola serie |
| `GET /machines/{ref}/credaudit?tail=N&denied=1&since=T` | registro de auditoría del proxy de credenciales, NDJSON (ver abajo) |
| `POST /machines/{ref}/guest` | reenvía una petición HTTP al invitado (ver abajo) |
| `DELETE /machines/{ref}` | la borra |

### Snapshots

| Ruta | Qué hace |
|---|---|
| `GET /snapshots` | lista |
| `GET /snapshots/{name}` | uno, con disco e instancias vivas |
| `PUT /snapshots/{name}/annotations/{key}` | guarda JSON opaco (≤1 MiB, ≤32 claves, clave `^[a-z0-9][a-z0-9._-]{0,63}$`) |
| `PUT /snapshots/{name}/credentials` | ata claves a una plantilla (mismo cuerpo; `"clear":true` las quita todas): cada instancia que nazca de ella (`run -from`, réplicas del gateway) las recibe en su proxy al arrancar, con un marcador propio. Exige que la plantilla tenga egress allowlist; `run -from` con otro egress se rechaza. `Snapshot.credential_domains` lista los dominios y `Snapshot.credential_any_database` las postgres que entran en cualquier base |
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
| `POST /images` | construye. Con `builder`, lo hace el ejecutable de root `/usr/local/lib/kindling/builders/<builder>` (o `$KLING_BUILDERS_DIR`) con el `spec` de la petición. `builder` es obligatorio desde v0.6: el núcleo trae `base`, `llm` (modelos VON, [`von.md`](von.md)) `android` (Redroid + base Debian + dm-verity, todo en Go: [`prototypes/android/docs/constructor.md`](../prototypes/android/docs/constructor.md)) `debian` (Debian fijada con los paquetes del spec, lockfile y dm-verity opcional, todo en Go: [`imagenes.md`](imagenes.md)) y `oci` (una imagen de Docker/OCI como base propia, con su ENTRYPOINT supervisado: [`imagenes.md`](imagenes.md#imágenes-de-docker-el-constructor-oci)), y kindling-mcp instala `mcp` |
| `GET /images/{name}/recipe` | cómo se construyó |
| `GET /images/{name}/files?path=/p[&max=N]` | el contenido de un fichero de dentro (1 MiB por defecto, hasta 64) |
| `GET /images/{name}/files?path=/p&stat=1` | `{exists, size, sha256}` |
| `PUT /images/{name}/files` | pone un fichero dentro (`path`, `mode`, `content_b64` hasta 8 MiB o `from_host` relativo a `/usr/local/lib/kindling`, `create`); se niega si la imagen está en uso. En Linux monta la imagen; en macOS escribe con `debugfs -w`, sin montarla (ver [mac.md](mac.md)) |
| `DELETE /images/{name}` | la borra si nada la usa |
| `GET /images/{name}/blob[?part=P]` | el fichero de la imagen, en flujo, con `Content-Length`, `X-Kling-Sha256` y `X-Kling-Part`. Sin `part`, el ext4 de una monolítica o la capa de una por capas. `HEAD` da las mismas cabeceras sin cuerpo |
| `PUT /images/{name}/blob?part=P` | recibe una parte en flujo (hasta 16 GiB): temporal, sha256 comprobado si llega `X-Kling-Sha256`, renombrado atómico. `201` si la escribe, `200` con `unchanged` si ya había una idéntica, `409` si la imagen está en uso y el contenido es distinto, también si la está leyendo un arranque en vuelo (vuelve a intentarlo) |

**Partes de un blob.** `part` es `image` (`<name>.ext4`), `layer`
(`<name>.layer.ext4`) o `recipe` (`<name>.recipe.json`). El nombre `vmlinux` está
reservado para el kernel compartido (`part` vacía o `kernel`). "En uso" es lo mismo
que impide borrarla: un dorado o una máquina que no esté parada que la usen, o
capas encima, o un arranque en frío en vuelo que ya la eligió; para el kernel,
cualquier máquina que no esté parada o cualquier arranque en vuelo. La comparación
con lo que hay, la comprobación de uso y el renombrado se hacen con los arranques
en frío en espera, así que ninguno lee una imagen a medio sustituir. La receta de
una imagen por capas cuenta como la imagen, porque decide su base. Es lo que usa
`kling image copy` (`api.CopyImage`) para llevar una imagen de un daemon Linux a
uno de macOS, donde `POST /images` contesta `501` salvo para los constructores
escritos en Go de punta a punta (hoy `android`, `debian` y `oci`), que no montan ni hacen chroot.

**El protocolo del constructor.** El daemon crea un directorio de trabajo, deja
en él `request.json` y ejecuta `<constructor> <dir>` con `KLING_ROOT`,
`KLING_IMAGE_NAME`, `KLING_BUILD_DIR` y, si se pidieron, `BASE_IMAGE` y `GROW`. El
constructor tiene que dejar `$KLING_ROOT/images/<name>.ext4` o
`<name>.layer.ext4` y salir con 0; su salida vuelve a quien pidió la construcción.
Tiene que ser de root y nadie más puede escribirlo, ni a él ni a su directorio,
porque el daemon lo ejecuta como root. Excepción: `oci` (el del núcleo o uno
instalado con ese nombre) corre con el usuario de construcción
(`kindling-build`, ver `docs/imagenes.md`) cuando existe: recibe además
`KLING_OUT_DIR` (deja ahí la imagen; el daemon la valida y la mueve a
`images/`) y `KLING_CACHE_DIR` (su caché), y del entorno del daemon solo una
lista blanca. La receta la escribe el daemon, con
permisos `0600` porque el spec puede llevar secretos. El constructor puede dejar
al lado de `request.json` un `recipe.json` (`api.BuildRecipeHints`: `base` si la
eligió o la hizo él, `cpu_pct`, `cpu_pct_per_vcpu`, `guest_ipv6_stack` y `built`,
lo que apuntó de lo construido) y el daemon lo lleva a la receta. Los del núcleo
en Go (`android`, `debian`, `oci`) no hace falta instalarlos: el daemon se
ejecuta a sí mismo como `<su binario> builder <nombre>`, y así construye
siempre el mismo binario que atiende la petición. El orden:

1. Sin `KLING_BUILDERS_DIR`, el propio binario del daemon, aunque haya uno
   instalado en `/usr/local/lib/kindling/builders/` (ese envoltorio lanza el
   `kling` del sistema: un daemon privado o recién compilado construiría con
   otro).
2. Con `KLING_BUILDERS_DIR` puesto, el constructor de ese directorio si está
   (para probar uno a mano), y si no, otra vez el propio binario.

Con usuario de construcción (`-build-as`), ese usuario tiene que poder ejecutar
el binario del daemon: uno bajo `/root` no le deja pasar y la construcción falla
con un 412 que lo dice. El resto de constructores, siempre el instalado.

### Volúmenes

| Ruta | Qué hace |
|---|---|
| `GET /volumes` | lista |
| `POST /volumes` | crea (`name`, `size_mib`) |
| `POST /volumes/{name}/populate` | instala paquetes dentro con una microVM de un solo uso |
| `DELETE /volumes/{name}` | lo borra si nada lo usa (409 si no); con snapshots, 409 salvo `?snapshots=1`, que se los lleva también |
| `GET /volumes/{name}/snapshots` | lista sus snapshots, del más antiguo al último (404 si no existe el volumen) |
| `POST /volumes/{name}/snapshots` | toma uno (`name`, opcional: hora UTC `20260928-153012`) |
| `POST /volumes/{name}/restore` | lo devuelve a un snapshot (`snapshot`); lo anterior queda en `undo` |
| `DELETE /volumes/{name}/snapshots/{snap}` | borra un snapshot |

`GET /volumes` trae además `snapshots`, cuántos tiene cada uno (`undo` incluido).

**Snapshots de volumen.** Un snapshot solo se toma sin escritores (los lectores no
cambian bloques) y un restore solo sin ningún usuario; una máquina congelada o warm
cuenta, porque sigue teniendo el volumen montado. Mientras dura, el volumen queda
reservado y un arranque que llegue a medias se rechaza. `POST .../snapshots` devuelve
el `VolumeSnapshot` (`volume`, `name`, `created_at`, `size_bytes`, `used_bytes`,
`undo`, `mode`); `POST .../restore` devuelve `{volume, snapshot, mode, undo}`, con
`undo` el snapshot que guardó el estado previo. `mode` dice cómo se copió: `reflink`
(XFS/Btrfs), `clone` (APFS) o `copy` (copia completa y dispersa).

Códigos: `400` nombre inválido (los dos siguen la regla de los volúmenes; `undo` está
reservado para crear), `404` volumen o snapshot inexistente, `409` en uso, otra
operación en curso sobre el mismo volumen, nombre repetido o tope de 16 snapshots
(sin contar `undo`), `507` si hace falta una copia completa y no cabe sin comerse el
suelo de disco libre (`KLING_MIN_FREE_DISK_MIB`) o el hueco hasta la marca alta del
gc. Los snapshots viven en `volumes/snapshots/<vol>/<snap>.ext4`, de root y `0600`.
A propósito, ni restore ni el borrado de snapshots se exponen como herramienta MCP:
son operaciones destructivas del operador, no del agente.

### `POST /machines/{ref}/pause`

Pausa una máquina running sin volcarla: el VMM sigue vivo con el invitado
parado (`PATCH /vm` `Paused` de Firecracker), sin gastar CPU pero reteniendo la
RAM que ya tocó. `thaw` la reanuda con un simple `Resume`, sin `LoadSnapshot`
ni resincronización: ~0,3 ms. Cuenta como viva para el vigilante, el TTL y la
reconciliación, así que sobrevive a un reinicio del daemon igual que una
running. Pausar una máquina que ya está pausada es un no-op (devuelve su
estado tal cual); pausar una que no está `running`, o que tiene carpetas
compartidas en vivo (no contestarían pausada y su sesión caería por
keepalive: usa `freeze` en su lugar), da `400`.

### `POST /machines/{ref}/resize`

`{"mem_mib": 1024}` cambia la memoria de una máquina en marcha sin reiniciarla,
entre 128 MiB y el techo con el que arrancó (`mem_max_mib` en `POST /machines`).
La máquina arranca con el techo y el globo retiene la diferencia; subir es
desinflarlo y bajar, inflarlo. Una máquina sin techo tiene la memoria fija (400).
El techo queda en el snapshot al hacer commit.

### `GET /machines/{ref}/credaudit`

El registro de auditoría del proxy de credenciales de la máquina
(`<root>/audit/<id>.jsonl` en Linux, fuera del alcance del VMM;
`machines/<id>/credaudit.jsonl` en macOS; y su rotación `.1`), como NDJSON: una
`api.CredAuditRecord` por línea, las más antiguas primero. Funciona con la máquina
corriendo, congelada o parada; sin registro (nunca tuvo credenciales) devuelve `200`
vacío. Lo usa `kling machine audit`.

| Parámetro | Valor |
|---|---|
| `tail` | últimas N líneas tras filtrar (defecto 200; `0` = todo lo que el daemon lee, hasta 4 MiB) |
| `denied` | `1`/`true`: solo las denegadas por política (las líneas `dropped` salen siempre) |
| `since` | RFC 3339 (`2026-09-28T10:00:00Z`) o una duración hacia atrás (`10m`, `2h`) |

Un parámetro que no se entiende es `400`; una máquina que no existe, `404`.

```json
{"ts":"2026-09-28T10:00:00.123Z","kind":"http","method":"GET","host":"api.example.com",
 "path":"/v1/files/:tok","query":true,"status":200,"creds":["API_KEY"],
 "req_bytes":0,"resp_bytes":512,"ms":84}
```

| Campo | Qué es |
|---|---|
| `kind` | `http`, `postgres` (una conexión, ver abajo), `link` (una conexión por una arista de un grafo: `host` es `<nodo>.graph:P`, `upstream` la máquina a la que llegó, `machine:<id>`, y `reason` puede ser `machine_unavailable`, `busy`, `no_capacity`, `upstream_error` o `invalidated`; ver [grafos.md](grafos.md)); `dropped` es una línea que solo lleva la cuenta de descartados |
| `path` | ruta normalizada, cortada a 256 bytes; un segmento con un marcador o una forma de una clave sale `:cred`, uno de ≥32 caracteres base64url/hex, `:tok`; los caracteres de control salen como `?` |
| `query` | si la petición llevaba query (su contenido no se escribe nunca) |
| `status` | lo que recibió el invitado |
| `reason` | vacío si llegó al proveedor y volvió entera; si no: `disabled`, `busy`, `no_credential`, `connect`, `ambiguous_path`, `not_allowed`, `body_too_large`, `bad_body`, `bad_request`, `upstream_error`, `bad_encoding`, `aborted` |
| `denied` | la rechazó la política: `disabled`, `no_credential`, `ambiguous_path`, `not_allowed` |
| `creds` | `env` de las credenciales cuyo marcador se sustituyó en esta petición |
| `req_bytes`, `resp_bytes`, `ms` | cuerpo leído del invitado, cuerpo enviado al invitado, duración |
| `dropped` | registros descartados antes de este (cola llena, fallo de disco o caídos de la generación más antigua al rotar) |
| `rotated` | de `dropped`, los que se cayeron al rotar |
| `user`, `database`, `auth` | solo en `kind: postgres` (ver Credenciales de Postgres) |
| `upstream` | solo en `kind: postgres` con upstream fijado: la dirección a la que marcó el proxy (configuración del operador, no un secreto) |

Nunca lleva la clave, el marcador, cabeceras, cuerpos, la query ni el texto de un
error del proveedor. Rota a 4 MiB con 3 generaciones (`daemon.credaudit_max_mib`,
`daemon.credaudit_generations`), se lee entero, y lo que se cae de la más antigua
cuenta en `dropped`; `commit` y `fork` no lo copian.

### Credenciales de Postgres

Una credencial con `"type":"postgres"` en `POST /machines/{ref}/credentials` o
`PUT /snapshots/{name}/credentials`:

```json
{"credentials":[{"type":"postgres","domain":"db.example.com","port":5432,"user":"app",
 "database":"appdb","ca_pem":"-----BEGIN CERTIFICATE-----\n...","env":"PGPASSWORD","secret":"..."}]}
```

| Campo | Qué es |
|---|---|
| `type` | `postgres`; vacío o `http` es una credencial HTTP (y entonces `port`, `user`, `database`, `any_database`, `ca_pem`, `upstream`, `upstream_tls` y `tls_server_name` no valen) |
| `domain` | nombre que usa el invitado. Sin `upstream`, es también al que sale el proxy (tiene que resolver a una IPv4 pública); con TLS, contra el que se verifica el certificado salvo `tls_server_name` |
| `port` | puerto del servidor (defecto 5432). El invitado puede usar cualquier puerto: le llega al proxy igual |
| `user` | rol (obligatorio). El invitado tiene que conectar con él |
| `database` | la única base a la que se deja conectar; obligatoria salvo `any_database` (la de por defecto de un cliente es el rol) |
| `any_database` | `true` deja conectar a cualquier base con `CONNECT` para el rol; excluyente con `database`. Un almacén anterior sin base se lee como `true` |
| `ca_pem` | opcional, ≤64 KiB: CA en PEM que se añade a las raíces del sistema |
| `upstream` | opcional, `"host:puerto"` (IP o nombre; IPv6 entre corchetes): a dónde marca el proxy en lugar de `domain:port`. Admite loopback y privadas; nunca `169.254.0.0/16`, `0.0.0.0/8`, multicast, `240.0.0.0/4`, `fe80::/10`, `fd00:ec2::254`, `172.16.0.0/30` ni `172.30.0.0/16`. Un nombre se resuelve al marcar y ninguna de sus IPs puede caer ahí (`localhost` es el loopback sin DNS); en macOS lo resuelve el daemon al entregarla a `kling-vz` (con las mismas comprobaciones) y este recibe la primera IP. Se devuelve normalizado (minúsculas) |
| `upstream_tls` | opcional: `verify-full` (defecto, se guarda vacío) o `disable`: sin TLS y solo SCRAM-SHA-256 (ni contraseña en claro, ni md5, ni trust, ni `-PLUS`). `disable` exige `upstream` y no admite `ca_pem` ni `tls_server_name` |
| `tls_server_name` | opcional: nombre (o IP) contra el que se verifica el certificado en lugar de `domain` |
| `upstream_machine` | opcional, solo en credenciales de máquina (no de plantilla): el **id** exacto (hexadecimal) de una copia de `kling db` a la que marca el proxy, el modelo A de [db.md](db.md). Nunca una dirección: el daemon la resuelve en cada conexión y solo si la copia existe con ese id, corre, lleva `kling.db.golden`, `kling.db.state=ready`, expone `port` en `kling.ports`, y ella, el agente y `upstream_owner` dicen el mismo `kling.db.owner`. Excluye `upstream`, exige `upstream_tls: "disable"` (SCRAM-SHA-256) y `database`. Se comprueba también al entregar. En macOS lo pide el `kling-vz` del agente en cada conexión al broker de enlaces del daemon, que marca él mismo (ver [db.md](db.md)) |
| `upstream_owner` | con `upstream_machine`, obligatorio: el `kling.db.owner` que tienen que compartir copia y agente |
| `secret` | contraseña del rol, ASCII imprimible |
| `allow` | no vale para Postgres |

El invitado recibe el marcador en `env` y lo usa como contraseña, sin TLS
(`sslmode=disable` o `prefer`) contra `domain`. Al rotar (misma `env`) se sustituyen
todos los campos con la clave. En macOS el daemon lo rechaza si el `kling-vz` de la
máquina no incluye `postgres` en `credential_kinds` de `GET /kling/info`, y rechaza una
que use `upstream`, `upstream_tls` o `tls_server_name` si no incluye `postgres-upstream`,
ni una con `upstream_machine` (ni las aristas de un grafo) si no incluye `graph-link`.
Recetas y límites en [postgres.md](postgres.md).

En el registro de auditoría cada conexión es una línea `kind: postgres` con `host`,
`upstream` (si lo hay), `user`, `database`, `auth` (`scram-sha-256-plus`, `scram-sha-256`, `password` o `trust`:
cómo se autenticó el proxy ante el servidor), `creds`, bytes y duración; `method` es
`cancel` en un `CancelRequest`. Sus `reason`: los de arriba (`disabled`, `busy`,
`no_credential`, `upstream_error`) y `bad_startup`, `timeout`, `bad_placeholder`,
`user_mismatch`, `database_mismatch`, `replication`, `upstream_tls`, `upstream_auth`,
`unknown_cancel`, `machine_unavailable` (la copia de `upstream_machine` no se pudo usar);
son `denied` `disabled`, `no_credential`, `bad_placeholder`, `user_mismatch`,
`database_mismatch`, `replication`, `unknown_cancel` y `machine_unavailable`. Con
`upstream_machine`, `upstream` es `machine:<id>` (nunca la dirección resuelta).

### Credenciales de MySQL

`"type":"mysql"` lleva los mismos campos que `postgres` salvo `upstream_machine` y
`upstream_owner` (no se admiten), con `port` 3306 por defecto y distinto de 53, 80 y 443.
`database` es la base con la que arranca la sesión (el invitado puede pedir esa o
ninguna), no una frontera: lo que acota es el `GRANT` del usuario. `upstream_tls:
"disable"` admite solo `mysql_native_password` y la ruta rápida de
`caching_sha2_password`. Si la máquina tiene credenciales `postgres` y `mysql`, las
`mysql` tienen que usar el 3306 y ninguna `postgres` puede. En macOS el daemon exige que
`credential_kinds` incluya `mysql`. En el registro, `kind: mysql` con los mismos campos
y `auth` `mysql_native_password`, `caching_sha2_password-fast`, `caching_sha2_password`
(autenticación completa, dentro del TLS) o `mysql_clear_password`; un `reason` más,
`capabilities` (el servidor no habla el dialecto que eligió el invitado). Ver
[mysql.md](mysql.md).

### Admisión

`POST /machines` y `POST /sandboxes` rechazan antes de arrancar:

| Código | Causa |
|---|---|
| `507` | no cabe en memoria, o el host está bajo presión (PSI `some avg10` por encima de `KLING_MAX_MEM_PRESSURE`, 20 % por defecto; en macOS, `kern.memorystatus_level` por debajo de `KLING_MIN_MEM_LEVEL`, 15 % por defecto, o el swap usado por encima de `KLING_MAX_SWAP_PCT`, 85 % por defecto, de lo que puede llegar a ocupar: el swap actual más el disco libre del volumen `VM` por encima del mínimo de disco. Ver [estabilidad.md](estabilidad.md) §9) |
| `503` | queda menos disco que `KLING_MIN_FREE_DISK_MIB` bajo `$KLING_ROOT` (2 GiB en Linux; 16 GiB en macOS, donde el swap crece en el mismo disco). No es un 507 a propósito: quien recibe un 507 congela para hacer sitio, y congelar escribe en disco |
| `409` | tope de máquinas del daemon (`KLING_MAX_MACHINES`, 256) |
| `429` | cuota del inquilino dueño (`kling.owner`) en la política de autorización: `max_machines`, `max_mem_mib` o `max_disk_mib`. El mensaje dice qué tope, cuánto lleva y cuánto pide, y lleva `quota exceeded` (`api.IsTenantQuota`). También en `run -from`, `fork`, `graph up` y `start`. Ver [authz.md](authz.md#cuotas) |

### El proxy y los puertos

`POST /machines/{ref}/guest` solo llega al puerto 8080 del invitado, salvo los
que la máquina declare en la etiqueta `kling.ports` (lista separada por comas),
que un snapshot hereda. Otro puerto es `403`.

Una máquina lleva `ip` y, en macOS, `forwards`: `{"8080": "127.0.0.1:61234", ...}`,
los puertos de loopback por los que el host llega a cada puerto expuesto del
invitado (allí todos los invitados comparten IP). Quien hable con un invitado sin
pasar por el daemon resuelve la dirección con `api.Machine.Addr(puerto)`, que usa
el reenvío si lo hay y `ip:puerto` si no.

## Grafos

Varias máquinas con aristas declaradas y ciclo de vida atómico (capacidad `graphs`;
uso en [grafos.md](grafos.md)). `{ref}` es el ID, el nombre o un prefijo único del ID.

| Ruta | Qué hace |
|---|---|
| `POST /graphs` | crea el grafo y arranca sus nodos `eager` (`201` con el grafo). Cuerpo `{"graph": Graph, "secrets": {"<from>/<ENV>": "clave"}}`: una clave por arista `credential`, ninguna de más. Todo o nada; `409` si ya hay uno con ese nombre o no caben las máquinas, `429` si no caben en la cuota de su inquilino, `507` si no cabe la memoria de los `eager`. En macOS las aristas `link` y `credential` van por el broker de enlaces y una `depends` con `port` espera preguntando al `kling-vz` del destino |
| `GET /graphs` | lista, por nombre |
| `GET /graphs/{ref}` | uno, con el estado de cada nodo |
| `POST /graphs/{ref}/freeze` · `/thaw` | todos los nodos con máquina. Si uno falla sigue con los demás y devuelve el primer error |
| `POST /graphs/{ref}/snapshot` | `{"name": "prefijo"}` opcional. Una plantilla `<prefijo>-<nodo>-<gen>` por nodo con máquina, del mismo instante; devuelve `{"graph", "generation", "templates": {nodo: plantilla}, "warnings"}`; `warnings` (si lo hay) nombra los nodos que se volcaron pero no se pudieron volver a congelar y siguen en marcha. Los congelados se despiertan para el instante y se vuelven a congelar. `409` si un nodo ya pausado tiene volúmenes o el grafo tiene aristas `share` (también en `fork`) |
| `POST /graphs/{ref}/fork` | `{"count": N}` (1 a 16). Devuelve `{"graphs": [...]}` (`201`) |
| `DELETE /graphs/{ref}` | el grafo y sus máquinas (`204`) |

`Graph` es `{"id", "name", "nodes": {nombre: GraphNode}, "edges": [GraphEdge], "state",
"generation", "created_at", "fork_of"}`; `id`, `state`, `generation`, `created_at`,
`fork_of` y en cada nodo `machine_id` y `state` los pone el daemon (lo que llegue en
ellos se ignora). `GraphNode`: `from` o `image`, `vcpus`, `mem_mib`, `egress`,
`allow_domains`, `ports`, `wake` (`eager`\|`lazy`), `idle_freeze`, `volumes`, `shares`,
`labels`, `allow_exec`. `GraphEdge`: `from`, `to`, `kind`
(`link`\|`credential`\|`share`\|`depends`; `mcp` se rechaza con el motivo), `port`, en
`credential` `env`, `user` y `database`, y en `share` `mount` y `mode` (`ro`\|`rw`). La clave de una arista no está
nunca en `Graph`. Las etiquetas `kling.graph` y `kling.graph.*` las pone solo el
daemon: `POST /machines`, `POST /sandboxes`, el fork de un sandbox y
`PUT /machines/{ref}/labels` las rechazan con `400`. El grafo se guarda en
`$KLING_ROOT/store/graph/<id>.json`, que `GET /store/graph` lee pero que solo el daemon
escribe.

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
`cpu_pct`, `allow_domains`, `volumes`, `shares` (ver más abajo; no con `from`) y
`labels`.

| Ruta | Qué hace |
|---|---|
| `GET /sandboxes` | los vivos |
| `GET /sandboxes/{ref}` | uno |
| `POST /sandboxes/{ref}/renew` | `{"ttl_seconds": N}`: vence N segundos a partir de ahora |
| `DELETE /sandboxes/{ref}` | lo destruye |

Estas rutas solo tocan máquinas con `kind=sandbox`.

### `POST /sandboxes/{ref}/fork`

Ramifica un sandbox en marcha en `count` copias (1 a 64; capacidad `fork`). Cuerpo
opcional:

```json
{"count": 2, "ttl_seconds": 600, "on_ttl": "remove", "labels": {"kling.db.state": "preparing"}}
```

`labels` (desde sin publicar) se suman a las de cada copia en su nacimiento, sin ventana
en la que exista sin ellas; van con `-label k=v` (repetible) en `kling sandbox fork`. Las
claves siguen `^[a-z0-9][a-z0-9._-]{0,63}$`, como mucho 32 etiquetas de 256 bytes de
valor, y `kind` y `kling.fork-of` están reservadas (`400`). Un cuerpo sin `labels` se
comporta como siempre. Una máquina con credenciales del proxy se rechaza con `409`:
arranca otra instancia con `run -from <plantilla>` en vez de ramificarla.

### `allow_exec` y `on_ttl` en `POST /machines`

`allow_exec: true` enciende exec y ficheros. Con `from`, las máquinas heredan la
del snapshot, y pedirla sobre uno que no la tiene es `409`. `on_ttl` decide qué pasa
al vencer `ttl_seconds`: `freeze` (por defecto) o `remove`. `commit` graba
`allow_exec` en el snapshot.

### Renovar el TTL de una máquina

El TTL se cuenta desde que se creó la máquina (o desde su última renovación), y
`thaw` no lo reinicia: un sandbox no alarga su vida por despertarse. Quien gestiona
una máquina con TTL y la despierta o la usa más allá de ese plazo tiene que
renovarlo con `POST /machines/{ref}/renew` y `{"ttl_seconds": N}`: vence N segundos
a partir de ahora. Sin cuerpo, o con `0`, conserva el plazo que tenía y solo
reinicia el reloj; sobre una máquina sin TTL no hace nada. Vale también con la
máquina congelada. Un sandbox da `409`: se renueva por su ruta, que aplica su tope.

El planificador del gateway lo usa como arrendamiento: sus instancias nacen con
TTL 2×idle por si el gateway muere, y lo renueva al despertarlas o adoptarlas y en
cada vuelta del segador mientras siguen despiertas.

## Carpetas compartidas

Un directorio del host dentro de la máquina. El diseño, los límites y el modelo
de amenaza están en [compartir.md](compartir.md).

`POST /machines` (y `POST /sandboxes`) aceptan `shares`, una lista en orden:

```json
{"image": "toolchain", "shares": [
  {"mode": "copy", "mount": "/work", "upload": "3f1c…", "source": "/Users/juan/repo"},
  {"mode": "rw",   "mount": "/src",  "source": "/srv/code/repo"}
]}
```

- `mode: copy` (por defecto): una copia de solo lectura. `upload` es el id que
  devolvió `POST /shares/uploads`; se consume al arrancar. `source` solo se
  guarda para enseñarlo.
- `mode: ro | rw`: el directorio vivo. `source` es una ruta absoluta EN EL HOST
  DEL DAEMON, bajo algún `daemon.share_roots`; sin raíces configuradas, `400`
  diciendo cómo permitirlo. La petición vuelve cuando la carpeta ya está
  montada dentro; si el agente de la imagen es anterior a v0.10, la máquina se
  destruye y el error lo dice.
- `mount`: absoluto, limpio, sin espacios, comas ni dos puntos; ni `/` ni
  debajo de `/proc`, `/sys`, `/dev`, `/run`, `/bin`, `/sbin`, `/lib`, `/usr` o
  `/etc`; ni repetido, ni anidado con otra carpeta o un volumen.
- Como mucho 8 carpetas, y 8 discos entre volúmenes y copias. Con `from`, `400`:
  las carpetas se piden al arrancar en frío.

`GET /machines/{ref}` las devuelve en `shares`, con `status` (`attached`,
`detached` o `error: …`) en las vivas e `image_bytes` en las copias.

### `POST /shares/uploads`

El cuerpo es un tar (`application/x-tar`) con el contenido de la carpeta.
Contesta `201` con `{"id", "bytes", "files", "dirs", "symlinks",
"image_bytes"}` cuando el ext4 está construido. Solo se aceptan directorios,
ficheros regulares y enlaces simbólicos relativos que no salgan del árbol ni
atraviesen otros enlaces; nada de rutas absolutas, `..`, enlaces duros,
dispositivos o FIFOs (`400`, y la subida entera se descarta). El contenido
está acotado por `daemon.share_copy_max_mib` (1024 por defecto; más, `413`), y
como mucho hay 8 subidas pendientes; las que nadie usa se borran a la hora.

### Del lado del invitado: `POST /share/attach`

La abre el daemon contra el agente con `Upgrade: kling-share/1` y un cuerpo
`{"tag", "mount", "mode"}`; tras el `101` la conexión transporta el protocolo
descrito en compartir.md. El agente pone `X-Kling-Share` en todas sus
respuestas a esa ruta (también en los errores): así se distingue de un agente
anterior. Es una ruta de control (`guest.IsControlPath`).

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

## Tras restaurar: reloj y entropía del invitado

Todas las instancias de un mismo snapshot despiertan con la misma memoria: el
reloj de pared parado en el instante del volcado y el estado del generador
aleatorio del kernel copiado. Desde v0.9.1 (capacidad `guest-resync`), justo
después de cada `thaw` y de cada `POST /machines` con `from`, y antes de
devolver la máquina como `running`, el daemon llama al agente del invitado:

```
POST /resync   {"unix_nano": <hora del host>, "entropy": "<64 bytes en base64>"}
→ 200 {"skew_ms": <desfase corregido>}
```

El agente (`pkg/guest`, en `kling-guest` y en el puente de kindling-mcp) pone el
reloj (`settimeofday`), mezcla la entropía acreditándola (`RNDADDENTROPY`) y
fuerza la resiembra del CRNG (`RNDRESEEDCRNG`). Cuesta unos milisegundos; el
evento `machine.thawed`/`machine.started` dice cuántos.

Un agente anterior (404, o el 400 de un puente viejo) o una máquina sin agente no
hacen fallar la restauración: la máquina queda como antes —reloj parado,
aleatorios compartidos con sus hermanas— y el daemon avisa una vez por imagen.
Un invitado sin nadie en el puerto del agente puede tardar segundos en contestar
tras restaurar (en Linux el primer SYN se pierde), así que el daemon lo sabe de
antes: `freeze` sondea el puerto con el invitado en marcha y, si nadie escucha, el
`thaw` no lo intenta; y un snapshot cuyas instancias contestan "nadie escucha" no
se vuelve a intentar en 10 minutos.

En Firecracker la primera petición a un invitado recién restaurado cuesta
~150–250 ms en el laboratorio (virtualización anidada: el invitado vuelve a traer
sus páginas); `/resync` es esa primera petición, así que ese coste pasa del
primer cliente al thaw, y la petición siguiente tarda lo de siempre. En macOS
(`vz`) el resync cuesta 1–2 ms.
Reconstruir la imagen con un `kling-guest` actual (`kling image build`) o
refrescar su puente MCP (`kling mcp refresh-bridge`) lo arregla.

Solo el host debe llamar a `/resync` (o a `/volume/*`, `/exec`, `/files`): quien
reenvíe peticiones de terceros al puerto del agente tiene que cortar esas rutas
(`guest.IsControlPath`). Ver SECURITY.md.

## Listo y ganchos tras restaurar

"Listo" era "algo contesta en el 8080". Un invitado que arranca algo grande
detrás del agente (Android: el agente contesta a los 1,4 s, `sys.boot_completed`
llega a los 3–10 s) quedaba congelado a medio arrancar si se guardaba en medio, y
todas sus copias con él. Con la capacidad `ready`, la imagen lo declara en su
propio rootfs y el agente de invitado (`pkg/guest`, en `kling-guest` y en el
puente MCP) lo ejecuta él mismo, **sin `allow_exec`** y sin argumentos de nadie:

| Ruta del invitado | Qué es |
|---|---|
| `/etc/kindling/ready` | ejecutable que sale con 0 cuando el invitado está listo. Se pregunta hasta que contesta 0 y a partir de ahí se recuerda: es "terminó de arrancar", no un chequeo de vida (un dorado guardado listo trae el recuerdo a cada copia). Plazo de 10 s por ejecución, o el `probe_timeout_seconds` del servicio de la imagen (`service.json`, el `Timeout` del `HEALTHCHECK`; hasta 120 s) |
| `/etc/kindling/post-restore.d/*` | ejecutables que corren en orden (como `run-parts`: sin ocultos, `*~` ni `*.disabled`) al final de cada restauración, con el reloj y la entropía resincronizados, los volúmenes montados y las credenciales en MMDS. `KLING_RESTORE` dice de qué: `instance` (`run -from`, fork), `thaw` o `manual` (`POST .../hooks`). Plazo de 60 s cada uno; el primero que falla para la tanda y deja `failed`. Mientras corren, el invitado no está listo. Su salida va a la consola (`kling logs`) |

Rutas del agente, de control (el gateway no las reenvía):

```
GET  /ready   → 200 | 503  {"ready":bool,"probe":bool,"has_hooks":bool,"hooks":"running|done|failed","detail":"...","start_period_seconds":N}
POST /hooks?restore=instance|thaw|manual → 202 (409 si ya corren)
GET  /meminfo → {"total_mib":N,"available_mib":N}
```

El `/resync` devuelve además ese estado (`ready`), así que una imagen que no
declara nada no cuesta ni una petición más por restauración; con ganchos, el
daemon pide `POST /hooks` al final de la restauración y no espera a que acaben.

Qué hace el daemon con ello:

- **`commit` y `sandbox fork`** esperan a que el original esté listo antes de
  pausarlo (hasta `ready_timeout_seconds`, 120 por defecto; `409` con el motivo
  de la sonda si no llega). `skip_ready` (`kling save -force`, `sandbox fork
  -skip-ready`) se lo salta. El fork devuelve además cada copia cuando está lista.
- **`POST /machines` y `POST /sandboxes` con `wait_ready`** (`kling run
  -wait-ready`, `kling sandbox create -wait-ready`) no contestan hasta que lo está;
  la máquina se devuelve igual si no llega, y `Machine.ready` dice cómo quedó.
- **`Machine.ready`** (`kling ps` añade la columna `READY` si alguna la tiene):
  `ready`, `waiting` o `failed`; vacío si la imagen no declara nada o nadie ha
  mirado. Una vigía de fondo lo sigue tras arrancar o restaurar.

Compatibilidad: un agente anterior (`404`, o el `400` del puente viejo) o una
imagen sin nada de esto cuentan como "nada que esperar", y una imagen sin agente
no retrasa un commit (se sondea el puerto antes): lo de siempre. Si el daemon no
puede mirar la imagen (sin `debugfs`) y su agente aún no ha contestado nunca,
commit y fork esperan a que conteste tres cuartos de su plazo (`-wait`; 90 s con
los 2 min por defecto, nunca menos de 30 s) antes de darla por imagen sin agente.
Si el plazo no da para eso, fallan en vez de congelar a ciegas.

Si el `/resync` de una restauración falla, la copia aún dice lo que se guardó en
el dorado (ganchos `done`): el daemon lanza igualmente sus ganchos en cuanto el
agente contesta a `/ready`, y si no puede, la máquina queda `failed` hasta
`kling machine hooks`.

**Identidad por copia.** Lo que un gancho necesita y llega después de restaurar
(un secreto por MMDS) se entrega y se aplica así:

```sh
printf '{"phone":{"android_id":"…","name":"c1"}}' | kling machine secret c1 -hooks   # PUT + POST /hooks?wait
printf '{}' | kling machine secret c1                                              # vacía y levanta la marca
```

La marca `has_secrets` solo se levanta si, tras la **última** inyección no vacía,
una tanda de ganchos terminó con éxito y después se vació el almacén. Nadie puede
demostrar desde fuera que la RAM del invitado ya no guarda el secreto: la
garantía es la de la imagen, cuyo gancho declara al salir con 0 que lo aplicó y no
dejó copia. Sin ganchos (un servidor MCP que lo puso en el entorno de sus
procesos) la marca se queda. El registro vive en la memoria del daemon: tras
reiniciarlo, la marca también se queda.

**`cpu_pct` por imagen.** La receta (`<imagen>.recipe.json`) puede llevar
`cpu_pct` (techo absoluto, 100 = un núcleo) o `cpu_pct_per_vcpu` (por vCPU: 100
con 2 vCPU = 200; manda si están los dos). Precedencia: `cpu_pct` de la petición >
el del dorado (`from`) > la receta > `cpu_pct_default` de la petición (el valor
por defecto de la configuración del CLI, que ya no viaja como si fuera un flag) >
el del daemon (50). En vz el regulador pausa la VM entera: un Android de 2 vCPU
con el 50 % iba a ¼.

**Impulso de arranque (Linux).** Mientras arranca (en frío, `run -from` o
`thaw`), la máquina corre con todas sus vCPU enteras (sin pasar de los núcleos
del host) y vuelve a su `cpu_pct` cuando pasa la sonda de listo de su imagen,
o cuando contesta su agente si no declara sonda; como mucho 60 s, más el
`start_period_seconds` que diga el agente (el `StartPeriod` del `HEALTHCHECK`,
hasta 120 s). Mientras dura,
`GET /machines/{ref}` trae `cpu_boost_pct` (el techo de ese momento; `kling ps`
lo enseña en READY) y al acabar se publica `machine.boost_ended` con el motivo.
Un `cpu_pct` pedido explícitamente (`cpu_pct_fixed` en la máquina y en el
dorado) no lleva impulso. En el daemon, `KLING_READY_BOOST=0` vuelve al impulso
de antes (un núcleo hasta que contesta el agente, 10 s como mucho) y una
duración (`KLING_READY_BOOST=2m`) cambia el plazo. La admisión no lo cuenta: es
un techo de cgroup, no una reserva.

**Pila IPv6 por imagen.** `"guest_ipv6_stack": true` en la receta arranca el
invitado en frío con `ipv6.disable_ipv6=1` en vez de `ipv6.disable=1`: hay
sockets `AF_INET6` pero ninguna interfaz con IPv6. Para lo que no escucha sin
ellos (el `adbd` de Android solo abre `[::]:5555`). Su dorado lleva
`Snapshot.guest_ipv6_stack` (y `guest_ipv6_off` en false, sin la marca de dorado
anterior a la barrera). La barrera del host sigue
igual ([SECURITY.md](../SECURITY.md), "IPv6").

**Squeeze y memoria compartida.** En Firecracker una copia de un dorado mapea su
`mem.file` `MAP_PRIVATE` (`Machine.mem_shared`): la caché de páginas del invitado
son páginas limpias compartidas con las demás copias. El globo hace que el
invitado las suelte y las vuelva a leer como privadas: medido con 24 Android, Σ
PSS de 4533 a 5072 MiB y el host colgado. `squeeze` lo rechaza (`409`) salvo
`force`, y el "hacer sitio" automático se salta esas copias. Descongelar apaga
`mem_shared` (la memoria pasa a ser suya). En vz no hay compartición; sin
estadísticas del globo, `squeeze` pregunta `GET /meminfo` al agente y reclama lo
disponible menos un cuarto de la RAM, nunca por debajo de la mitad (antes, la
mitad a ciegas: un Android de 1 GiB se quedaba con 4 MiB disponibles).

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
`health_err`. Hasta v0.17 el daemon pasaba esos campos de un `meta.json` de v0.4
a las anotaciones `mcp.tools` y `mcp.health`, y movía un `links.json` a
`store/mcp/links`. Desde v0.18 ya no: un dorado con esos campos no se lista ni
se restaura (el error dice que se rehaga con `kling save`), y una raíz con un
`links.json` sin migrar no arranca; las dos cosas se arreglan arrancando una
vez kling v0.17 sobre ella ([`actualizar.md`](actualizar.md) §5, PR 11). Lo
mismo con el estado `warm` de los daemons ≤ v0.13: el API solo dice `frozen`.
