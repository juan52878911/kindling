# API /v1 del gateway de sandboxes

La referencia completa es la spec OpenAPI 3.1: [`ext/sandbox/api/openapi.yaml`](../ext/sandbox/api/openapi.yaml).
Un test de contrato (`ext/sandbox/internal/frontal/contrato_test.go`) falla si una ruta
del código falta en la spec o sobra, o si un esquema deja de coincidir con los
`json` tags de su struct.

## Autenticación

Todas las rutas salvo `GET /v1/health` piden `Authorization: Bearer <token>`.
El token es el de un inquilino (sus sandboxes, grafos, plantillas y métricas) o el de
un admin (además `GET /v1/hosts`, `/v1/tenants` y las métricas de todos). En `GET /v1/metrics`
un inquilino sin admin ve solo sus series de máquinas y shells; las del operador (creaciones de
todos los inquilinos y sus tramos, el fondo precalentado, el estado de los hosts) solo las ve un
admin, como `/v1/hosts`. Sin token o con uno
inválido, 401 con `WWW-Authenticate: Bearer realm="kindling-sandbox"`.

Los errores son JSON `{"message": "...", "code": "..."}`; el código HTTP manda y `code`
puede faltar.

El id de un sandbox es `host/máquina`: la barra va sin codificar en la ruta
(`/v1/sandboxes/h1/m-abc/exec`).

## Inquilinos y claves (solo admin)

Los inquilinos de `KLING_SANDBOX_TENANTS` son estáticos. Con `kling sbx gateway -state-dir D`
(o `KLING_SANDBOX_STATE_DIR`) el gateway guarda además inquilinos y claves en
`D/tenants.json` (0600, escritura atómica con rename y fsync) y los administra la API,
solo con token de admin (si no, 403):

| Ruta | Qué hace |
|---|---|
| `GET /v1/tenants`, `GET /v1/tenants/{id}` | Lista o ve; los del entorno salen con `source: env` y `read_only: true` |
| `POST /v1/tenants` | `{name, admin, limits}`; el nombre es el id y no cambia |
| `PATCH /v1/tenants/{id}` | `{admin, limits}`; `limits` sustituye todos los topes |
| `DELETE /v1/tenants/{id}` | Borra el inquilino y sus claves; ver abajo si aún tiene recursos |
| `POST /v1/tenants/{id}/keys` | Crea una clave (`expires_at` opcional); la devuelve UNA vez en `key` |
| `POST /v1/tenants/{id}/keys/{kid}/rotate` | Clave nueva; la vieja vale `grace_minutes` más (10 por defecto, 0 = ya) |
| `DELETE /v1/tenants/{id}/keys/{kid}` | Revoca al instante |

Las claves (`ksk_<id>_<secreto>`) se usan como cualquier token (`Authorization: Bearer`). Del
secreto solo se guarda un hash con sal: no se puede recuperar, y ninguna lectura ni log lo
repite (solo el prefijo `ksk_<id>`, la creación y la último uso, volcado a disco cada minuto).
Los topes del almacén son los mismos de `KLING_SANDBOX_TENANTS` (`max_sandboxes`, `max_shells`,
`max_mem_mib`…) y las cuentas (sandboxes, memoria, procesos, lotes) se aplican igual. Los
inquilinos del entorno no se pueden cambiar por la API (409) y un nombre no puede estar en
los dos sitios (el gateway no arranca). Sin `-state-dir` las escrituras dan 501. Un admin del
almacén no puede quitarse el rol ni borrarse (409).

Borrar un inquilino que aún tiene sandboxes, grafos o volúmenes en algún host da `409` con
cuántos le quedan de cada cosa: si no, un inquilino nuevo con el mismo nombre lo heredaría todo.
Con `?force=true` el gateway los destruye (grafos y sandboxes primero, volúmenes después) y luego
borra el registro; si algo no se deja destruir, `502` y el inquilino sigue. Si un host no contesta
al listar, `503`: sin él no se sabe qué queda. Un puerto público deja de servirse en cuanto su
inquilino no existe.

CLI: `kling sbx tenant ls|show|add|set|rm` y `kling sbx key new|rotate|rm`; `key new` y
`key rotate` imprimen la clave en stdout y nada más. SDKs: `Admin` en Python y TypeScript.

## Crear en lote

`POST /v1/sandboxes` con `"count": N` crea N sandboxes iguales y responde
`{"sandboxes": [...], "errors": [...]}` (también con `count: 1`; sin `count` la respuesta es
el sandbox, como siempre). Es para evaluaciones y entrenamiento: un lote de 16 en una llamada.

- **Topes.** `count` va de 1 al tope del tenant (`MaxLote`) y nunca pasa del global
  (`Opciones.MaxLote`, 16 por defecto): si no, 400.
- **Cuota.** Se cuenta el lote entero antes de crear nada (sandboxes, por plantilla, memoria
  total): si no caben los N, 429 y no se crea ninguno. El 429 de cuota (como el 503 de «plantilla
  reconstruyéndose», 60 s) lleva `Retry-After` (5 s, un intervalo de sondeo: la cuota se libera
  cuando un sandbox se borra o vence su TTL).
- **Reparto.** El sandbox *i* empieza por el *i*-ésimo host candidato (los que tienen la
  plantilla o la imagen y contestan, por hueco libre) y pasa al siguiente si no cabe. Las
  precalentadas se reclaman como en una creación suelta. Hasta 4 creaciones a la vez.
- **`atomic: true`** es todo o nada: tras el primer fallo no se empieza ninguno más, se borran
  los ya creados (y los volúmenes que el lote clonó) y la respuesta es el error de ese fallo.
- **Sin `atomic`**, un lote parcial es **207** con los creados y un `errors` con
  `{index, status, error}` por cada uno que falló; si fallan todos, el error del primero.

## Volúmenes

Los volúmenes son del tenant: otro tenant no los ve ni los monta (404, igual que si no
existieran). Cada uno vive en **un host**, y un sandbox solo monta volúmenes del host donde
corre.

- `GET /v1/sandboxes` acepta `state`, `label=clave=valor` (repetible), `limit` (1-1000) y `cursor`.
  Sin ellos devuelve la lista entera. Con `limit` la respuesta es una página por id y, si hay más,
  `X-Kindling-Next-Cursor` trae el `cursor` de la siguiente (el último id devuelto).
- `POST /v1/sandboxes` acepta `env: {"CLAVE": "valor"}` (hasta 64 variables, 32 KiB): se guarda
  como la sesión `default` del sandbox, y **valen para todo**: `exec` y procesos de fondo sin
  `session` (que usan `default`, con su `cwd`: un `cd` dentro de un `exec` sin sesión también
  se recuerda), `/code` (el intérprete nace con ellas) y `/shell` (se leen al abrir; lo que
  pida la petición va encima). Quien pide otra `session` usa la suya. Cada uso de `default`
  renueva su plazo de 24 h; si el sandbox pasa 24 h sin que nada la use, caduca, las variables
  se pierden y el sandbox sigue sirviendo sin ellas (se recrea con `POST …/sessions`,
  `id: "default"`). Cuenta para `max_sessions`. No va junto con `count`.
- `PATCH /v1/sandboxes/{id}/metadata` `{"labels": {...}, "remove": [...]}` añade, cambia y quita
  etiquetas propias (valor no vacío; las reservadas —`kind`, `tenant`, `template`, `kling.*`—
  dan 400; hasta 32 en total). El daemon solo fusiona etiquetas, así que una quitada queda vacía
  allí y la API no la muestra.
- `POST /v1/sandboxes/{id}/fork` `{"count": 1, "ttl_seconds": N}` ramifica un sandbox vivo en
  `count` copias (memoria, procesos y disco tal como están; hasta el tope de lote del tenant y
  64). Las copias son sandboxes del mismo tenant y **cuentan para la cuota** (todas a la vez o
  `429`), con el `env` y las etiquetas del original pero sin sus puertos públicos. Pausa el
  original un instante.
- `GET /v1/sandboxes/{id}/metrics` da `memory_mib` (PSS en Linux, `footprint` en macOS:
  `memory_kind`), `cpu_seconds` si el daemon la mide, y `mem_limit_mib`; sin lectura (parado)
  esos campos faltan.
- `POST /v1/volumes` `{name, size_mib?, clone_from?, host?}` crea uno vacío o clon de otro
  propio (en su host); `GET /v1/volumes`, `GET /v1/volumes/{name}` y
  `DELETE /v1/volumes/{name}` (409 si está montado). Nombres `[a-z0-9_-]`, hasta 41.
- `POST /v1/sandboxes` acepta `volumes: [{name, mount, read_only, clone_from}]`. El volumen
  tiene que existir (si no, 404) o crearse al vuelo con `clone_from`. Uno existente **fija el
  host**; dos en hosts distintos son 409 con el motivo. Un volumen en escritura lo monta un
  solo sandbox a la vez (409 del daemon): un lote solo admite `read_only` y no admite
  `clone_from`.
- Dueño: el nombre en el daemon lleva un prefijo derivado del tenant y la etiqueta `tenant`.
  El núcleo no copia etiquetas al clonar, así que un clon se reconoce por el prefijo solo.
  El prefijo nuevo lleva 8 bytes del hash del tenant (`v<16 hex>-`); los volúmenes de antes
  (4 bytes) se siguen viendo, pero uno sin etiqueta con prefijo corto que comparten dos tenants
  conocidos no se atribuye a ninguno.
- `MaxVolumenes` por tenant (429) y `MaxVolumenMiB` global acotan lo que se crea, también los
  clones de `volumes[].clone_from` al crear un sandbox: si no caben, 429 y no se crea ninguno.

## Streaming de exec

`POST /v1/sandboxes/{id}/exec` responde `application/x-ndjson`: una línea JSON por
evento, vaciada al momento (`{"stream":"stdout","data":"<base64>"}`), y al final
`{"exit":0,"duration_ms":...}`, o `{"error":"..."}` si el flujo se corta después del 200.
Con `?wait=1` espera y devuelve un único `ExecResult` JSON.

## Procesos en segundo plano

`POST /v1/sandboxes/{id}/processes` (`cmd`, `dir`, `env`, `user`, `session`,
`timeout_seconds`, `max_output_bytes`) lanza un comando y devuelve `201` con su `pid` (el del
sandbox). El proceso sigue vivo aunque el cliente se desconecte. Rutas hermanas:
`GET …/processes` (lista), `GET …/processes/{pid}` (estado; `?wait=N` espera hasta N segundos,
máx. 300), `DELETE …/processes/{pid}` (lo mata y lo olvida),
`POST …/processes/{pid}/signal` (`{"signal":"KILL"}`, vacía = TERM) y
`GET …/processes/{pid}/output?offset=N[&follow=1]`: NDJSON de `ProcessEvent`
(`stream`+`data` base64, `offset` = el siguiente al trozo). El último evento lleva siempre el
`offset` desde el que continuar y, con `follow` y el proceso terminado, `exit`. Un `offset`
anterior a lo que conserva el sandbox se avisa con `dropped`.

Topes por tenant (todos con `429`): procesos vivos por sandbox (`max_processes`, 32 por
defecto) y lecturas con `follow` abiertas a la vez (`max_follow`, 32). Se fijan en
`KLING_SANDBOX_TENANTS` (campos 8.º a 10.º). Como en exec, el sandbox ajeno da `404`.

## Intérprete de código

`/code` y `/code/contexts` del execd de OpenSandbox, con **Python, bash y JavaScript**
(`language`: `python` por defecto, `bash`, `javascript`; este último necesita `node` en la
imagen). `POST /v1/sandboxes/{id}/code/contexts` crea un contexto (un proceso de fondo del
sandbox con un conductor embebido por lenguaje que guarda el estado: variables en Python y
JavaScript; variables, funciones y directorio actual en bash); `GET` los lista,
`GET|DELETE …/contexts/{cid}` lo ve o lo borra (mata el proceso). `POST …/code` con
`{"code","context_id","language","timeout_seconds"}` devuelve NDJSON de eventos `stdout`,
`stderr`, `result` (el valor de la última expresión, en Python y JavaScript), `error`
(`ename`, `evalue`, `traceback`) y `done`; sin `context_id` usa un contexto efímero. Un
`language` que contradice el del contexto es `400`.

- **El flujo siempre acaba en `done`**, también si el gateway pierde el host a mitad: sale
  un `error` (`GatewayError`, sin la dirección del host) y `done` con `ok:false`.
- **Resultados ricos.** `result` trae `text` (el `repr`) y `results`, un mapa por tipo MIME:
  `text/plain` siempre; `text/html` e `image/png` (base64) si el valor tiene `_repr_html_` /
  `_repr_png_` o es una figura de matplotlib. `result` y `done` llevan `execution_count`
  (las ejecuciones del contexto, con esta).
- **Interrumpir.** `DELETE …/code/contexts/{cid}/execution` manda SIGINT al conductor: la
  ejecución en curso acaba con `error` `KeyboardInterrupt` y `done` `ok:false`, y el contexto
  sigue. En bash un `exit` o un estado distinto de 0 es un `error` `ExitStatus`; no mata el
  contexto.
- Un error del código es un evento, no un código HTTP. Topes: 8 contextos por sandbox
  (`429`), 1 MiB de código (`413`), plazo por ejecución 60 s por defecto y 600 s como máximo.
  Sin el intérprete en la imagen, `422`. Los contextos de otro inquilino dan `404`. Una
  ejecución a la vez por contexto (las demás esperan).
- El plazo en Python se impone con `SIGALRM`: un código que atrape `BaseException` o que esté
  dentro de una llamada larga en C no se corta, y el contexto queda ocupado hasta su `DELETE`
  (la respuesta HTTP vuelve igual pasado el plazo; un contexto efímero se mata siempre). El
  de JavaScript corta código síncrono con `vm`; el de bash mata el grupo de procesos de la
  ejecución. El tope de 8 contextos se comprueba antes de crear, sin cerrojo: peticiones
  simultáneas pueden pasarlo por poco, siempre dentro del sandbox del propio inquilino.

## Sesiones

`POST /v1/sandboxes/{id}/sessions` (`id`, `dir`, `env`, `ttl_seconds`) crea una sesión; `GET`
lista, `GET|DELETE …/sessions/{id}` la ve o la borra. Un `exec` o un proceso de fondo con
`"session":"<id>"` corre con el directorio y el entorno de la sesión (y un `cd` o `export`
dentro de un exec la actualiza). Tope de sesiones por sandbox: `max_sessions`, 16 por defecto
(`429`).

## Shell interactiva

`POST /v1/sandboxes/{id}/shell` con `Upgrade: kling-shell/1` cambia el protocolo
(`101`) a tramas binarias `tipo(1) | longitud(uint32 BE) | carga`. La spec documenta los
tipos de trama.

## Ficheros

`/v1/sandboxes/{id}/files?path=...`: GET descarga (o `stat=1` da metadatos, `list=1` lista, `tar=1`
baja el directorio como tar), PUT sube (`mode` octal, `mkdir=1`, máximo 64 MiB; con `tar=1` el
cuerpo es un tar que se extrae dentro de `path`) y DELETE borra. Toda ruta del invitado ha de ser
absoluta y de 4096 bytes como mucho (`PATH_MAX`); si no, `400`.

- **`Range`** en la descarga: `Range: bytes=a-b` (también `a-` y `-n`, un solo rango) da `206` con
  `Content-Range`; fuera del fichero, `416`. El invitado no sabe saltar: el gateway lee y descarta
  lo anterior, así que el coste es el del desplazamiento.
- `POST …/files/move` `{"from","to"}` mueve o renombra (`mv -T`: el destino es el nombre final,
  nunca un directorio donde meter el origen). `409` si el destino existe y no hay `overwrite`,
  si apareció mientras se movía (no se movió nada: el gateway lo comprueba mirando que el origen
  ya no esté) o si `overwrite` cae sobre un directorio que no se puede sustituir.
- `POST …/files/chmod` `{"path","mode":"755","recursive"}` cambia permisos (modo octal de 3 o 4
  cifras; devuelve el `FileStat`), y `POST …/files/replace` `{"path","old","new","all"}` lee el
  fichero, sustituye `old` por `new` y lo reescribe con su modo (sin `all`, `old` debe aparecer
  una vez: `409`; fichero y resultado hasta 8 MiB: `413`; un inquilino tiene como mucho 4 a la
  vez, por encima `429`; no es atómico frente a un proceso del invitado que escriba el mismo
  fichero).
- `POST …/files/mkdir` `{"path"}`
  crea el directorio y los que falten (`201`, con su `FileStat`), y
  `GET …/files/search?path=/dir&glob=*.log` busca por nombre (un `find` dentro del sandbox con
  plazo de 20 s, 1 MiB de salida y 1000 resultados; `truncated` avisa de un corte).

## Errores

Toda respuesta de error es JSON `{"message","code"}`. El `code` es el del daemon si lo trae
(`no_claimable`, `machine_limit`, `name_taken`…) y, si no, el del estado HTTP (`bad_request`,
`not_found`, `method_not_allowed`, `too_many_requests`, `bad_gateway`…). También las rutas
desconocidas (`404`) y los métodos no admitidos (`405`). Un fallo tras el `200` de un flujo
sale como evento `error` con el mismo saneado que un 5xx: sin rutas de socket ni direcciones
de host. El cuerpo de `exec` rechaza campos desconocidos (`400`), como el resto de rutas.

## Entrada HTTP a puertos del sandbox

Con `kling sbx gateway -ingress-domain sbx.example.com` el gateway sirve los puertos de un
sandbox en `<puerto>-<id>.sbx.example.com`, con HTTP y WebSocket. `<id>` es el id del
gateway (`host/máquina`) en base32 minúsculas sin relleno, para que quepa en un nombre DNS
(la etiqueta entera, puerto incluido, no puede pasar de 63 caracteres; si no cabe, la
creación del endpoint da 422).

**DNS y TLS.** Hace falta un registro comodín `*.sbx.example.com` que apunte al gateway y,
si se sirve HTTPS (lo normal: `-tls-cert`/`-tls-key`), un certificado comodín para ese
dominio. Los nombres son uno por (puerto, sandbox), así que no hay forma de evitar el
comodín. Tras un balanceador que ponga el TLS, `-ingress-domain` sigue valiendo: las URL
que devuelve la API usan `https` si el gateway tiene certificado y `http` si no.

**Abrir un puerto.** `POST /v1/sandboxes/{id}/endpoints` con `{"port": 3000,
"ttl_seconds": 3600, "public": false}` (TTL por defecto 1 h, máximo 7 días) devuelve la `url`
y un `token`. El token no tiene estado (HMAC con `-ingress-secret-file`; sin él es uno al
azar por proceso y no sobrevive a un reinicio ni vale en otro gateway): no se puede revocar
antes de que caduque salvo con `DELETE …/endpoints/{port}`, que revoca los tokens de ese puerto
emitidos hasta ese momento (una lista de revocaciones por sandbox en una etiqueta interna, que
`autorizar` comprueba; los tokens nuevos valen) y quita el puerto de los públicos. Con
`"public": true` no hay token: cualquiera con la URL entra hasta que caduque o se revoque.
Revocar también **corta las conexiones ya abiertas** por ese puerto (WebSocket, SSE, descargas
largas), y borrar el sandbox corta todas las suyas. Un 204 es verdad en el gateway que lo
atendió: la petición que estaba resolviendo el sandbox en ese momento tampoco entra (401) y la
caché no se rellena con lo de antes. **Con varias réplicas** del gateway (`gateway.replicas > 1`)
no hay cerrojo ni registro de conexiones compartidos: las revocaciones se repiten hasta ver su
marca escrita (hasta 8 intentos, si no 409), una conexión abierta en otra réplica se corta en
**5 s como mucho** (cada réplica vuelve a autorizar cada 5 s lo que tiene abierto) y una petición
nueva que llegue a otra réplica puede ver las etiquetas viejas hasta 3 s (la caché). Es una cota,
no «nadie entra ya». Las revocaciones concurrentes de un mismo gateway se serializan por sandbox. Si la lista de revocaciones pasa de 2 KiB
se compacta a una marca que revoca todo lo emitido hasta ese momento, de cualquier puerto
(los tokens emitidos después valen). `GET …/endpoints` lista los públicos vigentes; los de token no se listan (no hay estado).

**Autorización.** En el subdominio vale el Bearer del tenant dueño, o el token del endpoint
(solo para ese sandbox y ese puerto) por `Authorization: Bearer`, `X-Kling-Endpoint-Token`
o `?kling_token=`. Este último, pensado para abrir la URL en un navegador, se cambia por
una cookie `kling_ep` HttpOnly del subdominio y una redirección 303 sin el parámetro. El
token del tenant solo se acepta por cabecera. Sin credencial, 401 (igual para un sandbox
que no existe); credencial válida de quien no es dueño, 404. El invitado nunca recibe
`Authorization`, el token de endpoint ni la cookie `kling_ep` (las demás cookies pasan). Un `Set-Cookie` del invitado llega **sin el atributo `Domain`**: la
cookie queda ligada al subdominio del sandbox y no puede fijarse para el dominio de todos
(una aplicación no puede compartir cookies entre sus propios puertos). Conviene registrar el
dominio de ingress como sufijo público (Public Suffix List) o usar uno dedicado. Al arrancar,
el gateway avisa en el log de cada tenant sin `MaxSandboxes` ni `MaxMemTotalMiB`.

**Transporte.** Si el daemon del host anuncia `tcp-tunnel` (sea local o `ssh://`), el
gateway hace de proxy inverso sobre `api.DialTCP` (`GET /machines/{ref}/tcp`, upgrade
`kling-tcp/1`) con el transporte del cliente, unix o ssh: cuerpos binarios sin tope de
tamaño, SSE y WebSocket funcionan también contra hosts `ssh://`. Sin la capacidad (daemon
anterior): un host local publica el puerto del invitado (`api.Publish`) y hace proxy a
`127.0.0.1`; uno `ssh://` cae a `POST /machines/{ref}/guest` (HTTP de texto, cuerpos de
hasta 1 MiB, el puerto tiene que estar en la etiqueta `kling.ports` y un WebSocket da 501).
Un sandbox congelado se despierta (`thaw`) en el primer
acceso. Si nada escucha en el puerto, 502 con el puerto en el mensaje.

**Topes.** Cabeceras de la petición: 32 KiB (`-ingress-max-header-bytes`, 431); conexiones
simultáneas por sandbox, WebSocket incluidos: 64 (`-ingress-max-conns`, 429); espera a las
cabeceras de la respuesta del invitado: 60 s (504). El acceso por la URL no renueva el TTL
del sandbox: ninguna otra ruta del gateway lo hace por actividad.

Ver también: [e2b.md](e2b.md) (la misma API con el protocolo de E2B), los SDKs
([Python](../sdks/python/README.md), [TypeScript](../sdks/typescript/README.md)), las
[recetas](recetas/README.md) y [kubernetes.md](kubernetes.md) (despliegue con Helm).
