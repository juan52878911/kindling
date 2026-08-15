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

### 2. `scripts/overlay-init.sh` (Stage 2 — guest)
- Leer `kling.layer=/dev/vdX` de `/proc/cmdline`. Si está:
  `mount -t ext4 -o ro $LAYER /svc` y
  `mount -t overlay -o lowerdir=/svc:/,upperdir=/overlay/upper,workdir=/overlay/work /overlay/merged`.
- Si no está: camino legacy actual (`lowerdir=/`). Intacto.

### 3. `internal/machine/manager.go` (Stage 2 — boot)
- `imagePath`: si existe `$image.layer.ext4` y no `$image.ext4`, es por capas;
  resolver la base desde la receta (nuevo helper `imageLayer(image) (base, layer,
  ok)`).
- `boot()`: cuando la imagen es por capas, vda=base (de la receta), y añadir un
  `SetDrive{DriveID:"svclayer", ...capa..., IsReadOnly:true}` como disco extra
  tras los volúmenes; calcular su `/dev/vdX` y pasarlo en `bootArgs` como
  `kling.layer=`.
- `bootArgs`: nuevo argumento opcional `layerDev string` → `+ " kling.layer=" +`.
- Jailer `toLink` (cold ~l.797): añadir el fichero de la capa a los bind-mounts.

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
- [ ] Stage 2 — overlay-init + manager boot.
- [ ] Stage 3 — snapshot/thaw.
- [ ] Stage 4 — accounting + bridge en base.
