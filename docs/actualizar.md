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
| `links.json` → `store/mcp/links` | `internal/daemon/store.go` (`migrateLinks`) | — | migración de v0.4, la única que hay: copia al almacén y deja `links.json.migrated` | un v0.4 ya no ve enlaces |
| grafos | `store/graph/*`, `internal/machine/grafo.go` | ninguna | se ignoran los ilegibles y sus aristas cierran | ídem |
| `net-claims` | `/run/kindling/net-claims` (tmpfs) | ninguna; `"<pid> <ns> <hex8>"` | se vacía al reiniciar el host: sin riesgo | un daemon sin reservas solo se protege comprobando las direcciones del host |

### Snapshots y máquinas congeladas

| Qué | Dónde | Versión | Qué pasa al cambiar |
|---|---|---|---|
| `snapshots/<n>/meta.json` | `api.Snapshot`, `internal/machine/snapshot.go` | **ninguna**, ni la de kling ni la de Firecracker | lectura laxa (`json.Unmarshal`), `liftV04` sube campos de v0.4. **`editMeta` de un binario viejo reescribe el meta sin los campos que no conoce** |
| `snap.file` + `mem.file` (Firecracker) | el formato es de Firecracker | **no se guarda la versión de Firecracker** | un Firecracker que no acepta el formato falla con su error crudo al restaurar. Solo el TSC tiene traducción (`explainRestoreErr`) |
| kernel del dorado | `kernel_sha256` en el meta | hash, no versión | cambiarlo solo da un aviso (`avisoKernel`): el kernel del invitado vive en `mem.file` y sigue restaurando |
| sello de congelación | `machines/<id>/volcado.{en-curso,ok}` + `sello`, `internal/machine/volcado.go` | ninguna | sin marcadores se acepta (máquina anterior al sello) |
| `snap.file` de kling-vz | `vz/internal/spec/spec.go` | **`kling_vz: 1`**; `Decode` rechaza cualquier otro | error claro en ambas direcciones. Pero `graphics` entró sin subir la versión: un kling-vz anterior lo ignora y restaura con otros dispositivos |
| `mem.file` de kling-vz | `SaveMachineStateToPath` de Apple | no se guarda la versión de macOS | un macOS que no lo acepta falla con "restoring the machine state" |
| firma de dorados | `secrets/snapshot.key`, `internal/machine/firma.go` | ninguna | la firma no cubre `kernel_sha256`, IPv6 ni anotaciones, así que añadir campos no la rompe |

### Imágenes y volúmenes

| Qué | Dónde | Versión | Qué pasa al cambiar |
|---|---|---|---|
| `images/<n>.ext4`, `.layer.ext4`, `vmlinux` | `internal/machine/layer.go` | ninguna; la forma se deduce de qué ficheros hay | nada que migrar. Una base sin `kling.layer` se detecta (`baseSupportsLayers`) |
| `images/<n>.recipe.json` | `api.ImageRecipe` | ninguna; `kling_version` es informativo | lectura laxa: ilegible es base `min` y sin techo de CPU. El de Android se escribe sin `durable` |
| `kling-guest` dentro de la imagen | `cmd/kling/builder*.go`, `internal/android` | la versión va en el binario pero **el host nunca la pregunta** | se detecta por sondeo: 404/405 en `/resync`, `/ready`, `/hooks`, o sin cabecera `X-Kling-Share`. Actualizarlo es reconstruir la imagen; solo MCP tiene `refresh-bridge` |
| caché OCI | `internal/oci` | direccionada por contenido | sin riesgo |
| volúmenes y sus snapshots | `volumes/*.ext4`, `internal/machine/volume*.go` | sin metadatos | sin riesgo mientras sean ext4 |

### Secretos

| Qué | Dónde | Versión | Qué pasa al cambiar |
|---|---|---|---|
| `secrets/snapshot.key` | `internal/machine/firma.go` | ninguna, 32 bytes | firma y, por HKDF, cifra. Si se pierde o se regenera, **ningún dorado firmado ni almacén de credenciales vuelve a abrirse** |
| `credentials.enc` (máquinas y plantillas) | `internal/machine/credenciales.go` | la cadena HKDF `"kindling credential store v1"`, **ningún byte de versión en el fichero** | `NormalizarAlmacen` arregla los postgres viejos. Un binario viejo que funde y vuelve a sellar **pierde los campos que no conoce** |
| `credaudit` | `audit/<id>.jsonl` (Linux), `machines/<id>/credaudit.jsonl` (Mac) | ninguna, JSONL | el lector salta las líneas que no entiende; `prepararAuditoria` mueve el sitio viejo al nuevo |

### Extensiones, configuración y servicios

| Qué | Dónde | Versión | Qué pasa al cambiar |
|---|---|---|---|
| `manifests.json` | `pkg/plugin/cache.go` | **`format: 1`**, y cada entrada con la versión de kling y la API | caché: una versión distinta la tira entera y la rehace. Correcto: es regenerable |
| manifiesto de extensión | `pkg/plugin/manifest.go` | **`ManifestVersion` 2** (acepta 1–2), `min_kling` | fuera de rango: la extensión sale con error en la lista, sin tumbar nada. **No hay `max_kling`** |
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
| CLI ↔ daemon | `GET /info` da `Version` y `Capabilities` (solo se añaden, nunca se reusan: `internal/daemon/server.go`). Sin `/v1` ni cabecera | `kling version` y `kling doctor` avisan si difieren; un 404 pelado lleva la pista "el daemon puede ser más viejo" |
| extensión ↔ daemon | `Info.Has(cap)` para cada función nueva, `min_kling` en el manifiesto | extensión vieja con núcleo nuevo: funciona mientras no se quite una ruta. Nueva con núcleo viejo: 404 o `Has` falso |
| daemon ↔ kling-vz | `GET /kling/info` con `credential_kinds` (`http-places`, `graph-link`…) | falta un tipo: "rebuild kling-vz". **La versión de kling-vz no se compara** |
| host ↔ `kling-guest` | ninguno; HTTP en el 8080, sondeo por ruta | host nuevo, invitado viejo: degradación ruta a ruta. Host viejo, invitado nuevo: nada lo nota. Un puente viejo con parámetros de kernel nuevos **muere, y como es PID 1, el invitado entra en pánico** |

### Cómo llega un binario nuevo al disco

| Camino | Verifica | Cambia atómicamente | Para/reinicia el daemon | Vuelta atrás |
|---|---|---|---|---|
| `scripts/install.sh` | sha256 contra `SHA256SUMS`, **antes** de escribir | sí (temporal + `mv -f`) | no | no; no guarda el binario anterior |
| `kling plugin install` | sha256 contra `SHA256SUMS`, manifiesto, `min_kling` | sí, companions primero | no | no |
| `make deploy` | — | `scp` | sí, `restart` | no |
| `kling up` | no baja nada | — | `enable --now` (**no reinicia uno vivo**) | — |

`SHA256SUMS` no está firmado. `kling-guest`, los constructores y las unidades
del host solo se actualizan con `make deploy`.

### Lo que más riesgo tiene

1. **Un binario viejo que reescribe un fichero nuevo pierde campos en
   silencio**: `meta.json` (`editMeta`), `credentials.enc` (la fusión) y
   `state.json` en la misma versión de esquema. Es el fallo que no avisa.
2. **Dorados contra otro Firecracker**: no se sabe con cuál se hicieron, y el
   fallo sale al despertar, lejos de la causa. Es el patrón exacto de §2 de
   [`estabilidad.md`](estabilidad.md).
3. **`snapshot.key`**: todo lo firmado y cifrado depende de 32 bytes sin copia.
   Una reinstalación que la regenere deja inservibles dorados y credenciales.
4. **`kling-guest` horneado sin handshake**: nadie sabe qué agente lleva cada
   imagen, y el caso del puente viejo es un pánico, no un error.
5. **Actualizar no para, no verifica y no vuelve**: `install.sh` cambia el
   binario con el daemon viejo corriendo, y nada comprueba que el nuevo arranca.

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

`credentials.enc` es binario: la versión va en un byte de cabecera delante del
nonce (`0x01`), no dentro del JSON cifrado, para poder rechazar sin descifrar.

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
  durante al menos una MINOR; después, `kling doctor` avisa y `kling images`
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
| extensión fuera de `min_kling`/`max_api` | sale con error en `kling plugins`, no se ejecuta |
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
v0.17.0; usa `kling upgrade`" y solo sigue con `--force`.

### 3.5 Pruebas

- **Fijaciones de estado viejo en `testdata/`.** Por cada formato versionado,
  un fichero real de cada versión anterior (`testdata/estado/state.v0.json`,
  `…/meta.v0.json`), y un test que lo carga con el binario actual y comprueba
  que no pierde nada. Se generan una vez, con el binario de esa versión, y no
  se tocan: son la definición de "lo que había".
- **Un fichero del futuro por formato**: `schema` = actual + 1 con un campo
  inventado, y el test de que se rechaza sin pisarlo (como
  `TestEstadoMasNuevoSeRechazaSinPisarlo`).
- **Guarda de CI:** un test que falla si cambia un struct persistido
  (`api.Machine`, `api.Snapshot`, `credproxy.Credential`) sin tocar su
  constante de versión, comparando los campos con una lista comiteada. Es lo
  que impide el riesgo 1 por descuido.
- **e2e "desde la anterior"** (`scripts/94-e2e-upgrade.sh`): instalar la
  etiqueta N-1 con `install.sh --tag`, crear estado (una máquina parada, una
  congelada, un dorado, un volumen, una credencial, un enlace de MCP), `kling
  upgrade` al binario de HEAD (`--from-dir` para no bajarlo), y verificar que
  todo sigue, que los `.bak` están, y que `--rollback` vuelve a N-1 con el
  estado intacto. Necesita KVM, así que no corre en `ci.yml`: en el laboratorio
  y en Mac (`92-e2e-mac.sh` ya sabe levantar su daemon), y **obligatorio antes
  de cada `release.sh`**. Sin VM, en CI sí puede correr la parte de ficheros:
  arrancar el `Manager` de N-1 y el de HEAD sobre el mismo directorio.

---

## 4. Qué romper ahora

Antes de que haya usuarios cada uno cuesta un commit; después, una migración y
su prueba para siempre.

| Prioridad | Qué | Por qué ahora |
|---|---|---|
| **P0** | `schema` en `state.json` | **hecho** en este cambio: sin él no hay forma de rechazar un fichero del futuro |
| **P0** | `schema` en `meta.json` y versión de Firecracker/kling-vz/macOS y de kling en el dorado | es lo que permite marcar obsoletos en vez de fallar al despertar; los dorados de hoy se pueden tirar |
| **P0** | byte de versión en `credentials.enc` | es binario: añadirlo después obliga a adivinar si un fichero lo tiene |
| **P0** | el agente y el puente ignoran `kling.*` desconocidos | cada imagen que se construye con el agente de hoy lleva el pánico dentro para siempre |
| **P1** | `/healthz` del agente con `version` y `caps` | mismo motivo: lo que se hornea hoy es lo que habrá que soportar |
| **P1** | `api` en `/info` y `max_api` en el manifiesto | el manifiesto ya tiene versión; añadir el campo ahora es gratis |
| **P1** | subir `kling_vz` a 2 por `graphics` | hoy un kling-vz anterior restaura en silencio con otros dispositivos |
| **P1** | `schema` en el almacén (`store`) para `mcp/links`, `phone/*` y grafos | los dueños son nuestras extensiones; poner la convención antes de que haya extensiones de otros |
| **P2** | quitar `migrateLinks` y `liftV04` | migraciones de v0.4 sin nadie en v0.4; son código que hay que mantener probado |
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
| 2 | `meta.json` v1 y dorados obsoletos | `schema`, `vmm` (`firecracker 1.17.0` / `kling-vz 0.18.0 macOS 26.1`) y `kling_version` en el meta; `stale` con causa en `kling snapshots` y `doctor`; conservar claves desconocidas en `editMeta` | M |
| 3 | `credentials.enc` con byte de versión | `0x01` delante del nonce; sin él es v0 y se reescribe al abrir; un byte mayor se rechaza | S |
| 4 | agente e invitado: parámetros desconocidos y `/healthz` con versión | ignorar `kling.*` desconocidos en `kling-guest` y `kling-bridge`; `/healthz` JSON si se pide con `Accept`; versión del agente en la receta | M |
| 5 | `api` en `/info` y `max_api` en extensiones | cliente con aviso claro; `kling plugins` marca las que no casan; `kling_vz` a 2 | S |
| 6 | guarda de structs persistidos | test que compara campos de `api.Machine`, `api.Snapshot`, `credproxy.Credential` con una lista comiteada y exige subir la versión | S |
| 7 | fijaciones de `testdata/` | los ficheros de v0.17 de cada formato y sus tests de carga | S |
| 8 | `kling upgrade` en Linux | pasos 1–9 de §3.4, copia de binarios y unidades, `--dry-run`, `--rollback`, `--from-dir` | L |
| 9 | `kling upgrade` en Mac y extensiones | `kling-vz`, `launchctl`, companions; `install.sh` remite a `upgrade` sobre una instalación existente | M |
| 10 | e2e `94-e2e-upgrade.sh` | N-1 → HEAD → rollback en el laboratorio y en Mac; paso obligatorio en [`releases.md`](releases.md) antes de etiquetar | M |
| 11 | limpieza de v0.4 | quitar `migrateLinks`, `liftV04` y el alias `warm` → `frozen` | S |

Los PR 2–5 son los de "romper ahora": conviene que salgan **en la misma MINOR**
(v0.18), para que haya una sola actualización incompatible y, desde ella, todo
lo siguiente migre.

---

## 6. Volver atrás desde este cambio

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

Las máquinas creadas **después** de migrar no están en la copia; sus
directorios siguen en `machines/` y el modo protegido los respeta hasta que se
retira la cuarentena.
