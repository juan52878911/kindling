#!/bin/sh
# /sbin/overlay-init — se inyecta en la imagen base y corre como init del invitado.
#
# Monta un overlayfs con la imagen base (/dev/vda, solo lectura y compartida por
# todas las microVMs) como capa inferior, y el disco propio de esta máquina
# (/dev/vdb) como capa escribible. Después cede el control al init real.
#
# Esto es lo que permite que N máquinas compartan una base de 800 MB en vez de
# copiarla N veces.
set -e

OVERLAY_DEV="${OVERLAY_DEV:-/dev/vdb}"

mount -t proc  proc  /proc 2>/dev/null || true
mount -t sysfs sysfs /sys  2>/dev/null || true

mkdir -p /overlay
mount -t ext4 "$OVERLAY_DEV" /overlay

mkdir -p /overlay/upper /overlay/work /overlay/merged
mount -t overlay overlay \
  -o lowerdir=/,upperdir=/overlay/upper,workdir=/overlay/work \
  /overlay/merged

# La raíz anterior queda accesible en /rom por si hace falta inspeccionarla.
mkdir -p /overlay/merged/rom
cd /overlay/merged
pivot_root . rom

exec /sbin/init
