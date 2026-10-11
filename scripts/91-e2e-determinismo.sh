#!/usr/bin/env bash
# e2e de los aleatorios en las copias de un dorado (issue #175).
#
# Un proceso vivo cuando se hace `kling save` despierta igual en todas las
# copias: /resync vuelve a sembrar el kernel, pero no lo que el proceso guarda
# en memoria (Math.random y randomUUID de V8, random de Python, el DRBG de
# OpenSSL dentro del proceso). Esta prueba lo reproduce y comprueba las dos
# salidas que da kindling:
#
#   1. Node sin nada: `save` avisa de los procesos vivos, y las copias repiten
#      los mismos valores (el fallo, para ver que la prueba lo detecta).
#   2. Node con `image import -on-restore restart`: `save` ya no avisa, y cada
#      copia (`run -from -wait-ready`) da valores distintos.
#   3. Python con `-on-restore signal:SIGUSR1`: la app vuelve a sembrar al
#      recibirla, sin reiniciarse.
#   4. /run/kindling/generation existe en la copia y vale 1.
#
# Corre contra el daemon de KLING_HOST (Linux con Firecracker o macOS con vz) y
# necesita red para bajar node:22-alpine y python:3.12-alpine.
#
#   KLING=./kling COPIES=3 ./scripts/91-e2e-determinismo.sh
#
#   KLING   el CLI (por defecto kling del PATH)
#   COPIES  copias por dorado (3)
#   KEEP=1  no borra máquinas, dorados ni imágenes al acabar
#
# Cada comprobación dice qué esperaba y qué obtuvo; un fallo no aborta el
# resto. Salida en inglés, como el CLI.
set -uo pipefail

KLING="${KLING:-kling}"
COPIES="${COPIES:-3}"
KEEP="${KEEP:-0}"
P="det$$"

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFAIL\033[0m  %s\n     want: %s\n     got:  %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }
k() { "$KLING" "$@"; }

limpiar() {
  [ "$KEEP" = 1 ] && return
  for i in $(seq 1 "$COPIES"); do
    for v in a b c; do k rm "$P-$v$i" >/dev/null 2>&1; done
  done
  k rm "$P-b0" >/dev/null 2>&1
  for v in a b c; do
    k rm "$P-src-$v" >/dev/null 2>&1
    k template rm "$P-tpl-$v" >/dev/null 2>&1
  done
  for img in "$P-node" "$P-node-r" "$P-py"; do k image rm "$img" >/dev/null 2>&1; done
}
trap limpiar EXIT

# El servidor genera un aleatorio ANTES de volcarse (como una app que ya
# atendió algo) y responde con tres más en cada petición.
NODE_SRV='const http=require("http"),c=require("crypto");c.randomUUID();Math.random();
http.createServer((q,r)=>r.end(JSON.stringify({u:c.randomUUID(),m:Math.random(),b:c.randomBytes(8).toString("hex")})+"\n")).listen(9000);'
# Python: SIGUSR1 vuelve a sembrar random y el DRBG de OpenSSL.
PY_SRV='import http.server,json,random,signal,ssl,os
random.random()
def resembrar(*_):
    random.seed()
    ssl.RAND_add(os.urandom(32), 32.0)
signal.signal(signal.SIGUSR1, resembrar)
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(s):
        b=(json.dumps({"r":random.random(),"s":ssl.RAND_bytes(8).hex()})+"\n").encode()
        s.send_response(200); s.end_headers(); s.wfile.write(b)
    def log_message(s,*a): pass
http.server.HTTPServer(("0.0.0.0",9000),H).serve_forever()'

# dorado VAR IMAGEN: arranca la plantilla, espera a que sirva y la guarda.
# Deja en SAVE_OUT lo que dijo save (los avisos van por stderr).
dorado() {
  local v="$1" img="$2"
  k run -name "$P-src-$v" -image "$img" -allow-exec -wait-ready >/dev/null || return 1
  # Que el servidor ya escuche (sin HEALTHCHECK ni EXPOSE no hay sonda).
  for _ in $(seq 1 100); do
    k exec "$P-src-$v" -- wget -qO- 127.0.0.1:9000 >/dev/null 2>&1 && break
    sleep 0.2
  done
  SAVE_OUT="$(k save "$P-src-$v" "$P-tpl-$v" 2>&1)"
}

# ms: milisegundos desde el arranque del reloj (date +%s%N no existe en macOS).
ms() { python3 -c 'import time; print(int(time.time()*1000))'; }

# copias VAR: COPIES copias del dorado y una petición a cada una. Deja las
# respuestas en RESP, una por línea, y lo que tardó cada run -from en RUNMS.
copias() {
  local v="$1" i out t0
  RESP="" RUNMS=""
  for i in $(seq 1 "$COPIES"); do
    t0="$(ms)"
    k run -name "$P-$v$i" -from "$P-tpl-$v" -wait-ready >/dev/null || { RESP+="run failed"$'\n'; continue; }
    RUNMS+="$(( $(ms) - t0 )) "
    out=""
    for _ in $(seq 1 100); do
      out="$(k exec "$P-$v$i" -- wget -qO- 127.0.0.1:9000 2>/dev/null)" && [ -n "$out" ] && break
      sleep 0.2
    done
    RESP+="$out"$'\n'
  done
}

distintas() { printf '%s' "$1" | grep -v '^$' | sort -u | wc -l | tr -d ' '; }

step "1. node without on-restore: save warns, copies repeat"
k image import node:22-alpine -name "$P-node" -- node -e "$NODE_SRV" >/dev/null || bad "import node" "ok" "failed"
if dorado a "$P-node"; then
  if contiene "$SAVE_OUT" "alive in this snapshot"; then ok "save warns about live processes"
  else bad "save warns about live processes" "a warning" "$SAVE_OUT"; fi
  copias a
  echo "     run -from -wait-ready (ms): $RUNMS"
  n="$(distintas "$RESP")"
  if [ "$n" = 1 ]; then ok "the $COPIES copies repeat the same values (the bug this test catches)"
  else bad "copies without on-restore repeat" "1 distinct answer" "$n: $RESP"; fi
else bad "template a" "running and saved" "failed"; fi

step "2. node with -on-restore restart: no warning, distinct copies"
k image import node:22-alpine -name "$P-node-r" -on-restore restart -- node -e "$NODE_SRV" >/dev/null || bad "import node -on-restore" "ok" "failed"
if dorado b "$P-node-r"; then
  if contiene "$SAVE_OUT" "alive in this snapshot"; then bad "save stays quiet" "no warning" "$SAVE_OUT"
  else ok "save does not warn: the service restarts in every copy"; fi
  copias b
  echo "     run -from -wait-ready (ms): $RUNMS"
  n="$(distintas "$RESP")"
  if [ "$n" = "$COPIES" ]; then ok "$COPIES copies, $COPIES distinct answers"
  else bad "distinct answers after restart" "$COPIES" "$n: $RESP"; fi
  gen="$(k exec "$P-b1" -- cat /run/kindling/generation 2>&1)"
  if [ "$gen" = 1 ]; then ok "/run/kindling/generation is 1 in the copy"
  else bad "/run/kindling/generation" "1" "$gen"; fi
  # Una copia nueva y UNA petición, sin reintentos: -wait-ready tiene que
  # haber esperado a que el proceso reiniciado vuelva a escuchar.
  k run -name "$P-b0" -from "$P-tpl-b" -wait-ready >/dev/null
  st="$(k exec "$P-b0" -- sh -c 'wget -qO- 127.0.0.1:9000 >/dev/null && echo up' 2>&1)"
  if [ "$st" = up ]; then ok "the restarted service answers right after run -from -wait-ready"
  else bad "service up after -wait-ready" "up" "$st"; fi
else bad "template b" "running and saved" "failed"; fi

step "3. python with -on-restore signal:SIGUSR1: reseeds without a restart"
k image import python:3.12-alpine -name "$P-py" -on-restore signal:SIGUSR1 -- python3 -c "$PY_SRV" >/dev/null || bad "import python" "ok" "failed"
if dorado c "$P-py"; then
  copias c
  echo "     run -from -wait-ready (ms): $RUNMS"
  n="$(distintas "$RESP")"
  if [ "$n" = "$COPIES" ]; then ok "$COPIES copies, $COPIES distinct answers"
  else bad "distinct answers after the signal" "$COPIES" "$n: $RESP"; fi
else bad "template c" "running and saved" "failed"; fi

printf "\n%d ok, %d failed\n" "$pass" "$fail"
[ "$fail" = 0 ]
