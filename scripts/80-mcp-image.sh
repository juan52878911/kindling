#!/usr/bin/env bash
# Envuelve un servidor MCP en una imagen de kindling.
#
#   sudo ./80-mcp-image.sh <nombre> <directorio-con-el-servidor> [paquetes apk]
#
# Ejemplos:
#   sudo ./80-mcp-image.sh echo    ./ejemplos/echo
#   sudo ./80-mcp-image.sh fs-mcp  ./mi-servidor  "nodejs npm"
#
# El directorio debe contener un ejecutable `entrypoint` que arranque el servidor
# MCP escuchando en el puerto 8080 (donde lo busca el gateway).
#
# Nota sobre Alpine: el minirootfs NO enlaza todos los applets de busybox, así
# que `/usr/sbin/httpd` no existe. Invócalos por el binario: `/bin/busybox httpd`.
set -euo pipefail

NAME="${1:?uso: $0 <nombre> <directorio> [paquetes]}"
SRC="${2:?falta el directorio del servidor}"
PKGS="${3:-}"
ROOT="${KLING_ROOT:-/var/lib/kindling}"
BASE="${BASE_IMAGE:-min}"
GROW="${GROW:-0}"   # MiB extra si el servidor no cabe

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
[ -d "$SRC" ] || { echo "no existe el directorio $SRC" >&2; exit 1; }
[ -x "$SRC/entrypoint" ] || { echo "falta $SRC/entrypoint ejecutable" >&2; exit 1; }
[ -f "$ROOT/images/$BASE.ext4" ] || { echo "falta la imagen base '$BASE' (corre 70-build-minimal-image.sh)" >&2; exit 1; }

DEST="$ROOT/images/$NAME.ext4"
cp "$ROOT/images/$BASE.ext4" "$DEST"

if [ "$GROW" -gt 0 ]; then
  truncate -s "+${GROW}M" "$DEST"
  e2fsck -fp "$DEST" >/dev/null 2>&1 || true
  resize2fs "$DEST" >/dev/null 2>&1
fi

mnt="$(mktemp -d)"
trap 'umount "$mnt" 2>/dev/null || true; rmdir "$mnt" 2>/dev/null || true' EXIT
mount -o loop "$DEST" "$mnt"

cp -a "$SRC"/. "$mnt"/
chmod +x "$mnt/entrypoint"

if [ -n "$PKGS" ]; then
  echo "instalando: $PKGS"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  chroot "$mnt" /sbin/apk add --no-cache $PKGS
  umount "$mnt/proc" 2>/dev/null || true
fi

umount "$mnt"
chmod a+r "$DEST"

echo "imagen '$NAME' lista ($(du -h "$DEST" | cut -f1) reales)"
echo
echo "Siguiente paso — congélala como servicio del gateway:"
echo "  kling run -name ${NAME}-tmpl -image $NAME -service $NAME"
echo "  kling commit ${NAME}-tmpl $NAME"
echo "  kling stop ${NAME}-tmpl"
echo
echo "Y a partir de ahí el gateway la despierta sola:"
echo "  curl http://127.0.0.1:8080/mcp/$NAME/"
