#!/usr/bin/env bash
# Construye una imagen base mínima sobre Alpine, pensada para herramientas
# efímeras: sin systemd, sin gestor de servicios, solo busybox y lo que añadas.
#
#   sudo ./70-build-minimal-image.sh                 # imagen "min"
#   sudo PKGS="nodejs npm" ./70-build-minimal-image.sh mcp-node
#
# El objetivo es el working set congelado, no el tamaño del disco: arrancar
# Ubuntu con systemd cuesta ~80 MB de snapshot; esto debería quedarse cerca de
# los ~36 MB que mide un arranque a /bin/sh pelado.
set -euo pipefail

NAME="${1:-min}"
ROOT="${KLING_ROOT:-/var/lib/kindling}"
SIZE="${SIZE:-256M}"
PKGS="${PKGS:-}"
HERE="$(cd "$(dirname "$0")" && pwd)"

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
for c in curl mkfs.ext4; do
  command -v "$c" >/dev/null || { echo "falta $c" >&2; exit 1; }
done

case "$(uname -m)" in
  x86_64)  ALPINE_ARCH=x86_64 ;;
  aarch64) ALPINE_ARCH=aarch64 ;;
  *) echo "arquitectura no soportada: $(uname -m)" >&2; exit 1 ;;
esac

BASE="https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/$ALPINE_ARCH"
echo "buscando el minirootfs de Alpine más reciente ($ALPINE_ARCH)..."
TARBALL="$(curl -sfL "$BASE/" \
  | grep -oE "alpine-minirootfs-[0-9.]+-$ALPINE_ARCH\.tar\.gz" | sort -V | tail -1)"
[ -n "$TARBALL" ] || { echo "no pude resolver el minirootfs" >&2; exit 1; }
echo "  -> $TARBALL"

work="$(mktemp -d)"
mnt="$(mktemp -d)"
cleanup() { umount "$mnt" 2>/dev/null || true; rm -rf "$work"; rmdir "$mnt" 2>/dev/null || true; }
trap cleanup EXIT

curl -#fL "$BASE/$TARBALL" -o "$work/rootfs.tar.gz"

mkdir -p "$ROOT/images"
DEST="$ROOT/images/$NAME.ext4"

# Disperso: el tamaño lógico es un techo, no una reserva.
rm -f "$DEST"
truncate -s "$SIZE" "$DEST"
mkfs.ext4 -q -F -E nodiscard "$DEST"
mount -o loop "$DEST" "$mnt"

echo "extrayendo..."
tar -xzf "$work/rootfs.tar.gz" -C "$mnt"

if [ -n "$PKGS" ]; then
  echo "instalando: $PKGS"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  chroot "$mnt" /sbin/apk add --no-cache $PKGS
  umount "$mnt/proc" 2>/dev/null || true
fi

install -m755 "$HERE/minimal-init.sh" "$mnt/sbin/overlay-init"
mkdir -p "$mnt/overlay" "$mnt/rom" "$mnt/run"

umount "$mnt"
e2fsck -fp "$DEST" >/dev/null 2>&1 || true
resize2fs -M "$DEST" >/dev/null 2>&1 || true   # encoge al contenido real

echo
echo "imagen '$NAME' lista:"
echo "  lógico: $(du -h --apparent-size "$DEST" | cut -f1)   real: $(du -h "$DEST" | cut -f1)"
echo "  úsala con:  kling run -image $NAME"
