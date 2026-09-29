#!/bin/bash
# wall-mac.sh — abre en el navegador el muro con todas las pantallas.
#
#   ./wall/wall-mac.sh            # teléfonos del CT de Proxmox (ssh phones)
#   ./wall/wall-mac.sh -local     # teléfonos del Mac (daemon privado de phone.sh)
#   ./wall/wall-mac.sh stop       # para el muro y el túnel
#
# En el CT: sube wall.py, lo arranca en 127.0.0.1:$PORT (solo local: tocar un
# teléfono es controlarlo) y lo trae al Mac con un túnel ssh -L. Variables:
# PORT (8765), MATCH (regex de nombres, por defecto todas las máquinas que
# corren), HOST (phones).
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
PORT="${PORT:-8765}"
HOST="${HOST:-phones}"
MATCH="${MATCH:-}"
PIDF="${TMPDIR:-/tmp}/kindling-wall-$PORT.pid"

parar() {
  if [ -f "$PIDF" ]; then kill "$(cat "$PIDF")" 2>/dev/null || true; rm -f "$PIDF"; fi
  ssh -o BatchMode=yes "$HOST" "pkill -f 'wall.py -port $PORT' || true" 2>/dev/null || true
}

case "${1:-}" in
  stop) parar; echo "wall: stopped"; exit 0 ;;
  -local)
    R="${PHONE_ROOT:-$HOME/.kindling-android-telefono}"
    export KLING_HOST="${KLING_HOST:-unix://$R/kling.sock}"
    ( nohup python3 "$HERE/wall.py" -port "$PORT" ${MATCH:+-match "$MATCH"} >/dev/null 2>&1 & echo $! >"$PIDF" )
    sleep 1; [ -n "${NO_OPEN:-}" ] || open "http://127.0.0.1:$PORT/"; echo "wall: http://127.0.0.1:$PORT/ (local)"; exit 0 ;;
esac

parar
scp -q "$HERE/wall.py" "$HOST:/usr/local/bin/kindling-wall.py"
ssh -o BatchMode=yes "$HOST" "nohup python3 /usr/local/bin/kindling-wall.py -port $PORT ${MATCH:+-match '$MATCH'} >/var/log/kindling-wall.log 2>&1 </dev/null & sleep 1; pgrep -f 'wall.py -port $PORT' >/dev/null"
( ssh -o BatchMode=yes -o ExitOnForwardFailure=yes -N -L "127.0.0.1:$PORT:127.0.0.1:$PORT" "$HOST" & echo $! >"$PIDF" )
for _ in 1 2 3 4 5 6 7 8 9 10; do curl -fs "http://127.0.0.1:$PORT/api/phones" >/dev/null 2>&1 && break; sleep 0.5; done
[ -n "${NO_OPEN:-}" ] || open "http://127.0.0.1:$PORT/"
echo "wall: http://127.0.0.1:$PORT/ (CT $HOST; stop: $0 stop)"
