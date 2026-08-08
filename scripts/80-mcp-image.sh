#!/usr/bin/env bash
# Envuelve un servidor MCP en una imagen de kindling.
#
# DOS MODOS, según cómo hable el servidor:
#
#   HTTP nativo — el servidor ya escucha en un puerto:
#     sudo ./80-mcp-image.sh http <nombre> <directorio> [paquetes apk]
#     El directorio necesita un `entrypoint` ejecutable que escuche en :8080.
#
#   stdio — el servidor habla JSON-RPC por tuberías, que es el caso MAYORITARIO
#   entre los servidores MCP open source:
#     sudo ./80-mcp-image.sh stdio <nombre> [-p "paquetes"] [-d dir] -- <comando>
#
#     Se le añade kling-bridge, que lanza el servidor como hijo y expone su
#     protocolo por HTTP. El servidor no se entera de nada.
#
# Ejemplos:
#   sudo ./80-mcp-image.sh stdio files -p "nodejs npm" -- \
#        npx -y @modelcontextprotocol/server-filesystem /data
#   sudo ./80-mcp-image.sh stdio eco -d ./examples/stdio-server-bin -- /opt/mcp/server
#   sudo ./80-mcp-image.sh http legacy ./mi-servidor-http
set -euo pipefail

MODE="${1:?uso: $0 <http|stdio> <nombre> ...}"; shift
NAME="${1:?falta el nombre de la imagen}"; shift

ROOT="${KLING_ROOT:-/var/lib/kindling}"
BASE="${BASE_IMAGE:-min}"
GROW="${GROW:-0}"
BRIDGE="${BRIDGE:-./kling-bridge}"

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
[ -f "$ROOT/images/$BASE.ext4" ] || {
  echo "falta la imagen base '$BASE'. Constrúyela con 70-build-minimal-image.sh" >&2; exit 1; }

PKGS=""; EXTRA_DIR=""; CMD=()
case "$MODE" in
  http)
    EXTRA_DIR="${1:?falta el directorio del servidor}"; shift
    PKGS="${1:-}"
    [ -x "$EXTRA_DIR/entrypoint" ] || { echo "falta $EXTRA_DIR/entrypoint ejecutable" >&2; exit 1; }
    ;;
  stdio)
    while [ $# -gt 0 ]; do
      case "$1" in
        -p) PKGS="$2"; shift 2 ;;
        -d) EXTRA_DIR="$2"; shift 2 ;;
        --) shift; CMD=("$@"); break ;;
        *)  echo "opción desconocida: $1" >&2; exit 1 ;;
      esac
    done
    [ ${#CMD[@]} -gt 0 ] || { echo "falta el comando tras --" >&2; exit 1; }
    [ -f "$BRIDGE" ] || { echo "no encuentro $BRIDGE (compílalo con: make bridge)" >&2; exit 1; }
    # El servidor puede tardar en instalarse; con stdio suele hacer falta sitio.
    [ "$GROW" -eq 0 ] && GROW=256
    ;;
  *) echo "modo desconocido '$MODE': usa http o stdio" >&2; exit 1 ;;
esac

DEST="$ROOT/images/$NAME.ext4"
cp "$ROOT/images/$BASE.ext4" "$DEST"

if [ "$GROW" -gt 0 ]; then
  truncate -s "+${GROW}M" "$DEST"
  e2fsck -fp "$DEST" >/dev/null 2>&1 || true
  resize2fs "$DEST" >/dev/null 2>&1
fi

mnt="$(mktemp -d)"
cleanup() { umount "$mnt/proc" 2>/dev/null || true; umount "$mnt" 2>/dev/null || true; rmdir "$mnt" 2>/dev/null || true; }
trap cleanup EXIT
mount -o loop "$DEST" "$mnt"

[ -n "$EXTRA_DIR" ] && cp -a "$EXTRA_DIR"/. "$mnt"/

if [ -n "$PKGS" ]; then
  echo "instalando en la imagen: $PKGS"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  chroot "$mnt" /sbin/apk add --no-cache $PKGS
  umount "$mnt/proc" 2>/dev/null || true
fi

if [ "$MODE" = "stdio" ]; then
  install -m755 "$BRIDGE" "$mnt/usr/local/bin/kling-bridge"
  # El entrypoint es PID 1 de la microVM: si muere, el kernel entra en pánico.
  # `exec` evita dejar un shell intermedio que no aporta nada.
  {
    echo '#!/bin/sh'
    echo '# Generado por 80-mcp-image.sh — envoltorio stdio -> Streamable HTTP.'
    printf 'exec /usr/local/bin/kling-bridge -listen :8080 --'
    for a in "${CMD[@]}"; do printf ' %q' "$a"; done
    echo
  } > "$mnt/entrypoint"
  chmod +x "$mnt/entrypoint"
fi

chmod +x "$mnt/entrypoint"
umount "$mnt"
chmod a+r "$DEST"

echo "imagen '$NAME' lista ($(du -h "$DEST" | cut -f1) reales)"
echo
echo "Congélala como servicio del gateway:"
echo "  kling run -name ${NAME}-tmpl -image $NAME -service $NAME"
echo "  kling commit ${NAME}-tmpl $NAME && kling stop ${NAME}-tmpl"
echo
echo "Y a partir de ahí el gateway la despierta sola:"
echo "  curl -X POST http://127.0.0.1:8080/mcp/$NAME/ -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}'"
