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

### 4. `internal/machine/snapshot.go` (Stage 3 — snapshot/thaw)
- `api.Snapshot`: nuevo campo `Layer string` (nombre de la capa) junto a `Image`
  (que pasa a ser la base).
- `Commit`: no copia nada (la capa es ro compartida, como la base); graba la ruta.
- `runFrom`/thaw: replicar el drive de la capa en el jail (toLink ~l.1349) y
  re-pasar `kling.layer=` en la cmdline. Los snapshots viejos (sin `Layer`) siguen
  el camino de 2 discos: back-compat por presencia de campo.

### 5. `internal/daemon/images.go` + `bridge.go` (Stage 4 — accounting/bridge)
- `handleImages`/`Images()`: listar imágenes por capas (existe `.layer.ext4`),
  sumar tamaño base compartida aparte. Proteger la base de borrado si hay capas
  que la referencian (extender el conteo `usedBy`).
- Bonus: hornear el bridge en la BASE → `RefreshBridges` actualiza 1 fichero en
  vez de N. `imageHasBridge` consulta base+capa.

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
3. **Stage 3**: `kling mcp import` (freeze) de un servicio por capas; `thaw`;
   verificar que restaura y responde, y que `thaw_ms` sigue ~25 ms.
4. **Regresión**: una imagen legacy monolítica sigue booteando y thaweando igual.
5. **Ahorro**: `images ls` — base una vez + capas pequeñas; medir disco real total
   vs el modelo monolítico.

## Estado
- [x] Mecanismo OCI (delta-como-lower + whiteout) validado en el kernel de fc-test.
- [x] Decisión de numeración de discos (capa por cmdline `kling.layer`, como `kling.volume`).
- [ ] Stage 1 — build script.
- [x] Stage 2 — overlay-init + manager boot (código; falta la prueba en fc-test).
- [ ] Stage 3 — snapshot/thaw.
- [ ] Stage 4 — accounting + bridge en base.
