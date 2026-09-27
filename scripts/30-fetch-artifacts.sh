#!/usr/bin/env bash
# Descarga kernel y rootfs del CI de Firecracker y deja un ext4 escribible.
#
# Las rutas `firecracker-ci/v1.x/` que salen en todos los tutoriales devuelven 404.
# El bucket usa directorios por fecha y no hay alias `latest`, así que hay que
# listarlo y quedarse con el más reciente. Ver docs/hallazgos.md.
#
# KERNEL_SOURCE elige de dónde sale el kernel:
#   ci     (por defecto) el vmlinux del CI de Firecracker, serie $KERNEL_PREF;
#   build  el kernel mínimo propio, compilado con scripts/builders/kernel/build.sh
#          (versión fijada en kernel.pin). El rootfs sigue saliendo del CI.
# Se cambia de uno a otro sin tocar nada más: `vmlinux` es un enlace.
set -euo pipefail

BUCKET="https://s3.amazonaws.com/spec.ccfc.min"
ARCH="$(uname -m)"
DEST="${DEST:-/opt/fc}"
KERNEL_PREF="${KERNEL_PREF:-6.1}"      # serie de kernel preferida
ROOTFS_SIZE="${ROOTFS_SIZE:-800M}"
KERNEL_SOURCE="${KERNEL_SOURCE:-ci}"   # ci | build
KBUILD="$(cd "$(dirname "$0")" && pwd)/builders/kernel/build.sh"

case "$KERNEL_SOURCE" in
  ci|build) ;;
  *) echo "KERNEL_SOURCE debe ser ci o build (es '$KERNEL_SOURCE')" >&2; exit 1 ;;
esac

for c in curl unsquashfs mkfs.ext4; do
  command -v "$c" >/dev/null || { echo "falta $c (apt install squashfs-tools e2fsprogs curl)" >&2; exit 1; }
done

echo "buscando la build más reciente del CI..."
BUILD="$(curl -sfL "$BUCKET/?list-type=2&prefix=firecracker-ci/&delimiter=/" \
  | tr '<' '\n' | grep -oE 'firecracker-ci/[0-9]{8}-[a-z0-9]+-[0-9]+/' | sort -u | tail -1)"
[ -n "$BUILD" ] || { echo "no pude listar el bucket" >&2; exit 1; }
echo "  -> $BUILD"

BASE="${BUILD}${ARCH}/"
LISTING="$(curl -sfL "$BUCKET/?list-type=2&prefix=${BASE}&max-keys=200" \
  | tr '<' '\n' | grep -oE "${BASE}[^ ]+" | grep -v '/debug/' | sed "s|$BASE||" | sort -u)"

ROOTFS="$(echo "$LISTING" | grep -E '^ubuntu-[0-9.]+\.squashfs$'      | sort -V | tail -1)"
if [ "$KERNEL_SOURCE" = build ]; then
  KERNEL="$("$KBUILD" --print-name)"
else
  KERNEL="$(echo "$LISTING" | grep -E "^vmlinux-${KERNEL_PREF}\.[0-9]+$" | sort -V | tail -1)"
  [ -n "$KERNEL" ] || { echo "sin kernel para la serie $KERNEL_PREF. Disponibles:"; echo "$LISTING" | grep vmlinux; exit 1; }
fi
[ -n "$ROOTFS" ] || { echo "sin rootfs ubuntu en la build" >&2; exit 1; }
echo "  kernel: $KERNEL ($KERNEL_SOURCE)"
echo "  rootfs: $ROOTFS"

mkdir -p "$DEST" && cd "$DEST"
if [ "$KERNEL_SOURCE" = build ]; then
  [ -f "$KERNEL" ]      || OUT="$DEST" "$KBUILD"
else
  [ -f "$KERNEL" ]      || curl -#fL "$BUCKET/${BASE}${KERNEL}" -o "$KERNEL"
fi
[ -f "$ROOTFS" ]        || curl -#fL "$BUCKET/${BASE}${ROOTFS}" -o "$ROOTFS"

# squashfs es de solo lectura: lo pasamos a ext4 para poder escribir dentro
echo "convirtiendo a ext4 escribible..."
work="$(mktemp -d)"; trap 'rm -rf "$work"' EXIT
rm -f rootfs.ext4
unsquashfs -q -f -d "$work/rootfs" "$ROOTFS"
mkfs.ext4 -q -F -d "$work/rootfs" rootfs.ext4 "$ROOTFS_SIZE"

ln -sf "$KERNEL" vmlinux
echo
echo "listo en $DEST:"
ls -lh vmlinux "$KERNEL" rootfs.ext4 | awk '{print "  " $5, $9}'
