#!/usr/bin/env bash
# Prepara una imagen base de kindling: inyecta overlay-init y la deja lista para
# ser compartida en solo lectura por todas las microVMs.
#
#   sudo ./50-prepare-image.sh /opt/fc/rootfs.ext4 default
set -euo pipefail

SRC="${1:?uso: $0 <rootfs.ext4> [nombre-imagen]}"
NAME="${2:-default}"
ROOT="${KLING_ROOT:-/var/lib/kindling}"
HERE="$(cd "$(dirname "$0")" && pwd)"

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
[ -f "$SRC" ] || { echo "no existe $SRC" >&2; exit 1; }
[ -f "$HERE/overlay-init.sh" ] || { echo "falta overlay-init.sh junto a este script" >&2; exit 1; }

mkdir -p "$ROOT/images"
DEST="$ROOT/images/$NAME.ext4"

echo "copiando $SRC -> $DEST"
cp "$SRC" "$DEST"

mnt="$(mktemp -d)"
trap 'umount "$mnt" 2>/dev/null || true; rmdir "$mnt"' EXIT
mount -o loop "$DEST" "$mnt"

install -m755 "$HERE/overlay-init.sh" "$mnt/sbin/overlay-init"
# El init necesita estos puntos de montaje antes de tener overlay.
mkdir -p "$mnt/overlay" "$mnt/rom"
umount "$mnt"

echo "imagen '$NAME' lista en $DEST"
echo "las microVMs la montarán en solo lectura; cada una escribe en su propio overlay."
