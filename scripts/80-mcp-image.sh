#!/usr/bin/env bash
# Envuelve un servidor MCP en una imagen de kindling.
#
# DOS MODOS, según cómo hable el servidor:
#
#   HTTP nativo — el servidor ya habla Streamable HTTP y escucha él mismo.
#   No lleva puente: el gateway le habla directamente.
#     sudo ./80-mcp-image.sh http <nombre> [-p "apk"] [-n "npm"] [-d dir] -- <comando>
#     sudo ./80-mcp-image.sh http <nombre> <directorio> [paquetes apk]
#
#     El servidor debe escuchar en el puerto que le diga $PORT (8080) y servir
#     el protocolo en /mcp, que es donde el gateway llama. La segunda forma
#     espera un `entrypoint` ejecutable en el directorio.
#
#   stdio — el servidor habla JSON-RPC por tuberías, que es el caso MAYORITARIO
#   entre los servidores MCP open source:
#     sudo ./80-mcp-image.sh stdio <nombre> [-p "apk"] [-n "npm"] [-d dir] -- <comando>
#
#     -n PREINSTALA paquetes npm en la imagen. Es obligatorio, no una comodidad:
#        las microVMs arrancan sin salida a internet, así que un `npx -y` en
#        tiempo de ejecución fallaría al intentar descargar.
#
#     Se le añade kling-bridge, que lanza el servidor como hijo y expone su
#     protocolo por HTTP. El servidor no se entera de nada.
#
# Ejemplos:
#   sudo ./80-mcp-image.sh stdio files -p "nodejs npm" \
#        -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data
#   sudo ./80-mcp-image.sh stdio eco -d ./examples/stdio-server-bin -- /opt/mcp/server
#   sudo ./80-mcp-image.sh http everything -p "nodejs npm" \
#        -n "@modelcontextprotocol/server-everything" -- mcp-server-everything streamableHttp
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

PKGS=""; NPM=""; EXTRA_DIR=""; CMD=()

# Los dos modos se instalan igual; solo cambia quién habla HTTP al final.
parse_build_opts() {
  while [ $# -gt 0 ]; do
    case "$1" in
      -p) PKGS="$2"; shift 2 ;;
      -n) NPM="$2"; shift 2 ;;
      -d) EXTRA_DIR="$2"; shift 2 ;;
      --) shift; CMD=("$@"); break ;;
      *)  echo "opción desconocida: $1" >&2; exit 1 ;;
    esac
  done
  [ ${#CMD[@]} -gt 0 ] || { echo "falta el comando tras --" >&2; exit 1; }
  # Node y sus dependencias piden bastante más sitio que un binario suelto.
  if [ "$GROW" -eq 0 ]; then
    [ -n "$NPM" ] && GROW=768 || GROW=256
  fi
  # npm implica node: se añade si no se pidió explícitamente.
  if [ -n "$NPM" ] && ! echo " $PKGS " | grep -q " nodejs "; then
    PKGS="$PKGS nodejs npm"
  fi
}

case "$MODE" in
  http)
    # Forma antigua: un directorio que ya trae su propio entrypoint.
    if [ $# -gt 0 ] && [ -d "$1" ]; then
      EXTRA_DIR="$1"; shift
      PKGS="${1:-}"
      [ -x "$EXTRA_DIR/entrypoint" ] || { echo "falta $EXTRA_DIR/entrypoint ejecutable" >&2; exit 1; }
    else
      parse_build_opts "$@"
    fi
    ;;
  stdio)
    parse_build_opts "$@"
    [ -f "$BRIDGE" ] || { echo "no encuentro $BRIDGE (compílalo con: make bridge)" >&2; exit 1; }
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

if [ -n "$NPM" ]; then
  echo "preinstalando npm: $NPM"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  # --omit=dev recorta lo que no hace falta en ejecución; el espacio dentro de la
  # imagen es el que más pesa en el snapshot.
  chroot "$mnt" /usr/bin/npm install -g --omit=dev --no-fund --no-audit $NPM
  chroot "$mnt" /bin/sh -c 'rm -rf /root/.npm /usr/lib/node_modules/npm/man' 2>/dev/null || true
  umount "$mnt/proc" 2>/dev/null || true
fi

if [ "$MODE" = "stdio" ]; then
  install -m755 "$BRIDGE" "$mnt/usr/local/bin/kling-bridge"
  # El entrypoint es PID 1 de la microVM: si muere, el kernel entra en pánico.
  # `exec` evita dejar un shell intermedio que no aporta nada.
  {
    echo '#!/bin/sh'
    echo '# Generado por 80-mcp-image.sh — envoltorio stdio -> Streamable HTTP.'
    echo '#'
    echo '# El entrypoint es PID 1 y el kernel no le pasa PATH, así que hay que'
    echo '# fijarlo: sin él no se encuentran los binarios que instala npm.'
    echo 'export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
    echo 'export HOME=/root'
    printf 'exec /usr/local/bin/kling-bridge -listen :8080 --'
    for a in "${CMD[@]}"; do printf ' %q' "$a"; done
    echo
  } > "$mnt/entrypoint"
elif [ ${#CMD[@]} -gt 0 ]; then
  # HTTP nativo: no hay puente. El servidor escucha él mismo, y el gateway le
  # habla igual que a cualquier otro: POST a /mcp del puerto 8080.
  {
    echo '#!/bin/sh'
    echo '# Generado por 80-mcp-image.sh — servidor HTTP nativo, sin puente.'
    echo '#'
    echo '# El entrypoint es PID 1 y el kernel no le pasa PATH, así que hay que'
    echo '# fijarlo: sin él no se encuentran los binarios que instala npm.'
    echo 'export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
    echo 'export HOME=/root'
    echo '# 8080 es el puerto que busca el gateway dentro del invitado.'
    echo 'export PORT=8080'
    printf 'exec'
    for a in "${CMD[@]}"; do printf ' %q' "$a"; done
    echo
  } > "$mnt/entrypoint"
fi

[ -f "$mnt/entrypoint" ] || { echo "la imagen se queda sin entrypoint" >&2; exit 1; }
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
