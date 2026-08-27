#!/bin/sh
# /sbin/overlay-init — se inyecta en la imagen base y corre como init del invitado.
#
# Monta un overlayfs con la imagen base (/dev/vda, solo lectura y compartida por
# todas las microVMs) como capa inferior, y el disco propio de esta máquina
# (/dev/vdb) como capa escribible. Después cede el control al init real.
#
# Esto es lo que permite que N máquinas compartan una base de 800 MB en vez de
# copiarla N veces.
#
# IMÁGENES POR CAPAS. Si el anfitrión engancha además una capa de servicio, su
# device llega en kling.layer=/dev/vdX y se monta de lower POR DELANTE de la
# base: lowerdir=<capa>:/. Así la base deja de duplicarse también por servicio.
# El contenido de la capa cuelga de /upper dentro de su ext4, porque se construyó
# como el upperdir de un overlay sobre la base (scripts/80-mcp-image.sh).
#
# Sin ese parámetro, el camino es el de siempre: una sola lower, la raíz. Las
# imágenes monolíticas ya construidas arrancan exactamente igual.
set -e

OVERLAY_DEV="${OVERLAY_DEV:-/dev/vdb}"

mount -t proc  proc  /proc 2>/dev/null || true
mount -t sysfs sysfs /sys  2>/dev/null || true

mkdir -p /overlay
mount -t ext4 "$OVERLAY_DEV" /overlay

mkdir -p /overlay/upper /overlay/work /overlay/merged

# La capa se monta DENTRO de /overlay y no en la raíz: la raíz es de solo
# lectura, así que aquí no se puede crear un punto de montaje nuevo.
#
# La línea de comandos se parte por palabras y se compara entera, sin regex: el
# sed de busybox va contra el regex de musl, que no trae las extensiones de GNU
# (\b y compañía), y un patrón que aquí no case dejaría la capa sin montar — un
# invitado sin /entrypoint, que no se parece en nada a la causa.
LOWER="/"
LAYER_DEV=""
for tok in $(cat /proc/cmdline); do
  case "$tok" in
    kling.layer=*) LAYER_DEV="${tok#kling.layer=}" ;;
  esac
done
if [ -n "$LAYER_DEV" ]; then
  mkdir -p /overlay/svc
  mount -t ext4 -o ro "$LAYER_DEV" /overlay/svc
  LOWER="/overlay/svc/upper:/"
fi

mount -t overlay overlay \
  -o "lowerdir=$LOWER,upperdir=/overlay/upper,workdir=/overlay/work" \
  /overlay/merged

# La raíz anterior queda accesible en /rom por si hace falta inspeccionarla.
mkdir -p /overlay/merged/rom
cd /overlay/merged
pivot_root . rom

exec /sbin/init
