#!/usr/bin/env bash
# verity.sh — pone la capa de Android detrás de dm-verity (docs/verity.md).
#
#   sudo prototypes/android/image/verity.sh IMAGES_DIR LAYER BASE [NEW_LAYER NEW_BASE]
#   sudo prototypes/android/image/verity.sh /var/tmp/kindling-android/images android13 android-base
#   sudo prototypes/android/image/verity.sh DIR android13 android-base android13v android-basev
#
# Corre en un Linux como root, con veritysetup (cryptsetup-bin), e2fsprogs y
# red (instala dmsetup en la base con apt). build-image.sh lo llama con
# VERITY=1; también sirve sobre un paquete ya construido.
#
# Qué hace:
#   1. Deja el fichero de la capa en el tamaño de su ext4 y le pega detrás, en
#      el MISMO fichero, el árbol de hashes (sha256, bloques de 4 KiB, sin
#      superbloque) y el FEC (Reed-Solomon, 2 raíces: ~0,8 % más). kindling
#      engancha la capa como un disco (vdc); el ext4 ignora lo que hay tras su
#      último bloque, así que la misma capa arranca igual con una base sin
#      verity.
#   2. Saca la tabla de device-mapper con un `veritysetup open` de verdad (en
#      un loop) y la guarda en la base, en /etc/kindling-android/layer.verity,
#      con el dispositivo como @DEV@. La raíz del hash va en esa tabla y en la
#      receta (spec.verity_root_hash).
#   3. Instala dmsetup en la base y cambia su /sbin/overlay-init (el
#      minimal-init de kindling) para que, si existe esa tabla, cree
#      /dev/mapper/android-layer y monte la capa desde ahí. Si falla, el init
#      se para: montar la capa sin verificar sería volver al fallo de siempre.
#
# Con NEW_LAYER/NEW_BASE escribe copias (la capa como enlace duro: el árbol va
# pegado, así que la capa original queda también con él) y una receta
# NEW_LAYER.recipe.json que apunta a NEW_BASE. Así el mismo daemon puede tener
# la imagen con verity y la de siempre sobre la misma capa.
#
# Modo de error: el de por defecto de dm-verity, EIO. Nunca ignore_corruption
# ni restart/panic_on_corruption. El kernel del prototipo (6.1.140) ya relee
# un bloque de datos que no cuadra (verity_recheck) y, si sigue mal, prueba
# con FEC; el parche kernel/patches/dm-verity-reread.patch hace lo mismo con
# los bloques de hashes y deja en dmesg cada relectura que arregla algo.
set -euo pipefail

[ "$(id -u)" -eq 0 ] || { echo "run it as root" >&2; exit 1; }
[ $# -eq 3 ] || [ $# -eq 5 ] || { sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
DIR="$1"; LAYER="$2"; BASE="$3"; NLAYER="${4:-$2}"; NBASE="${5:-$3}"
FEC_ROOTS="${FEC_ROOTS:-2}"          # 0 = sin FEC
PATH="$PATH:/usr/sbin:/sbin"
for c in veritysetup dmsetup losetup dumpe2fs resize2fs e2fsck python3 chroot mount; do
  command -v "$c" >/dev/null 2>&1 || { echo "missing $c (apt-get install cryptsetup-bin dmsetup e2fsprogs)" >&2; exit 1; }
done
L="$DIR/$LAYER.layer.ext4"; B="$DIR/$BASE.ext4"
[ -f "$L" ] && [ -f "$B" ] || { echo "need $L and $B" >&2; exit 1; }
log() { printf '==> %s\n' "$*"; }

# ── 1. árbol de hashes y FEC detrás del ext4 ────────────────────────────────
bs="$(dumpe2fs -h "$L" 2>/dev/null | sed -n 's/^Block size: *//p')"
nb="$(dumpe2fs -h "$L" 2>/dev/null | sed -n 's/^Block count: *//p')"
[ "$bs" = 4096 ] || { echo "$L: ext4 block size $bs, expected 4096" >&2; exit 1; }
DATA_BYTES=$((nb * 4096))
# Si ya tenía un árbol pegado (otra pasada), se tira y se rehace.
truncate -s "$DATA_BYTES" "$L"
# Tamaño del árbol: 128 hashes de 32 bytes por bloque, nivel a nivel hasta 1.
HASH_BLOCKS="$(python3 -c '
import sys; n = int(sys.argv[1]); t = 0
while n > 1:
    n = (n + 127) // 128; t += n
print(t)' "$nb")"
HASH_OFF=$DATA_BYTES
FEC_OFF=$(( (nb + HASH_BLOCKS) * 4096 ))
fec=()
[ "$FEC_ROOTS" = 0 ] || fec=(--fec-device "$L" --fec-offset "$FEC_OFF" --fec-roots "$FEC_ROOTS")
log "veritysetup format: $nb bloques de datos, $HASH_BLOCKS de hashes, FEC con $FEC_ROOTS raíces"
t0=$(date +%s)
out="$(veritysetup format --no-superblock --hash sha256 --data-block-size 4096 --hash-block-size 4096 \
        --data-blocks "$nb" --hash-offset "$HASH_OFF" "${fec[@]}" "$L" "$L")"
# shellcheck disable=SC2001
echo "$out" | sed 's/^/    /'
ROOT="$(echo "$out" | sed -n 's/^Root hash:[[:space:]]*//p')"
SALT="$(echo "$out" | sed -n 's/^Salt:[[:space:]]*//p')"
[[ "$ROOT" =~ ^[0-9a-f]{64}$ ]] || { echo "no root hash in veritysetup output" >&2; exit 1; }
log "formato en $(( $(date +%s) - t0 )) s; capa: $((DATA_BYTES >> 20)) MiB → $(( $(stat -c %s "$L") >> 20 )) MiB"

# ── 2. la tabla, de un open real ─────────────────────────────────────────────
# Sin device-mapper (un CT de LXC: /dev/mapper/control está pero el host no
# deja usarlo, o no tiene dm_verity cargado; medido en el CT 106 de Proxmox)
# no hay open real: la tabla se escribe a mano —es la de la versión 1 de
# dm-verity, la misma que saca dmsetup de un open sin superbloque— y el árbol y
# el FEC se comprueban en espacio de usuario con `veritysetup verify`. Lo que
# no se comprueba aquí es que el kernel acepte la tabla: eso lo dice el
# arranque (el init de la base se niega a montar la capa sin verity).
if ! dmsetup version >/dev/null 2>&1; then
  log "sin device-mapper aquí: tabla escrita a mano y comprobación con veritysetup verify"
  t0=$(date +%s)
  veritysetup verify --no-superblock --hash sha256 --data-block-size 4096 --hash-block-size 4096 \
    --data-blocks "$nb" --hash-offset "$HASH_OFF" --salt "$SALT" "${fec[@]}" "$L" "$L" "$ROOT" ||
    { echo "veritysetup verify of the layer failed" >&2; exit 1; }
  log "veritysetup verify en $(( $(date +%s) - t0 )) s"
  TABLE="0 $((nb * 8)) verity 1 @DEV@ @DEV@ 4096 4096 $nb $nb sha256 $ROOT $SALT"
  MANUAL_TABLE=1
else
  MANUAL_TABLE=0
fi
if [ "$MANUAL_TABLE" = 0 ]; then
lo="$(losetup -f --show -r "$L")"
dm="kvtmp$$"
cerrar() { dmsetup remove "$dm" 2>/dev/null || true; losetup -d "$lo" 2>/dev/null || true; }
trap cerrar EXIT
# Se abre SIN FEC: el kernel de la VM de construcción puede no tenerlo (el
# de Ubuntu 24.04 no trae DM_VERITY_FEC: "Invalid number of feature args").
# Los argumentos de FEC se añaden a mano abajo; el FEC cubre datos + hashes
# (no hay relleno entre medias), que es lo que codificó veritysetup.
veritysetup open --no-superblock --hash sha256 --data-block-size 4096 --hash-block-size 4096 \
  --data-blocks "$nb" --hash-offset "$HASH_OFF" --salt "$SALT" "$lo" "$dm" "$lo" "$ROOT"
majmin="$(lsblk -dno MAJ:MIN "$lo" | tr -d ' ')"
TABLE="$(dmsetup table --showkeys "$dm" | sed "s#$majmin#@DEV@#g; s#$lo#@DEV@#g")"
fi
if [ "$FEC_ROOTS" != 0 ]; then
  [ "$(echo "$TABLE" | wc -w)" = 13 ] || { echo "unexpected optional args in: $TABLE" >&2; exit 1; }
  TABLE="$TABLE 8 use_fec_from_device @DEV@ fec_roots $FEC_ROOTS fec_blocks $((nb + HASH_BLOCKS)) fec_start $((FEC_OFF / 4096))"
fi
if [ "$MANUAL_TABLE" = 0 ]; then
  # Comprobación: la capa se lee entera a través de verity sin un solo error.
  dd if="/dev/mapper/$dm" of=/dev/null bs=4M status=none || { echo "reading the layer through dm-verity failed" >&2; exit 1; }
  e2fsck -fn "/dev/mapper/$dm" >/dev/null || { echo "e2fsck through dm-verity failed" >&2; exit 1; }
  cerrar; trap - EXIT
else
  e2fsck -fn "$L" >/dev/null || { echo "e2fsck of the layer failed" >&2; exit 1; }
fi
case "$TABLE" in *verity*@DEV@*@DEV@*"$ROOT"*) ;; *) echo "unexpected table: $TABLE" >&2; exit 1 ;; esac
log "tabla: $TABLE"

# ── 3. la base: dmsetup + el init que monta la capa por verity ──────────────
if [ "$NBASE" != "$BASE" ]; then cp --sparse=always "$B" "$DIR/$NBASE.ext4"; B="$DIR/$NBASE.ext4"; fi
if [ "$NLAYER" != "$LAYER" ]; then ln -f "$L" "$DIR/$NLAYER.layer.ext4"; fi
mnt="$(mktemp -d)"
montado=0
desmontar() {
  if [ "$montado" = 1 ]; then umount "$mnt/proc" "$mnt/dev" 2>/dev/null || true; umount "$mnt"; fi
  rmdir "$mnt"
}
trap desmontar EXIT
if ! debugfs -R 'stat /usr/sbin/dmsetup' "$B" 2>/dev/null | grep -q Inode; then
  # dmsetup + libdevmapper son ~1 MiB, pero las listas de apt piden ~300 MiB
  # mientras dura; al final se encoge (abajo).
  truncate -s $(( $(stat -c %s "$B") + 384 * 1048576 )) "$B"
  e2fsck -fy "$B" >/dev/null || true
  resize2fs "$B" >/dev/null
fi
mount -o loop "$B" "$mnt"; montado=1
if [ ! -x "$mnt/usr/sbin/dmsetup" ]; then
  log "instalando dmsetup en la base $NBASE"
  mount --bind /dev "$mnt/dev"; mount -t proc proc "$mnt/proc"
  rm -f "$mnt/etc/resolv.conf.kv"; [ -e "$mnt/etc/resolv.conf" ] && mv "$mnt/etc/resolv.conf" "$mnt/etc/resolv.conf.kv"
  cp -L /etc/resolv.conf "$mnt/etc/resolv.conf"
  chroot "$mnt" /usr/bin/env DEBIAN_FRONTEND=noninteractive PATH=/usr/sbin:/usr/bin:/sbin:/bin \
    sh -c 'apt-get update -qq -o Acquire::Languages=none && apt-get install -y -qq --no-install-recommends dmsetup >/dev/null && apt-get clean && rm -rf /var/lib/apt/lists/*'
  rm -f "$mnt/etc/resolv.conf"; [ -e "$mnt/etc/resolv.conf.kv" ] && mv "$mnt/etc/resolv.conf.kv" "$mnt/etc/resolv.conf"
  umount "$mnt/proc" "$mnt/dev"
fi
mkdir -p "$mnt/etc/kindling-android"
{
  echo "# dm-verity de la capa (prototypes/android/image/verity.sh). Lo lee /sbin/overlay-init."
  echo "# root_hash=$ROOT salt=$SALT data_blocks=$nb hash_blocks=$HASH_BLOCKS fec_roots=$FEC_ROOTS"
  echo "$TABLE"
} >"$mnt/etc/kindling-android/layer.verity"
init="$mnt/sbin/overlay-init"
if ! grep -q 'kindling-android: dm-verity' "$init"; then
  # shellcheck disable=SC2016  # el texto literal de la línea del init
  ancla='  mount -t ext4 -o ro "$LAYER_DEV" /overlay/svc'
  grep -qxF "$ancla" "$init" || { echo "$init: can't find where the layer is mounted" >&2; exit 1; }
  python3 - "$init" "$ancla" <<'PY'
import sys
p, ancla = sys.argv[1], sys.argv[2]
bloque = r'''  # kindling-android: dm-verity (prototypes/android/image/verity.sh). La capa
  # se lee a través de dm-verity: un bloque que no cuadra con su hash da EIO
  # (y el kernel lo relee) en vez de entrar en la caché como bueno.
  if [ -f /etc/kindling-android/layer.verity ]; then
    tabla="$(grep -v '^#' /etc/kindling-android/layer.verity | sed "s#@DEV@#$LAYER_DEV#g")"
    if ! DM_DISABLE_UDEV=1 dmsetup create android-layer --readonly --table "$tabla"; then
      echo "kindling-android: dm-verity on $LAYER_DEV failed; refusing to mount the layer unverified" >&2
      exit 1
    fi
    LAYER_DEV=/dev/mapper/android-layer
  fi
'''
s = open(p).read()
s = s.replace(ancla + "\n", bloque + ancla + "\n", 1)
open(p, "w").write(s)
PY
fi
grep -q 'kling.layer' "$init" || { echo "$init lost kling.layer" >&2; exit 1; }
sync
umount "$mnt"; montado=0
# Encoger: lo mínimo más 16 MiB de holgura (la base se monta de solo lectura).
e2fsck -fy "$B" >/dev/null || true
resize2fs -M "$B" >/dev/null 2>&1
nbb="$(dumpe2fs -h "$B" 2>/dev/null | sed -n 's/^Block count: *//p')"
bsb="$(dumpe2fs -h "$B" 2>/dev/null | sed -n 's/^Block size: *//p')"
resize2fs "$B" $(( (nbb * bsb + 16 * 1048576) / 1024 ))K >/dev/null 2>&1
nbb="$(dumpe2fs -h "$B" 2>/dev/null | sed -n 's/^Block count: *//p')"
truncate -s $((nbb * bsb)) "$B"
e2fsck -fn "$B" >/dev/null
log "base $NBASE: $(( $(stat -c %s "$B") >> 20 )) MiB"

# ── 4. receta ────────────────────────────────────────────────────────────────
R="$DIR/$LAYER.recipe.json"
[ -f "$R" ] || { echo "no $R" >&2; exit 1; }
python3 - "$R" "$DIR/$NLAYER.recipe.json" "$NLAYER" "$NBASE" "$ROOT" "$SALT" "$FEC_ROOTS" <<'PY'
import json, sys
src, dst, name, base, root, salt, fec = sys.argv[1:]
r = json.load(open(src))
r["name"], r["base"] = name, base
r.setdefault("spec", {}).update({"verity_root_hash": root, "verity_salt": salt,
                                 "verity_fec_roots": int(fec), "verity_hash": "sha256"})
json.dump(r, open(dst, "w"), indent=2)
PY
log "listo: $NLAYER (capa con verity) sobre $NBASE; raíz $ROOT"
