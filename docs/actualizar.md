# Actualizar entre versiones

Qué deja kindling en disco, qué se rompe al cambiar de versión y cómo hacer que
pasar de una a la siguiente sea un comando y no una tarde. Escrito el 30 de
septiembre de 2026, sobre `main` tras v0.17, leyendo el código: nada de esto se
ha medido todavía en un host que actualice de verdad.

**Nadie usa kindling en producción.** Es la última vez que se puede cambiar un
formato sin escribir su migración, así que este documento hace dos cosas: fija
las reglas para cuando haya usuarios y lista lo que conviene romper **ahora**,
antes de que haga falta migrarlo.

---

## 1. La regla, en corto

1. **Todo fichero que kindling persiste lleva `"schema": N`.** Sin el campo es
   la versión 0 (lo de antes). `pkg/esquema` es lo común: leer la versión,
   reconocer el fichero del futuro y guardar la copia.
2. **Migrar es hacia delante, de N a N+1, con copia.** Antes de la primera
   escritura que cambia la versión, `<fichero>.v<N>.bak`. La copia buena es la
   primera y nunca se pisa.
3. **Un fichero de una versión mayor no se lee, no se aparta y no se escribe
   encima.** Lo dejó un kling más nuevo; uno viejo que lo "arreglara" perdería
   lo que no conoce. Lo que se niega, se niega diciendo qué encontró y cómo
   volver.
4. **Lo que se puede regenerar se invalida, no se migra.** Un dorado hecho con
   otro Firecracker o con otro kernel no se traduce: se marca obsoleto y se
   vuelve a guardar.
5. **N → N+1 es lo garantizado.** Saltarse versiones funciona si cada migración
   encadena, pero lo que se prueba en CI es la anterior a la actual.

---

## 2. Inventario

Qué persiste kindling, dónde, si lleva versión y qué pasa hoy cuando lo lee un
binario de otra versión. "Nuevo lee viejo" es actualizar; "viejo lee nuevo" es
volver atrás, que también pasa (un `make deploy` desde una rama vieja, §1 de
[`estabilidad.md`](estabilidad.md)).

### Estado del daemon

| Qué | Dónde | Versión | Nuevo lee viejo | Viejo lee nuevo |
|---|---|---|---|---|
| `state.json` | `internal/machine/manager.go` (`load`, `writePending`) | **`schema: 1`** desde este cambio; antes, un array sin nada | lee el array (v0), copia a `state.json.v0.bak`, escribe v1 | un kling ≤ v0.17 no entiende el objeto: lo aparta a `.corrupt-*` y entra en modo protegido (no borra nada; hay que restaurar la copia). Uno futuro con `schema` > 1: **no arranca** |
| Campos de `api.Machine` | `pkg/api/types.go` | por el de `state.json` | los que faltan quedan a cero | **los que no conoce se pierden** en su siguiente `persist` |
| `store/<ns>/<key>.json` | `internal/daemon/store.go` | ninguna; el daemon lo trata como opaco | lo que decida cada dueño (`mcp/links`, `graph/*`, `phone/*`) | igual: sin campo, nadie lo sabe |
| `links.json` de v0.4 | `internal/daemon/store.go` (`comprobarLinksV04`) | — | ya no se migra (PR 11): con uno sin migrar el daemon no arranca y dice que se pase por v0.17 | un v0.4 ya no ve enlaces |
| grafos | `store/graph/*`, `internal/machine/grafo.go` | ninguna | se ignoran los ilegibles y sus aristas cierran | ídem |
| `net-claims` | `/run/kindling/net-claims` (tmpfs) | ninguna; `"<pid> <ns> <hex8>"` | se vacía al reiniciar el host: sin riesgo | un daemon sin reservas solo se protege comprobando las direcciones del host |

### Snapshots y máquinas congeladas

| Qué | Dónde | Versión | Qué pasa al cambiar |
|---|---|---|---|
| `snapshots/<n>/meta.json` | `api.Snapshot`, `internal/machine/meta.go` | **`schema: 1`** desde el PR 2, con `vmm`, `macos` y `kling_version` | sin `schema` es v0: se lee igual y se copia a `meta.json.v0.bak` en la primera reescritura. `editMeta` conserva las claves que no conoce. Uno con `schema` mayor no se restaura, no se anota y no se borra. Un kling ≤ v0.17 lo lee, y al anotarlo pierde `schema` y los campos nuevos (vuelve a ser v0; la copia ya está) |
| `snap.file` + `mem.file` (Firecracker) | el formato es de Firecracker | `vmm` en el meta (PR 2) y en el sello de las congeladas | otro VMM u otra MAJOR.MINOR de Firecracker: el dorado sale `stale` en `kling template ls`/`inspect` y `run -from` da `409` con la orden para rehacerlo, sin llegar al VMM; una congelada no se descongela y se dice con cuál se congeló. Un parche no invalida. Los dorados sin `vmm` (v0) se intentan como siempre. El TSC sigue con su traducción (`explainRestoreErr`) |
| kernel del dorado | `kernel_sha256` en el meta | hash, no versión | cambiarlo solo da un aviso (`avisoKernel`): el kernel del invitado vive en `mem.file` y sigue restaurando |
| sello de congelación | `machines/<id>/volcado.{en-curso,ok}` + `sello`, `internal/machine/volcado.go` | ninguna; lleva `vmm` desde el PR 2 (opcional) | sin marcadores se acepta (máquina anterior al sello); sin `vmm`, no se compara |
| `snap.file` de kling-vz | `vz/internal/spec/spec.go` | **`kling_vz: 2`** desde v0.18 (la 2 es `graphics`); `Decode` lee 1 y 2 | uno ≤ v0.17 rechaza la 2 con su error de siempre; uno de una versión futura se rechaza diciendo que se actualice kling-vz |
| `mem.file` de kling-vz | `SaveMachineStateToPath` de Apple | `macos` en el meta (PR 2) | no invalida por adelantado (no hay regla conocida); si restaurar falla y el macOS o el kling-vz no son los del dorado, el error lo dice y da la orden para rehacerlo. Un `kling_vz` que el kling-vz no entiende se traduce igual |
| firma de dorados | `secrets/snapshot.key`, `internal/machine/firma.go` | ninguna | la firma no cubre `kernel_sha256`, IPv6 ni anotaciones, así que añadir campos no la rompe |

### Imágenes y volúmenes

| Qué | Dónde | Versión | Qué pasa al cambiar |
|---|---|---|---|
| `images/<n>.ext4`, `.layer.ext4`, `vmlinux` | `internal/machine/layer.go` | ninguna; la forma se deduce de qué ficheros hay | nada que migrar. Una base sin `kling.layer` se detecta (`baseSupportsLayers`) |
| `images/<n>.recipe.json` | `api.ImageRecipe` | ninguna; `kling_version` es informativo | lectura laxa: ilegible es base `min` y sin techo de CPU. El de Android se escribe sin `durable` |
| `kling-guest` dentro de la imagen | `cmd/kling/builder*.go`, `internal/android` | desde v0.18, `/healthz` con `Accept: application/json` da `agent`, `version` y `caps`; el daemon lo guarda en `api.Machine.Agent` | un agente anterior contesta `ok` y se sigue detectando por sondeo: 404/405 en `/resync`, `/ready`, `/hooks`, o sin cabecera `X-Kling-Share`. Actualizarlo es reconstruir la imagen; solo MCP tiene `refresh-bridge`. En una imagen de Docker sin `sh` es también el init (`/sbin/overlay-init` enlaza a él, `built.init` = `go` en la receta): lo mismo, se reconstruye |
| caché OCI (`cache/oci`, `cache/builder/oci`, `cache/verified/oci`) | `internal/oci`, `internal/daemon/builders_cache.go` | direccionada por contenido | sin riesgo: la verificada se crea y se llena sola en la primera construcción sin root; un kling anterior no la lee y rehashea como siempre. Se puede borrar entera. Lo que sube `image import -archive` vive ahí también: una receta con `source: archive` solo se rehace mientras sus blobs sigan en la caché (si no, hay que volver a importar el archivo); un daemon sin la capacidad `oci-blobs` no la entiende y el CLI lo dice antes de subir nada |
| volúmenes y sus snapshots | `volumes/*.ext4`, `internal/machine/volume*.go` | sin metadatos | sin riesgo mientras sean ext4 |

### Secretos

| Qué | Dónde | Versión | Qué pasa al cambiar |
|---|---|---|---|
| `secrets/snapshot.key` | `internal/machine/firma.go` | ninguna, 32 bytes | firma y, por HKDF, cifra. Si se pierde o se regenera, **ningún dorado firmado ni almacén de credenciales vuelve a abrirse** |
| `credentials.enc` (máquinas, plantillas y secretos de grafo) | `internal/machine/credenciales.go` | **cabecera `KLCS` + `0x01`** delante del nonce desde el PR 3 (y la cadena HKDF `"kindling credential store v1"`) | sin cabecera es v0: se lee y, al volver a sellarlo, se copia a `<fichero>.v0.bak` (quitar todas las credenciales borra también la copia). Con una versión mayor no se descifra, no se pisa y no se borra. Un kling ≤ v0.17 no descifra un v1. `NormalizarAlmacen` sigue arreglando los postgres viejos. Un binario de la misma versión que funde y vuelve a sellar **pierde los campos que no conoce**: añadir uno que importe sube el byte |
| `credaudit` | `audit/<id>.jsonl` (Linux), `machines/<id>/credaudit.jsonl` (Mac) | ninguna, JSONL | el lector salta las líneas que no entiende; `prepararAuditoria` mueve el sitio viejo al nuevo |
| `registries.json` | `internal/daemon/registries.go` | ninguna; `{"auths": {"<registro>": {"username", "password"}}}`, root 0600, en claro | un daemon anterior no lo lee: importa solo de registros públicos, como siempre. Los campos que no conoce se pierden al guardar otro `kling registry login` |

### Extensiones, configuración y servicios

| Qué | Dónde | Versión | Qué pasa al cambiar |
|---|---|---|---|
| `manifests.json` | `pkg/plugin/cache.go` | **`format: 1`**, y cada entrada con la versión de kling y la API | caché: una versión distinta la tira entera y la rehace. Correcto: es regenerable |
| manifiesto de extensión | `pkg/plugin/manifest.go` | **`ManifestVersion` 2** (acepta 1–2), `min_kling`, `max_api` (v0.18) | fuera de rango: la extensión sale con error en la lista, sin tumbar nada. Un `max_api` menor que el API del daemon es un aviso en `kling plugins` |
| sidecar `.json` de cada extensión | `pkg/plugin/install.go` | ninguna | versión, url, sha256, fecha |
| `~/.config/kling/config.json` | `pkg/config/config.go` | ninguna | claves desconocidas se ignoran |
| `/etc/default/kling`, `/etc/kling/gateway.env` | `Makefile`, `ext/mcp/Makefile` | ninguna | se crean si no existen y **no se tocan nunca más**: una variable nueva no llega a un host viejo |
| unidades de systemd | `packaging/kling.service`, `ext/*/…` | ninguna | solo `make deploy` las reescribe. `install.sh` y `kling up` no |
| launchd | plantilla en `docs/mac.md` | ninguna | nada la genera; `kling up` solo la carga si el daemon no responde |
| tokens de `ext/phone` | almacén `phone/<id>` y `phone-tokens/…/<id>.json` | ninguna | — |
| anotación `phone` del dorado | `ext/phone/golden.go` | lleva `daemon_version` y la de `phoned`, **no la de su formato** | — |
| `kling-phoned` | `prototypes/android/phoned`, API `/v1/*` | `/v1` y `version` en `/v1/health` | es lo único con prefijo de API; vive en el invitado |

### Protocolos

| Entre | Cómo se versiona hoy | Si se mezclan |
|---|---|---|
| CLI ↔ daemon | `GET /info` da `Version`, `Capabilities` (solo se añaden, nunca se reusan: `internal/daemon/server.go`) y, desde v0.18, `api`; cada respuesta lleva `X-Kling-API` y `X-Kling-Version`. Sin `/v1` | `pkg/api` compara el API en cada petición: aviso si el daemon es más nuevo, error claro si es más viejo que `MinDaemonAPI`. `kling version` y `kling doctor` avisan si las versiones difieren |
| extensión ↔ daemon | `Info.Has(cap)` para cada función nueva, `min_kling` y `max_api` en el manifiesto, y la misma comprobación de `X-Kling-API` que el CLI (usan `pkg/api`) | extensión vieja con núcleo nuevo: funciona mientras no se quite una ruta, y con un API mayor avisa. Nueva con núcleo viejo: 404 o `Has` falso |
| daemon ↔ kling-vz | `GET /kling/info` con `credential_kinds` (`http-places`, `graph-link`…) | falta un tipo: "rebuild kling-vz". **La versión de kling-vz no se compara** |
| host ↔ `kling-guest` | `/healthz` JSON con `version` y `caps` (v0.18); sondeo por ruta para los anteriores | host nuevo, invitado viejo: degradación ruta a ruta. Host viejo, invitado nuevo: nada lo nota. Un puente viejo con una opción de volumen nueva **muere, y como es PID 1, el invitado entra en pánico**; desde v0.18 el agente ignora lo que no conoce |

### Cómo llega un binario nuevo al disco

| Camino | Verifica | Cambia atómicamente | Para/reinicia el daemon | Vuelta atrás |
|---|---|---|---|---|
| `kling upgrade` (PR 8-9) | sha256 contra `SHA256SUMS`, los esquemas que lee el nuevo, que arranca | sí (temporal + `rename`) | sí: `systemctl stop/start` o `launchctl bootout/bootstrap` | sí, sola si el nuevo no contesta o pierde algo, y `-rollback` |
| `scripts/install.sh` | sha256 contra `SHA256SUMS`, **antes** de escribir | sí (temporal + `mv -f`) | no | no; sobre una instalación existente remite a `kling upgrade` (o `--force`) |
| `kling plugin install` | sha256 contra `SHA256SUMS`, manifiesto, `min_kling` | sí, companions primero | no | no |
| `make deploy` | — | `scp` | sí, `restart` | no |
| `kling up` | no baja nada | — | `enable --now` (**no reinicia uno vivo**) | — |

`SHA256SUMS` no está firmado. `kling-guest`, los constructores y las unidades
del host solo se actualizan con `make deploy`.

### Lo que más riesgo tiene

1. **Un binario viejo que reescribe un fichero nuevo pierde campos en
   silencio**: `meta.json` (`editMeta`), `credentials.enc` (la fusión) y
   `state.json` en la misma versión de esquema. Es el fallo que no avisa.
   *Desde el PR 2, `editMeta` conserva lo que no conoce; en `credentials.enc`
   y `state.json` la defensa es subir la versión (§3.1).*
2. **Dorados contra otro Firecracker**: no se sabe con cuál se hicieron, y el
   fallo sale al despertar, lejos de la causa. Es el patrón exacto de §2 de
   [`estabilidad.md`](estabilidad.md). *Resuelto para los dorados nuevos en el
   PR 2: el meta lo guarda y salen `stale`; los de antes no lo guardan.*
3. **`snapshot.key`**: todo lo firmado y cifrado depende de 32 bytes sin copia.
   Una reinstalación que la regenere deja inservibles dorados y credenciales.
4. **`kling-guest` horneado sin handshake**: nadie sabe qué agente lleva cada
   imagen, y el caso del puente viejo es un pánico, no un error.
5. **Actualizar no para, no verifica y no vuelve**: `install.sh` cambia el
   binario con el daemon viejo corriendo, y nada comprueba que el nuevo arranca.
   *Resuelto en los PR 8-9: `kling upgrade` (§3.4).*

---

## 3. La estrategia

### 3.1 Formatos: `schema` en todo lo que se escribe

`pkg/esquema` da tres piezas: `Comprobar(ruta, datos, soportada)`, que
devuelve la versión o `*ErrMasNuevo`; `Respaldar(ruta, v)`, la copia `.v<v>.bak`
que no se pisa; y `Cabecera`, el campo `schema` para incrustar. Cada dueño de un
fichero escribe su `switch` de versiones (como `decodificarEstado` en
`internal/machine/manager.go`). No hay registro central de migraciones: con
diez ficheros no lo necesita.

Qué hace cada uno según lo que encuentre:

| Encuentra | Fichero de estado (`state.json`, `meta.json`, `credentials.enc`) | Caché (`manifests.json`, recetas derivadas) |
|---|---|---|
| sin fichero | vacío | vacío |
| versión menor | copia `.v<N>.bak`, lee, escribe la actual | se tira y se rehace |
| la misma | normal | normal |
| **versión mayor** | **no arranca / no toca ese fichero**, con mensaje | se ignora, **sin escribir encima** |
| ilegible | cuarentena (`.corrupt-*`) y modo protegido, como ya hace `state.json` | se tira |

`credentials.enc` es binario: la versión va en una cabecera delante del nonce
(`KLCS` y el byte `0x01`), no dentro del JSON cifrado, para poder rechazar sin
descifrar; y va también en el dato autenticado, para que no se pueda cambiar.
La magia de cuatro bytes es porque un v0 empieza por un nonce aleatorio: con un
byte solo, uno de cada 256 almacenes viejos parecería llevar versión. Con
cuatro es uno en 2^32, y aun ese se cubre: una cabecera v1 que no descifra se
prueba como v0 antes de dar el error.

**Campos desconocidos en la misma versión.** Es el riesgo 1. Dos defensas,
baratas: (a) **subir la versión cuando se añade un campo que un binario viejo
no debe perder** —es lo que convierte la pérdida silenciosa en un rechazo—; y
(b) en `meta.json`, que se edita a menudo, conservar lo desconocido: leer el
JSON a un `map[string]json.RawMessage` además del struct y volver a escribir
las claves que el struct no tiene. Con (a) basta para el resto.

### 3.2 Compatibilidad: qué se promete

Mientras sigamos en 0.x:

- **PATCH (0.17.x):** ningún formato cambia de versión. Se puede ir y volver.
- **MINOR (0.18):** puede subir la versión de un formato, siempre con
  migración desde la anterior y copia. **Volver** a la anterior es restaurar
  las copias `.bak` (lo hace `kling upgrade --rollback`, §3.4). Se garantiza
  N-1 → N; de más atrás, encadenando, sin prueba.
- **Dorados y congeladas:** no se migran nunca. Son caché cara, no datos.
- **Imágenes:** sobreviven a cualquier actualización. Un agente de invitado
  viejo dentro sigue funcionando con lo que tuviera (degradación, no fallo),
  durante al menos una MINOR; después, `kling doctor` avisa y `kling image ls`
  lo marca.

**Dorados obsoletos.** El meta guarda lo que los ata al host: la versión de
Firecracker (o de kling-vz y de macOS), el `kernel_sha256` que ya hay y la
versión de kling que lo hizo. Al restaurar, si el VMM no es el mismo:

- se marca `stale` con la causa (`firecracker 1.12 → 1.17`), visible en
  `kling snapshots` y en `doctor`;
- **no se intenta** restaurar a ciegas: el fallo crudo de Firecracker al
  despertar es justo el síntoma desfasado de la causa que hay que evitar;
- `kling upgrade` los recongela solo al final si se puede (dorados de MCP y de
  `db` saben rehacerse: `kling mcp import`, `kling db golden`); los hechos a
  mano con `kling save` quedan marcados con la orden para rehacerlos.

Cómo quedó en el PR 2 (`internal/machine/meta.go`): `stale` **no se guarda**,
se calcula al leer comparando el `vmm` del meta con el `--version` del VMM que
usa el daemon, así que volver al Firecracker de antes lo quita solo. Cuenta
como obsoleto otro VMM u otra MAJOR.MINOR de Firecracker (el formato del
volcado es suyo y lo sube en versiones menores; un parche no lo cambia). La
versión de kling-vz no se compara: su volcado lleva `kling_vz` y el propio
kling-vz rechaza el que no entiende, error que el daemon traduce a "rehazlo".
La de macOS tampoco, porque no hay regla conocida: solo se menciona si la
restauración falla. Afinar Firecracker con `firecracker --snapshot-version`
(el número de formato en vez de la versión) queda para cuando se pueda probar
en el laboratorio.

Las máquinas **congeladas** con otro VMM no tienen quién las rehaga:
`kling upgrade` las descongela y para (o avisa y pide `--force`) **antes** de
cambiar el binario, que es cuando todavía se puede.

### 3.3 Protocolos: preguntar antes de suponer

Nada de `/v1` en la API del daemon: `Capabilities` ya funciona y es más fino.
Lo que falta:

- **CLI y extensiones ↔ daemon:** añadir `api` (un entero, hoy `1`) a `/info`.
  Sube solo si se quita o cambia una ruta, no al añadir. El cliente con una
  `api` mayor que la suya dice "este kling es más viejo que el daemon" en vez
  del 404. El manifiesto de extensión gana `max_api` (no `max_kling`: la
  versión dice poco, la API dice lo que importa).
- **daemon ↔ kling-vz:** comparar también `version` de `/kling/info`; distinta
  de la del daemon es un aviso en `doctor`, y un `kling_vz` de snapshot que no
  entiende, lo que ya es.
- **host ↔ invitado:** `/healthz` devuelve `{"version", "caps": [...]}` además
  del `ok` que ya sirve (un invitado viejo sigue contestando `ok` y se trata
  como "sin caps", exactamente lo que hoy se deduce por 404). El daemon guarda
  la versión del agente en el `recipe.json` al construir, y la de cada máquina
  al arrancar. Con eso, "qué imágenes llevan un agente viejo" es una consulta,
  no una arqueología.
- **Parámetros de kernel:** el agente y el puente **ignoran** los `kling.*`
  que no conocen, con un aviso. Es lo que evita el pánico de PID 1, y hay que
  hacerlo antes de que existan imágenes de usuarios con el agente de hoy.

Mezclar versiones queda así:

| Mezcla | Resultado |
|---|---|
| CLI nuevo, daemon viejo | funciona con lo que el daemon anuncie; lo que no, "actualiza el daemon" |
| CLI viejo, daemon nuevo | funciona mientras `api` sea la misma |
| extensión fuera de `min_kling` | sale con error en `kling plugins`, no se ejecuta |
| extensión con `max_api` menor que el API del daemon | aviso en `kling plugins` y en sus propias peticiones; se ejecuta |
| invitado viejo | funciona sin las `caps` que le falten; `doctor` lo cuenta |
| kling-vz de otra versión | aviso; error si falta un `credential_kind` o el `kling_vz` no casa |

### 3.4 El flujo: `kling upgrade`

Una orden nueva, no más banderas en `kling up`: `up` es "que esto funcione",
`upgrade` es "cambia lo que funciona por otra cosa y, si no, déjalo como
estaba". Comparten `-check`.

```sh
kling upgrade                  # a la última estable
kling upgrade --tag v0.18.0
kling upgrade --dry-run        # qué haría, sin tocar nada
kling upgrade --rollback       # vuelve a la anterior con sus copias
```

Pasos, en orden, parando en el primero que falle:

1. **Plan.** Versión actual y destino, qué formatos van a migrar (el nuevo
   binario lo sabe: `kling-nuevo upgrade --plan-from <versión>` imprime la lista),
   qué dorados quedarán obsoletos, qué extensiones hay y si el destino las
   trae. Con `--dry-run`, termina aquí.
2. **Descarga y verificación.** Todos los binarios de la release (`kling`,
   `kling-guest`, `kling-chispa`, `kling-vz` en Mac, cada extensión instalada y
   sus companions) a `~/.local/state/kling/upgrade/<tag>/`, verificados contra
   `SHA256SUMS` como hace `install.sh`. Nada ha cambiado todavía.
3. **Prueba en seco del nuevo.** `kling-nuevo version` y `kling-nuevo up -check`
   contra la configuración actual. Si el nuevo no arranca aquí, no llega a
   tocar nada.
4. **Drenar.** Congelar lo congelable con el VMM **actual** si el VMM no
   cambia; si cambia, parar (o `--force`). Parar el gateway y las unidades de
   las extensiones.
5. **Copia.** `~/.local/state/kling/upgrade/<tag-anterior>/` guarda los
   binarios actuales, las unidades y `/etc/default/kling`. Los ficheros de
   estado ya se copian solos (`.v<N>.bak`) al migrar.
6. **Cambio atómico.** Cada binario por `rename` en su directorio. En Linux,
   `systemctl restart kling` (el `KillMode=process` deja vivas las microVMs);
   en Mac, `launchctl kickstart -k`. Las unidades se regeneran desde la
   plantilla de la release, conservando `/etc/default/kling` y añadiendo, en
   comentario, las variables nuevas.
7. **Migrar.** Lo hace el propio daemon nuevo al arrancar (§3.1): no hay paso
   de migración aparte que se pueda olvidar.
8. **Verificar.** `kling up -check`, `/info` con la versión esperada, las
   máquinas que había siguen en `kling ps`, las extensiones pasan su gancho
   `status`. Recongelar dorados obsoletos que sepan rehacerse.
9. **Vuelta atrás si 7 u 8 fallan:** parar, restaurar binarios y unidades de la
   copia, restaurar los `.v<N>.bak` de lo que migró, arrancar el viejo. Es
   exactamente `kling upgrade --rollback`, que también se puede pedir a mano
   mientras exista la copia (se guardan las dos últimas).

Contra un host remoto (`KLING_HOST`), igual que `kling up` hoy: imprime la
orden de `ssh` en vez de ejecutarla. En Mac el daemon y el CLI son el mismo
host y todo es local.

`install.sh` sobre una instalación existente pasa a decir "ya hay un kling
v0.17.0; usa `kling upgrade`" y solo sigue con `--force`. Con `--with` y sin
`--tag` sí sigue: añade esas extensiones y deja kling como está.

**Cómo quedó (PR 8-9).** `internal/upgrade` es el flujo, con lo que se puede
probar sin host (un servidor de releases falso, un daemon y un servicio de
mentira); `cmd/kling/upgrade.go`, lo de cada sistema.

```sh
sudo kling upgrade                     # Linux: el daemon de kling.service
kling upgrade                          # Mac: el agente de launchd de docs/mac.md
kling upgrade -tag v0.18.0             # -version es lo mismo
kling upgrade -from-dir ./dist         # sin bajar nada (el e2e)
kling upgrade -dry-run                 # -check es lo mismo
kling upgrade -rollback
kling upgrade -cli                     # solo este binario: un CLI sin daemon
```

Los pasos son los de arriba, con estas diferencias, y por qué:

- **Dónde.** Lo bajado y las copias van en `<raíz del daemon>/upgrade/`
  (`<etiqueta>/`, que se borra al acabar, y `backups/<fecha>-<versión>/` con un
  `manifest.json`, las dos últimas), no en `~/.local/state`: en Linux corre con
  `sudo` (y entonces `~` es el de root o el del usuario según la distribución),
  y la copia es de ese daemon, no de quien teclea. Un CLI sin daemon (`-cli`, o
  un contexto remoto) sí usa `~/.local/state/kling/upgrade/`. Dos `kling
  upgrade` a la vez sobre la misma raíz no pueden correr: comparten la descarga
  (que cada uno borra al acabar) y el servicio, así que el segundo se niega con
  un `flock` sin espera sobre `upgrade/.lock` (también `-rollback` y
  `-dry-run`). Con un contexto `ssh://` se imprime la orden para el host del
  daemon con sus banderas (`-dry-run`, `-force`, `-unit`, `-root`): un
  `-dry-run` que se perdiera por el camino sería una actualización de verdad.
- **Qué se cambia.** El binario que ejecuta el daemon (`/proc/<MainPID>/exe` de
  la unidad, o el `program` de launchd), y a su lado lo que ya esté instalado:
  `kling-guest` y `kling-chispa` en el `KLING_LIB_DIR` del daemon (Linux),
  `kling-vz` junto a `kling` (Mac, comprobando antes de parar nada que lleva el
  permiso de virtualización). Las extensiones instaladas con `kling plugin
  install` se pasan a la misma versión después de verificar el núcleo (con sus
  compañeros, y guardadas en la misma copia para que `-rollback` las devuelva);
  con `sudo` no se tocan las de root y se dice que cada usuario corra `kling
  upgrade -cli`. Las unidades de las extensiones (`kling-gateway`…) siguen con
  el binario que arrancaron hasta que se reinician.
- **Plan.** En vez de `kling-nuevo upgrade --plan-from <versión>`, el nuevo
  dice qué sabe leer (`kling upgrade -schemas`: su versión y los esquemas de
  `state.json`, `meta.json` y credenciales, `machine.EsquemasSoportados`) y el
  que actualiza lo compara con lo que hay en disco (`machine.EsquemasEnDisco`,
  que solo lee cabeceras). Algo que el nuevo no sabría leer para **antes** de
  tocar nada, diciendo qué fichero; lo que migrará sale en el plan. Un kling
  anterior sin `-schemas` (≤ v0.17) se trata como lo que era: esquemas 0, y los
  `meta.json` sin mirar. Uno con `-schemas` ya no trae las migraciones de PR 11:
  un `state.json` de v0.13, un `meta.json` de v0.4 o un `links.json` sin
  migrar también lo paran antes (`machine.ObsoletosEnDisco`), con el mensaje
  con el que el daemon se negaría.
- **Qué daemon.** El que contesta en el socket (`-host`, `KLING_HOST`) tiene que
  ser el proceso de la unidad (`-unit`, por defecto `kling`) o del agente de
  launchd: se compara su PID con el del otro lado del socket (`SO_PEERCRED`,
  `LOCAL_PEERPID`). Con `KLING_HOST` en un daemon privado, sin esto se
  reiniciaría el de producción y se verificaría el privado.
- **Prueba en seco.** Es ese `-schemas` (o `version`) del binario bajado, sin
  hablar con ningún daemon. No se corre `kling up -check`: diagnostica el host,
  que no cambia, y sale en rojo por avisos que no tienen que ver.
- **Versiones.** La misma o una anterior (por sus tres números; un build de
  desarrollo de la misma no lo es) piden `-force`; una etiqueta que no dice lo
  mismo que el binario bajado es un error.
- **Drenar y recongelar.** No hace falta: Firecracker no viene en la release,
  así que el VMM no cambia y ningún dorado ni congelada queda obsoleto; las
  microVMs en marcha siguen (`KillMode=process`) y el daemon nuevo las readopta.
  En el Mac `kling-vz` sí cambia, pero su versión no se compara (§3.2).
- **Unidades y `/etc/default/kling`.** No se regeneran: la release no publica
  `kling.service` (lo instala `make deploy`), y lo que no se cambia no hace
  falta copiarlo.
- **Verificar.** `/info` con la versión del binario nuevo dentro del plazo
  (`-timeout`, 60 s), y que siguen las máquinas congeladas (congeladas, o
  despiertas si un cliente las pidió) y las paradas, y todos los dorados. Las
  que corrían no se exigen: el gateway crea y borra las de cada sesión. La foto
  de antes se toma justo antes de parar, no al empezar: la descarga puede durar
  minutos. Desde que se para, una señal no corta nada a medias: las órdenes
  van sin cancelar (con su plazo) y Ctrl-C solo acorta la espera, que entonces
  vuelve atrás entera. El `state.json` va a la copia ya parado el daemon, y no
  con los binarios: lo que el viejo escribió entre tanto no se pierde al volver.
  Si algo falla, vuelve sola: para, devuelve binarios,
  el `state.json` de la copia y cada `.v<N>.bak` que no estaba antes (y lo
  borra, para que la siguiente migración la vuelva a hacer), arranca y espera
  a la versión de antes; esa copia ya no sirve y se borra (las copias se podan
  solo tras una actualización buena, así que dos intentos fallidos no se
  llevan la del último bueno). El gancho `status` de las extensiones no se
  corre.
- **Congeladas que el nuevo despertó.** Volver atrás deja un `state.json` (el
  de la copia, o el `.bak` de la migración) donde siguen congeladas máquinas
  que un cliente despertó en cuanto el daemon nuevo contestó, y que desde
  entonces escriben en su disco. El viejo las despertaría cargando el
  `mem.file` de antes sobre un disco cambiado después: memoria y disco ya no
  casan y el sistema de ficheros del invitado puede romperse en silencio. Así
  que, antes de parar para volver, se comparan con el `List` del daemon nuevo y
  se congelan otra vez con él (el VMM no cambia, así que su `mem.file` lo lee
  el viejo). Si no se puede (no contesta y su `state.json` dice que corren, o
  el freeze falla), no se vuelve atrás y se dice cuáles: el nuevo sigue
  corriendo, la copia se queda, y `-rollback -force` vuelve igualmente. Lo
  creado después sí se pierde, como dice §6.
- **`-rollback`.** Lo mismo con la última copia, menos el `state.json`
  guardado (se usa el `.bak` de la migración, que es lo que dice §6); la copia
  usada se borra, así que el siguiente `-rollback` va a la anterior. Funciona
  también con el daemon caído, que es cuando más falta hace (el nuevo no
  arranca tras un reinicio, o la vuelta atrás sola falló): la raíz y el socket
  salen de cómo lo arranca su servicio (`-root`/`-socket` del `ExecStart`, si
  no `KLING_ROOT`/`KLING_SOCKET` de `Environment=` o del `EnvironmentFile`, de
  los que solo se leen esas dos claves; en el Mac, del plist), o de `-root DIR`;
  lo que se restaura, del `manifest.json` de la copia. No hay PID que comparar:
  en su lugar, el socket del servicio tiene que ser el que se espera.

### 3.5 Pruebas

- **Fijaciones de estado viejo en `testdata/`.** Por cada formato versionado,
  un fichero real de cada versión anterior (los de v0.17 en
  `internal/machine/testdata/esquema/v0.17/`), y un test que lo carga con el
  binario actual y comprueba que no pierde nada
  (`internal/machine/fijaciones_v017_test.go`). Se generan una vez, con el
  `pkg/api` de esa versión, y no se tocan: son la definición de "lo que había".
- **Un fichero del futuro por formato**: `schema` = actual + 1 con un campo
  inventado, y el test de que se rechaza sin pisarlo (como
  `TestEstadoMasNuevoSeRechazaSinPisarlo`).
- **Guarda de CI:** un test que falla si cambia un struct persistido
  (`api.Machine`, `api.Snapshot`, `credproxy.Credential`, `api.CredentialSpec`)
  sin tocar su constante de versión, comparando los campos con una lista
  comiteada (`TestCamposPersistidos`, `internal/machine/campos_persistidos_test.go`).
  Es lo que impide el riesgo 1 por descuido. Si solo se añaden campos, se
  regenera la lista con `go test ./internal/machine -run TestCamposPersistidos
  -update` después de decidir si un kling anterior los puede ignorar.
- **e2e "desde la anterior"** (`scripts/94-e2e-upgrade.sh`): instalar la
  etiqueta N-1 con `install.sh --tag`, crear estado (una máquina parada, una
  congelada, un dorado, un volumen, una credencial, un enlace de MCP), `kling
  upgrade` al binario de HEAD (`--from-dir` para no bajarlo), y verificar que
  todo sigue, que los `.bak` están, y que `--rollback` vuelve a N-1 con el
  estado intacto. Necesita KVM, así que no corre en `ci.yml`: en el laboratorio
  y en Mac (`92-e2e-mac.sh` ya sabe levantar su daemon), y **obligatorio antes
  de cada `release.sh`**. Sin VM, en CI sí puede correr la parte de ficheros:
  arrancar el `Manager` de N-1 y el de HEAD sobre el mismo directorio.

  Cómo quedó (PR 10), en Linux: `sudo OLD_DIR=… NEW_DIR=… scripts/94-e2e-upgrade.sh`
  monta un daemon privado de N-1 con su propia unidad de systemd de tiempo de
  ejecución (`/run/systemd/system/<NAME>.service`, raíz en `/srv/<NAME>`), sin
  tocar el del sistema; sin `OLD_DIR` baja `OLD_TAG` con `install.sh`. Con N-1
  crea una máquina congelada (con una marca en su RAM), una parada, un dorado
  y un volumen; luego prueba una actualización a un binario que no arranca
  (tiene que volver sola), la de verdad con `-from-dir` (y su `-dry-run`), que
  la congelada despierte con su marca, que el dorado sirva para `run -from` y
  la parada para `start`, y `-rollback` a N-1 con el `state.json` de antes. La
  credencial y el enlace de MCP quedan fuera (son de extensiones). Con
  binarios sin su versión sellada (los dos `dev`) se para al principio y lo
  dice. En Mac, pendiente: el camino de launchd tiene tests unitarios pero no
  e2e, así que [`releases.md`](releases.md) lo pide a mano (paso 3b) antes de
  cada etiqueta.

---

## 4. Qué romper ahora

Antes de que haya usuarios cada uno cuesta un commit; después, una migración y
su prueba para siempre.

| Prioridad | Qué | Por qué ahora |
|---|---|---|
| **P0** | `schema` en `state.json` | **hecho** en este cambio: sin él no hay forma de rechazar un fichero del futuro |
| **P0** | `schema` en `meta.json` y versión de Firecracker/kling-vz/macOS y de kling en el dorado | **hecho** (PR 2): es lo que permite marcar obsoletos en vez de fallar al despertar |
| **P0** | byte de versión en `credentials.enc` | **hecho** (PR 3): es binario, y añadirlo después obliga a adivinar si un fichero lo tiene |
| **P0** | el agente y el puente ignoran `kling.*` desconocidos | **hecho** (PR 4). Los `kling.*` ya se buscaban por nombre; lo que mataba a PID 1 era una opción nueva dentro de `kling.volume`, que el puente anterior a `:ro` pegaba al directorio |
| **P1** | `/healthz` del agente con `version` y `caps` | **hecho** (PR 4): lo que se hornea hoy es lo que habrá que soportar |
| **P1** | `api` en `/info` y `max_api` en el manifiesto | **hecho** (PR 5): el manifiesto ya tenía versión; añadir el campo era gratis |
| **P1** | subir `kling_vz` a 2 por `graphics` | **hecho** (PR 5): un kling-vz anterior restauraba en silencio con otros dispositivos |
| **P1** | `schema` en el almacén (`store`) para `mcp/links`, `phone/*` y grafos | los dueños son nuestras extensiones; poner la convención antes de que haya extensiones de otros |
| **P2** | quitar `migrateLinks` y `liftV04` | **hecho** (PR 11), con el alias `warm`: lo de esa época se rechaza diciendo que se pase por v0.17 |
| **P2** | recetas de Android con `durable.Escribir` | lo único que escribe estado sin la escritura segura |
| **P2** | `snapshot.key`: aviso en `doctor` si no hay copia y `kling secrets export` | no rompe formato, pero es el único fichero cuya pérdida no tiene arreglo |

Lo que **no** hace falta romper: la API del daemon sin `/v1` (las capacidades
bastan), el formato de las imágenes (no tiene metadatos que versionar) y
`net-claims` (vive en tmpfs y se vacía al reiniciar).

---

## 5. Plan en PR pequeños

En orden; cada uno se puede fusionar solo y deja `main` mejor que antes.
Esfuerzo: S ≈ medio día, M ≈ uno o dos días, L ≈ una semana.

| # | PR | Qué incluye | Esfuerzo |
|---|---|---|---|
| 1 | **`pkg/esquema` y `state.json` v1** | helper común, `schema` en `state.json`, copia `.v0.bak`, el daemon no arranca con uno del futuro. Tests: versión 0 migra con copia; futura se rechaza sin pisarla | S — **hecho** |
| 2 | `meta.json` v1 y dorados obsoletos | `schema`, `vmm` (`firecracker 1.17.0` / `kling-vz 0.18.0`), `macos` y `kling_version` en el meta; `stale` con causa en `kling template ls`/`inspect` (en `doctor`, pendiente); `run -from` y `thaw` se niegan con la orden; conservar claves desconocidas en `editMeta` | M — **hecho** |
| 3 | `credentials.enc` con byte de versión | `KLCS` + `0x01` delante del nonce, también en plantillas y secretos de grafo; sin él es v0 y se reescribe (con copia) en el siguiente sellado; un byte mayor se rechaza sin tocar el fichero | S — **hecho** |
| 4 | agente e invitado: parámetros desconocidos y `/healthz` con versión | ignorar `kling.*` desconocidos en `kling-guest` y `kling-bridge` (y las opciones de volumen que no conocen: solo lectura); `/healthz` JSON si se pide con `Accept`; versión y `caps` del agente en la máquina (`api.Machine.Agent`), no en la receta: construir no arranca el invitado | M — **hecho** |
| 5 | `api` en `/info` y `max_api` en extensiones | `api` en `/info` y `X-Kling-API` en cada respuesta, que `pkg/api` compara (aviso si el daemon es más nuevo, error si es más viejo que el mínimo); `kling plugins` avisa de las que no casan; `kling_vz` a 2 | S — **hecho** |
| 6 | guarda de structs persistidos | `TestCamposPersistidos` compara nombre JSON y tipo de los campos de `api.Machine`, `api.Snapshot`, `credproxy.Credential` y `api.CredentialSpec` (y de los structs del módulo que cuelgan de ellos) con `internal/machine/testdata/esquema/campos-persistidos.txt`, que también apunta la versión de cada fichero. Quitar o cambiar de tipo un campo sin subir la versión falla y `-update` se niega; añadir uno se registra con `-update` | S — **hecho** |
| 7 | fijaciones de `testdata/` | `internal/machine/testdata/esquema/v0.17/`: `state.json`, `meta.json` y `recipe.json` con todos los campos de v0.17.0 (generados con su `pkg/api`); `fijaciones_v017_test.go` comprueba que se leen, que migran con su `.v0.bak` y que ningún campo se pierde ni cambia, tanto en el fichero reescrito como al leerlo al struct. `credentials.v0.enc` ya estaba (PR 3) | S — **hecho** |
| 8 | `kling upgrade` en Linux | pasos 1–9 de §3.4, copia de binarios y unidades, `--dry-run`, `--rollback`, `--from-dir` | L — **hecho** (las unidades no se tocan: §3.4) |
| 9 | `kling upgrade` en Mac y extensiones | `kling-vz`, `launchctl`, companions; `install.sh` remite a `upgrade` sobre una instalación existente | M — **hecho** |
| 10 | e2e `94-e2e-upgrade.sh` | N-1 → HEAD → rollback en el laboratorio y en Mac; paso obligatorio en [`releases.md`](releases.md) antes de etiquetar | M — **hecho** en Linux (en Mac, a mano: paso 3b de `releases.md`) |
| 11 | limpieza de v0.4 | quitar `migrateLinks`, `liftV04` y el alias `warm` → `frozen` | S — **hecho** |

Los PR 2–5 son los de "romper ahora": conviene que salgan **en la misma MINOR**
(v0.18), para que haya una sola actualización incompatible y, desde ella, todo
lo siguiente migre.

---

## 6. Volver atrás desde este cambio

Desde el PR 8, `kling upgrade -rollback` hace lo que sigue (binarios y
`.v<N>.bak`) con la copia de la última actualización. A mano, para lo que se
instaló sin `kling upgrade`:

Con `state.json` ya en v1, un kling ≤ v0.17 lo lee como ilegible, lo aparta a
`state.json.corrupt-*` y arranca en modo protegido (no borra ni mata nada). Para
volver de verdad:

```sh
sudo systemctl stop kling
sudo cp /var/lib/kindling/state.json.v0.bak /var/lib/kindling/state.json
sudo rm /var/lib/kindling/state.json.corrupt-*   # si el viejo llegó a arrancar
# instalar el binario anterior
sudo systemctl start kling
```

Los almacenes de credenciales (PR 3) sí: un kling ≤ v0.17 no descifra uno v1.
Los que se volvieron a sellar tienen su `<fichero>.v0.bak` al lado
(`machines/<id>/credentials.enc.v0.bak`, `secrets/credentials/<plantilla>.enc.v0.bak`,
`store/graph/<id>.secrets.enc.v0.bak`); con el daemon parado, cada copia vuelve
a su nombre. Lo que se selló por primera vez después de migrar no tiene copia:
hay que volver a darlo.

Los `meta.json` de los dorados (PR 2) no hace falta restaurarlos: un kling
≤ v0.17 los lee, e ignora `schema` y los campos nuevos. Si llega a anotar uno,
lo reescribe sin ellos y vuelve a ser v0; la copia `meta.json.v0.bak` que dejó
la primera migración sigue ahí y no se pisa.

Las máquinas creadas **después** de migrar no están en la copia; sus
directorios siguen en `machines/` y el modo protegido los respeta hasta que se
retira la cuarentena.
