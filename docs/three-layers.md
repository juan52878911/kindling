# Imágenes por capas (base compartida + capa de servicio + overlay)

Objetivo: dejar de duplicar la base (~110–130 MiB reales, ~810 MiB lógicos) en
cada imagen de servicio. Hoy `scripts/80-mcp-image.sh` hace `cp base → $NAME.ext4`
y el resultado es un ext4 monolítico donde casi todo son bytes idénticos entre
servicios. Con capas: **base ro compartida una vez + una capa de servicio ro
pequeña (solo el delta) + el overlay rw por máquina que ya existe.**

Ahorro (bytes reales, medidos con `images ls`): 500 servicios **~65 GB → ~10–15 GB**.

## Mecanismo validado (fc-test, 2026-08-15)

Probado en el kernel real de fc-test que el modelo OCI funciona:
1. **Construir** el delta como *upper* de un overlay sobre la base
   (`lowerdir=base,upperdir=layer,workdir=work`): el `layer` queda con ficheros
   nuevos, overrides, y **whiteouts** (char device 0/0) para los borrados.
2. **Usar** ese `layer` como *lower* junto a la base
   (`lowerdir=layer:base,upperdir=rw,workdir=wk`): fichero nuevo visible, override
   gana, whiteout oculta el de la base. ✓
El `vmlinux` del invitado usa el mismo módulo overlayfs (ya hace single-lower en
`overlay-init.sh`), así que multi-lower + whiteout aplica igual dentro.

## Decisión de numeración de discos

El invitado nombra los discos por orden de enganche: hoy vda=base(ro),
vdb=overlay(rw), vdc+=volúmenes. Insertar una capa NO debe romper el layout
legacy. Decisión: **la capa de servicio se engancha como un disco ro extra y su
device viaja por la cmdline del kernel** (`kling.layer=/dev/vdX`), igual que el
volumen viaja por `kling.volume`. `overlay-init` decide el modo por la presencia
de ese parámetro:

- **Legacy (sin capa)**: vda=base, vdb=overlay, vdc+=vols. `overlay-init` hace
  `lowerdir=/` como hoy. Sin cambios.
- **Con capa**: se añade la capa como el disco inmediatamente posterior a los
  volúmenes (para no correr vdb/vdc y no tocar `volumeDriveID`), y su device real
  se pasa en `kling.layer=`. `overlay-init` monta la capa ro y hace
  `lowerdir=<capa>:/`.

Racional: pasar el device por cmdline evita depender del orden absoluto (que
cambia con el nº de volúmenes) — es el mismo patrón ya probado de `kling.volume`.
El manager conoce el orden de enganche, así que calcula el device de la capa y lo
pone en `kling.layer`.

## Cambios por fichero

### 1. `scripts/80-mcp-image.sh` (Stage 1 — construir la capa)
- Sustituir `cp base→DEST` (l.156) por: crear `$NAME.layer.ext4` vacío
  (`createOverlay`-style: truncate + mkfs.ext4 -O ^has_journal -E nodiscard),
  montarlo, montar la base ro, y `mount -t overlay -o
  lowerdir=$basemnt,upperdir=$layermnt,workdir=$layermnt/.work $merged`.
- `$mnt` pasa a ser `$merged`: TODO lo que el script ya hace (apk/npm/bundle,
  entrypoint, capabilities.json, browser.json, EXTRA_DIR) cae en el upper = la
  capa. Cero cambios en esa parte.
- Al final: `umount $merged`, borrar `$layermnt/.work`, `umount` base y capa,
  `resize2fs -M $NAME.layer.ext4` (encoger al mínimo), `--sparse`.
- La receta ya persiste `Base` (`daemon/images.go`); el runtime la leerá.
- Flag de salida: escribir `$NAME.layer.ext4` (no `$NAME.ext4`). Un `$NAME.ext4`
  ausente + `$NAME.layer.ext4` presente = imagen por capas.

### 2. `scripts/overlay-init.sh` + `scripts/minimal-init.sh` (Stage 2 — guest) ✅
- Leer `kling.layer=/dev/vdX` de `/proc/cmdline`. Si está: montar la capa ro y
  `lowerdir=<capa>/upper:/`. Si no está: camino legacy (`lowerdir=/`), intacto.
- **Los dos scripts.** `70-build-minimal-image.sh` instala `minimal-init.sh` como
  `/sbin/overlay-init` en la base `min` — que es la de los servicios MCP—, así que
  tocar solo `overlay-init.sh` (la base con systemd) no habría cambiado nada donde
  importa. `TestInitScriptsReadLayerParam` ata los dos al nombre del parámetro.
- **Punto de montaje `/overlay/svc`, no `/svc`**: la raíz va en solo lectura, así
  que dentro del invitado no se puede crear un punto de montaje nuevo. `/overlay`
  ya es el disco escribible de la máquina.
- **Prefijo `/upper`**: el delta cuelga de ahí dentro de la capa porque el build lo
  construye como upperdir (overlayfs exige upper y work hermanos, sin anidar).
- El parámetro se busca partiendo `/proc/cmdline` por palabras, sin `sed`: el sed
  de busybox va contra el regex de musl y no trae `\b` ni las demás extensiones
  de GNU. Un patrón que no case dejaría la capa sin montar.

### 3. `internal/machine/manager.go` + `layer.go` (Stage 2 — boot) ✅
- Nuevo `internal/machine/layer.go`: `imageLayer(image) (base, layer, err)`
  —`layer == ""` es monolítica—, `layerDevice(nvols)`, `layerBootArg`,
  `layerGuestPath`, `baseSupportsLayers`. `imagePath` NO cambia de significado:
  sigue siendo la ruta de la monolítica, y la resolución va aparte para no
  alterarla bajo `snapshot.go` (Stage 3).
- Precedencia: si están `$image.ext4` y `$image.layer.ext4`, manda la monolítica.
  La base sale de la receta (`Base`), y sin receta se asume `min`, que es lo que
  asume el propio build. Una capa sin base da un error que la nombra.
- `boot()`: vda=base, y `SetDrive{DriveID:"svclayer", IsReadOnly:true}` como disco
  extra TRAS los volúmenes; su `/dev/vdX` va en `bootArgs` como `kling.layer=`.
- `bootArgs(vols, allowExec, layerDev)`: sin capa la línea no cambia ni un byte.
- Jailer `toLink`: la capa entra en los hardlinks del jail.
- **Base antigua = error explicado, no pánico.** El `overlay-init` viaja DENTRO de
  la base, así que una base anterior a las capas ignoraría `kling.layer` y
  arrancaría el invitado sin el servicio: se vería como un pánico del kernel.
  `baseSupportsLayers` lo comprueba con debugfs (cacheado por tamaño+mtime) y
  Create falla diciendo qué reconstruir. Sin debugfs se sigue, con aviso.
- `imageHasBridge` y `ImageCapabilities` leen de la capa (`/upper/...`) cuando la
  hay, con la base de respaldo: si no, un servicio por capas con volumen se
  rechazaría por "no lleva puente" llevándolo.

### 4. `internal/machine/snapshot.go` (Stage 3 — snapshot/thaw) ✅
- `Commit`: **cero cambios**. La capa es ro compartida como la base, así que no se
  copia; y el conjunto de discos ya queda grabado por Firecracker.
- `runFrom`/thaw: replicar el drive de la capa en el jail. Y ya está.
- **Sin campo `Layer` en el meta, y sin re-pasar `kling.layer=`.** El plan preveía
  las dos cosas; ninguna hace falta, y añadirlas habría sido estado de más:
  - La línea de comandos del kernel se congeló DENTRO de la memoria. Una VM
    restaurada no admite una cmdline nueva —ni discos nuevos—: despierta con la
    capa ya montada en su tabla de montajes. Lo único que hay que garantizar es
    que el fichero siga en la ruta que el snapshot grabó, y eso es el enlace.
  - La capa se resuelve del nombre de la imagen (`imageLayer(snap.Image)`), igual
    que la base. Guardarlo aparte habría cambiado el significado de `Image` —que
    hoy es el nombre del SERVICIO, y de ahí sale el conteo `usedBy`— sin comprar
    nada. Los snapshots viejos siguen valiendo sin tocar su meta.

### 5. `internal/daemon/images.go` + `bridge.go` (Stage 4 — accounting/bridge) ✅
- `Images()`: recorta `.layer.ext4` ANTES que `.ext4` — el sufijo largo también
  casa con el corto, y sin ese orden aparecía una imagen fantasma `files.layer`.
- `handleImages`: `api.Image` gana `Base` (sobre qué se apoya) y `Layers` (cuántas
  se apoyan en ella). Los tamaños de una imagen por capas miden LA CAPA: la base
  no es suya, y sumársela a cada una haría creer que el disco está N veces más
  lleno de lo que está. `kling images ls` añade columna BASE y un total al pie —la
  cifra que justifica todo esto, con cada base contada UNA vez—.
- Protección de la base: no hay endpoint de borrado (las imágenes se quitan a mano
  del host), así que la protección es lo que `images ls` enseña antes del `rm`:
  una base con capas encima sale con `N layer(s)` en USED BY aunque no tenga
  snapshots propios.
- `imageUsers()`: una máquina por capas retiene su capa **y la base**. Sin eso,
  `images refresh` reescribiría la base bajo un servicio vivo — la corrupción que
  esa cuenta existe para impedir.
- `RefreshBridges`: en una imagen por capas toca la CAPA (el puente está en
  `/upper/...`), nunca la base, que es de otros.
- **Bonus hecho — el puente, horneado en la base.** `70-build-minimal-image.sh` lo
  instala en la base, y `80-mcp-image.sh` (`install_bridge`) NO lo copia a la capa
  si el binario que ya se ve es idéntico. Así actualizarlo son ~8 MiB menos por
  servicio y **un** fichero (`kling images refresh min`) en vez de N. Una capa sin
  puente propio no se reporta como "no es una imagen de servicio" sino como *"its
  bridge comes from base X"*. Si la base no lo trae, la capa se lleva el suyo y
  todo sigue igual: lo que está en la capa gana por ir de lower delante.

## Back-compat y migración
- Imágenes monolíticas existentes: `$NAME.ext4` presente → camino legacy de 2
  discos, sin cambios. Nada que migrar para que sigan funcionando.
- Migrar un servicio a capas = reimportarlo (flujo ya conocido, como el refresh de
  bridge). No hay conversión in-place.
- **La base hay que reconstruirla una vez** (`scripts/70-build-minimal-image.sh`):
  el init que entiende `kling.layer` vive dentro de ella. Las imágenes monolíticas
  siguen arrancando con la base nueva —el parámetro simplemente no está—, así que
  se puede hacer antes de empaquetar nada por capas.

## Plan de pruebas (fc-test, por etapas)
1. **Stage 1**: construir una imagen por capas; `ls -la` la capa (debe ser MB, no
   ~130 MiB); montarla y verificar que solo tiene el delta.
2. **Stage 2**: `kling run` de una imagen por capas; debe bootear y el MCP
   responder `tools/list`. Comparar `boot_ms` con legacy.
   - **Lo primero que hay que confirmar aquí** es que la letra de disco de la capa
     sale del ORDEN DE ENGANCHE. El manager la calcula así (`layerDevice`), igual
     que el puente cuenta los volúmenes desde vdc, pero esto es lo único del
     diseño que no se puede comprobar sin Firecracker delante. Si fallara, el
     síntoma es un `mount ... failed: No such file or directory` en la consola
     nombrando el device — y la prueba que lo aisla es arrancar CON un volumen,
     donde base+overlay+volumen empujan la capa a vdd.
3. **Stage 3**: `kling mcp import` (freeze) de un servicio por capas; `thaw`;
   verificar que restaura y responde, y que `thaw_ms` sigue ~25 ms.
4. **Regresión**: una imagen legacy monolítica sigue booteando y thaweando igual.
5. **Ahorro**: `images ls` — base una vez + capas pequeñas; medir disco real total
   vs el modelo monolítico.

## Resultados medidos (fc-test, 2026-08-15)

Las cinco pruebas pasan sobre Firecracker de verdad, con el parque real de 9
servicios monolíticos delante y sin tocarlos.

| | monolítico | por capas |
|---|---|---|
| `sequentialthinking` / `nseq` | 128 MiB | **31 MiB** |
| `memory` / `nmem` | 139 MiB | **20 MiB** |
| base `node` compartida | — | 113 MiB (una vez) |
| **total de los dos** | **267 MiB** | **164 MiB** |

Arranque en frío: 117/38/86 ms por capas contra 63/142/83 ms monolítico — el ruido
tapa la diferencia, la capa no cuesta arranque medible. Thaw: 18/20/27/36/41/42 ms,
dentro de lo de siempre. `tools/list` responde por el gateway igual en las dos
formas, y los servicios legacy (`memory`, `fetch`, `everything`, `sequentialthinking`)
siguen despertando y respondiendo tras el despliegue.

**EL AHORRO SALE DE LO QUE HAY EN LA BASE, no de las capas.** Es el hallazgo que
corrige la premisa de este documento: la base `min` real pesa 23 MiB, no los
110-130 MiB que se supusieron aquí. Esos 130 MiB de un servicio monolítico son en
su mayoría **nodejs+npm**, no la base. Construir por capas sobre `min` deja una
capa de 126 MiB y ahorra ~2 MiB: nada.

Con el runtime dentro de la base (`PKGS="nodejs npm" ./70-build-minimal-image.sh
node`) la capa cae a 20-31 MiB y el ahorro aparece. Extrapolado a 500 servicios
node: 500 × 130 MiB ≈ **65 GB** monolítico contra 113 MiB + 500 × 25 MiB ≈
**13 GB** — que es la cifra que prometía el plan, pero solo por esta vía.

Consecuencia práctica: **hace falta una base por familia de runtime** (una `node`,
una `python`…). `kling add -base node` ya existe para elegirla; sin ese flag, todo
lo que se empaquetara por el camino cómodo caía sobre `min` y no ahorraba nada.
`nodejs`/`npm` se piden igual aunque la base ya los traiga: apk sobre lo instalado
no engorda la capa (medido: 28 MiB contra 31) y así el flag vale con cualquier
base, en vez de fallar con un `npm: not found` a mitad de construcción.

## Familias de runtime y servidores de Python

Las bases de familia se construyen POR NOMBRE, sin recordar listas de paquetes:

    sudo BRIDGE=<ruta> ./scripts/70-build-minimal-image.sh node     # nodejs npm
    sudo BRIDGE=<ruta> ./scripts/70-build-minimal-image.sh python   # python3 py3-pip

El nombre no es cosmético: `kling add` sin `-base` busca en el daemon una base
que se llame **como la familia del runtime del paquete** (`node`, `python`) y la
usa sola; si no está, avisa de que la capa cargará con el runtime entero y dice
qué construir. `-base` explícito sigue mandando (`-base min` fuerza la mínima).
`TestFamiliasDeRuntimeCasanConElScriptDeBase` ata los nombres a los presets.

Con eso, los servidores MCP de **PyPI** entran por el mismo camino que los de
npm: `registry.Stdio()` acepta ambos tipos (npm gana si el servidor publica en
los dos), el paquete se preinstala con `pip` (`-P`, con `--only-binary=:all:`
por la misma razón que `--ignore-scripts` en npm) y la base elegida es `python`.
Dos diferencias con npm que hay que conocer:

- **El ejecutable se INFIERE, no se resuelve.** PyPI no publica los entry
  points en su API JSON (el equivalente al campo `bin` vive dentro de la
  rueda), así que se aplica la convención de uvx: ejecutable = nombre
  normalizado (PEP 503) de la distribución. Los paquetes que no la siguen los
  caza el propio build: `80-mcp-image.sh` comprueba con `chroot ... command -v`
  que el comando exista dentro de la imagen y falla ANTES de importar, no en el
  import como "el servidor no abrió el puerto".
- **`kling add -env KEY=value`** hornea variables de entorno en el entrypoint
  (en texto plano: para interruptores, no para secretos). Existía `-e` en el
  script y ningún camino del CLI llegaba a él.

### El caso que lo justifica: semgrep

El `semgrep` que corre en producción NO trae semgrep dentro de la imagen: hace
`pip install` al arrancar, cada arranque en frío paga la descarga y varios a la
vez tiran la máquina por OOM. Reimportado por capas queda horneado y sin
phone-home (que con el egress cerrado costaba ~2 min de timeout por arranque):

    kling add semgrep \
      -env SEMGREP_SEND_METRICS=off -env SEMGREP_ENABLE_VERSION_CHECK=0

(la base `python` la encuentra sola si existe; las ruedas de semgrep pesan
~400 MB, el GROW por defecto con `-P` ya lo contempla).

### El encogido de la capa nunca se ejecutaba

Salió al empaquetar una calculadora con `kling add`: **451 MiB de capa para
73 MiB de contenido**. El sistema de ficheros seguía a 805 MiB con 716 libres
dentro — `resize2fs -M` no había corrido nunca.

La causa es un orden: resize2fs se NIEGA a tocar un fs que no se acabe de
comprobar (*"Please run 'e2fsck -f' first"*), y el script lo llamaba ANTES del
`e2fsck`, con la salida a `/dev/null` y un `|| true` detrás. Fallaba siempre y en
silencio. Además, encoger el fs no basta: el FICHERO conserva asignado todo lo que
se escribió alguna vez —los node_modules temporales de npm, la caché de apk— y eso
solo se devuelve recortándolo a la frontera del fs, tamaño que ahora se lee del
superbloque en vez de estimarse.

Arreglado: e2fsck → resize2fs -M → truncate a `Block count × Block size` → e2fsck.
La misma imagen pasa de **451 MiB a 75 MiB**, y sigue arrancando y respondiendo
`tools/call` por el gateway (`6*7` → `42`).

Verificado además, punto por punto:
- **El orden de discos era el correcto**, incluido el caso que lo podía romper: con
  un volumen enganchado, la consola del invitado enseña `vdc` = volumen de 64 MiB y
  `vdd` = capa de 256 MiB, y el volumen montado en `/data`. Era lo único del diseño
  que no se podía comprobar sin Firecracker delante.
- **El puente horneado en la base funciona**: el build dijo *"ya viene en la base
  'min', no se duplica en la capa"*, la capa NO lo lleva dentro (`debugfs`), y aun
  así la microVM arranca — porque el PID 1 sale de la base.
- `images ls` enseña `BASE`, `N layer(s)` y el total con cada base contada una vez.
- `images refresh` sobre una capa avisa *"its bridge comes from base X"* y no la
  toca; sobre la base, la actualiza para todas.

Un fallo encontrado por el camino que **no es de las capas**: `sweepMachineDirs`
(reconcile) borra el directorio de una máquina que aún se está creando, porque
`runFrom` lo crea cientos de ms antes de registrarla en `byID`. Se ve como
`open .../firecracker.log: no such file or directory` y solo aparece cuando el GC
corre a menudo (disco >90%). Queda anotado aparte.

## El parque real, reconstruido (fc-test, 2026-08-15)

Los 7 servicios node de producción, reimportados por capas sobre la base `node`:

| servicio | antes | después |
|---|---|---|
| context7 | 129 MiB | 39 MiB |
| everything | 134 MiB | 39 MiB |
| fetch | 164 MiB | 77 MiB |
| filesystem-mcp | 135 MiB | 36 MiB |
| memory | 139 MiB | 37 MiB |
| sequentialthinking | 128 MiB | 38 MiB |
| wikipedia-mcp-server | 471 MiB | 54 MiB |
| **suma** | **1300 MiB** | **320 MiB** + 113 de base compartida |

**1300 MiB → 433 MiB, un 67% menos**, y el disco de la VM baja del 89% al 82%.
Los 7 responden `tools/list` por el gateway con el mismo catálogo que antes.

Efecto secundario que vale tanto como el disco: **ahora los 7 tienen receta**.
Ninguno la tenía —se construyeron antes de que existieran—, así que reconstruirlos
exigió sacar el comando de cada `/entrypoint` con `debugfs` y deducir el paquete
npm del árbol de `node_modules`. Eso ya no hay que volver a hacerlo.

El procedimiento fue reversible por servicio: la imagen monolítica no se borra, se
APARTA (la monolítica tiene precedencia sobre la capa, así que apartarla es lo que
activa la nueva), y solo se retira cuando el catálogo del servicio reconstruido
coincide con el de antes.

Los dos que siguen monolíticos:
- **semgrep** — es Python. Espera a la base `python`.
- **playwright** — 2.5 GiB con Chromium dentro; además hoy no arranca en fc-test
  por RAM: pide 1536 MiB y quedan ~1222 libres, porque docker/containerd retienen
  2,4 GiB de la VM. No es de las capas.

## Estado
- [x] Mecanismo OCI (delta-como-lower + whiteout) validado en el kernel de fc-test.
- [x] Decisión de numeración de discos (capa por cmdline `kling.layer`, como `kling.volume`).
- [x] Stage 1 — build script.
- [x] Stage 2 — overlay-init + manager boot.
- [x] Stage 3 — snapshot/thaw.
- [x] Stage 4 — accounting + bridge en base.
- [x] **Validación en fc-test** — las cinco pruebas pasan; ver arriba.
- [x] Flag `-base` en `kling add`, probado de punta a punta: construye por capas,
  importa, congela y responde `tools/call` por el gateway.
- [x] El encogido de la capa, que no se ejecutaba nunca (451 MiB → 75 MiB).
- [x] Reimportar el parque: los 7 servicios node, hechos y verificados (ver arriba).
- [x] Python de primera: familias de runtime con nombre en 70-build, `kling add`
  para PyPI (base automática, pip, ejecutable por convención + comprobación en
  el build, `-env`). Ver "Familias de runtime y servidores de Python".
- [ ] Construir la base `python` en fc-test (`sudo ./70-build-minimal-image.sh
  python`) y validar el camino PyPI de punta a punta con semgrep: build por
  capas, import, thaw y `tools/call`. La `node` ya está.
- [ ] `playwright`: 2.5 GiB con Chromium. Merece su propia base (`node` + las
  dependencias del navegador) y decidir si el navegador va en la base o en la capa.
  Hoy además no arranca en fc-test por RAM.
