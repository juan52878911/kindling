#!/usr/bin/env bash
# Prueba de extremo a extremo de kindling-mcp contra un daemon y un gateway
# REALES ya desplegados.
#
# Los tests de Go (internal/gateway/*_test.go) cubren la lógica del gateway con
# daemons falsos sobre socket unix, pero no pueden cubrir lo que de verdad
# importa aquí: que `kling add` traiga un servidor del registro y lo deje
# importado de verdad, que el gateway conteste con el token correcto y rechace
# sin él, que `heal`/`health` salgan con código 0 contra snapshots reales, y que
# enlazar un servidor externo funcione de punta a punta. El e2e del NÚCLEO
# (microVMs, volúmenes, exec) vive en kindling, no aquí — ver su propio
# scripts/90-e2e.sh.
#
#   ./scripts/90-e2e.sh                          contra kling del PATH, gateway
#                                                en http://127.0.0.1:8080
#   GATEWAY=http://192.168.2.60:8080 ./90-e2e.sh  otro gateway
#   KLING_HOST=ssh://juan@192.168.2.60 ./90-e2e.sh  daemon remoto (kling lo lee
#                                                de KLING_HOST, igual que el
#                                                resto de kling-mcp)
#   KLING_GATEWAY_TOKEN=xxx ./90-e2e.sh          token a mano, en vez de leer
#                                                /etc/kling/gateway.env
#   KEEP=1 ./90-e2e.sh                           no limpia al terminar
#
# Cada comprobación dice qué esperaba y qué obtuvo. Un fallo NO aborta el resto:
# saber que fallan tres cosas relacionadas vale más que enterarse de una.
set -uo pipefail

KLING="${KLING:-kling}"
GATEWAY="${GATEWAY:-http://127.0.0.1:8080}"
SVC="${SVC:-e2e-fs-$$}"
LINKSVC="${LINKSVC:-e2e-link-$$}"
BRIDGE_ADDR="${BRIDGE_ADDR:-127.0.0.1:18099}"
KEEP="${KEEP:-0}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFAIL\033[0m  %s\n     expected: %s\n     got:      %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }

# contiene busca una subcadena SIN tuberías.
#
# `algo | grep -q X` es una trampa con `set -o pipefail`: grep sale en cuanto
# encuentra la coincidencia y cierra la tubería, el productor recibe SIGPIPE y
# sale distinto de cero, y la tubería entera se da por fallida AUNQUE el texto
# estuviera. Una prueba que falla sobre algo que funciona es peor que no
# tenerla: manda a corregir lo que no está roto. Por la misma razón, un
# `cmd | tr ... || echo x` con pipefail imprimiría "x" DOS veces si cmd fallara
# (una del `||` de la tubería, otra si algo más abajo también comprueba $?): en
# este script cada resultado se captura primero en una variable con `=$(...)`
# y se examina después, nunca se encadena con `||` justo detrás de una tubería.
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }

need() { command -v "$1" >/dev/null || { echo "missing $1" >&2; exit 1; }; }
need "$KLING"; need curl; need python3

# ── token del gateway ─────────────────────────────────────────────────────────
#
# Nunca por flag ni por argv (ver resolveGatewayToken en cmd/kling-mcp/gateway.go):
# se lee de la variable de entorno, o del fichero 0600 que `kling gateway` deja
# en el host del daemon.
if [ -n "${KLING_GATEWAY_TOKEN:-}" ]; then
  TOKEN="$KLING_GATEWAY_TOKEN"
elif [ -n "${KLING_HOST:-}" ] && [[ "${KLING_HOST}" == ssh://* ]]; then
  TOKEN=$(ssh "${KLING_HOST#ssh://}" 'sudo cut -d= -f2 /etc/kling/gateway.env' 2>/dev/null | tr -d '\r\n')
else
  TOKEN=$(sudo cut -d= -f2 /etc/kling/gateway.env 2>/dev/null | tr -d '\r\n')
fi
if [ -z "$TOKEN" ]; then
  echo "no gateway token: set KLING_GATEWAY_TOKEN, or run this where /etc/kling/gateway.env is readable (sudo)" >&2
  exit 1
fi

BRIDGE_BIN="${BRIDGE_BIN:-}" STDIO_BIN="${STDIO_BIN:-}" BRIDGE_PID=""

cleanup() {
  if [ "$KEEP" = "1" ]; then
    echo
    echo "KEEP=1: not cleaning up. Leftovers: service $SVC, link $LINKSVC, bridge on $BRIDGE_ADDR"
    return
  fi
  echo
  echo "cleaning up..."
  [ -n "$BRIDGE_PID" ] && kill "$BRIDGE_PID" >/dev/null 2>&1
  $KLING mcp unlink "$LINKSVC" >/dev/null 2>&1
  # Las máquinas que el gateway despertó de $SVC primero: rmi se niega a
  # borrar un snapshot dorado con instancias vivas.
  for m in $($KLING ps -a 2>/dev/null | awk -v n="$SVC" '$0 ~ n {print $1}'); do
    $KLING rm "$m" >/dev/null 2>&1
  done
  $KLING rmi "$SVC" >/dev/null 2>&1
  $KLING images rm "$SVC" >/dev/null 2>&1
  # Solo se borran si los compiló esta prueba: los que vienen de fuera son de
  # quien los pasó.
  if [ "${COMPILADOS:-0}" = "1" ]; then
    [ -n "${BRIDGE_BIN:-}" ] && rm -f "$BRIDGE_BIN"
    [ -n "${STDIO_BIN:-}" ] && rm -f "$STDIO_BIN"
  fi
}
trap cleanup EXIT

# httpCode hace una petición y deja el cuerpo en $1, devolviendo el código HTTP
# por stdout. Con archivo de cuerpo aparte porque -w y el cuerpo no se pueden
# leer los dos de la salida estándar sin arriesgarse a mezclarlos.
httpCode() {
  local bodyfile="$1"; shift
  curl -s -o "$bodyfile" -w '%{http_code}' "$@"
}

# initSession hace la petición initialize UNA sola vez y dispone tanto el
# cuerpo ($1) como las cabeceras ($2) en ficheros aparte, para poder sacar el
# código, el JSON y el Mcp-Session-Id de la MISMA respuesta. Repetirla —una
# vez por dato que hace falta— abriría una sesión de más por cada intento.
initSession() {
  local bodyfile="$1" headerfile="$2"; shift 2
  curl -s -D "$headerfile" -o "$bodyfile" -w '%{http_code}' \
    -X POST -H "Content-Type: application/json" -H "Accept: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}' \
    "$@"
}

# sessionOf saca el Mcp-Session-Id de un fichero de cabeceras de curl.
sessionOf() { grep -i '^mcp-session-id:' "$1" 2>/dev/null | tr -d '\r' | awk '{print $2}'; }

# rpcField extrae un campo del último JSON-RPC con python3, en vez de grep/sed:
# el cuerpo es JSON de verdad y un grep frágil rompería con espacios distintos.
rpcField() {
  python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
except Exception:
    print("")
    raise SystemExit(0)
node = doc
for key in sys.argv[1:]:
    if isinstance(node, dict):
        node = node.get(key)
    elif isinstance(node, list):
        try:
            node = node[int(key)]
        except (ValueError, IndexError):
            node = None
    else:
        node = None
    if node is None:
        break
print(node if node is not None else "")
' "$@"
}

# ── 1. la extensión está instalada ───────────────────────────────────────────
step "1. Extension"
plugins=$($KLING plugins 2>&1) || { echo "$plugins"; echo "could not reach kling"; exit 1; }
contiene "$plugins" "mcp" && ok "kling-mcp appears in kling plugins" \
  || bad "kling plugins" "mcp listed" "$plugins"

# ── 2. catálogo: kling add trae un servidor del registro ─────────────────────
step "2. Catalog: kling add"
out=$($KLING add io.github.domdomegg/filesystem-mcp -as "$SVC" -arg /tmp 2>&1)
if contiene "$out" "Done."; then
  ok "kling add packaged and imported $SVC"
else
  bad "kling add io.github.domdomegg/filesystem-mcp" "\"Done.\"" "$out"
fi

out=$($KLING mcp list 2>&1)
contiene "$out" "$SVC" && ok "the catalog kept it: kling mcp list shows $SVC" \
  || bad "kling mcp list" "a line for $SVC" "$out"

# ── 3. gateway con token: initialize + tools/list + tools/call ───────────────
step "3. Gateway with token"

# 3a. Por /mcp/<servicio> directamente.
body=$(mktemp)
hdr=$(mktemp)
code=$(initSession "$body" "$hdr" -H "Authorization: Bearer $TOKEN" "$GATEWAY/mcp/$SVC")
initBody=$(cat "$body")
sid=$(sessionOf "$hdr")
if [ "$code" = "200" ]; then
  ok "initialize on /mcp/$SVC answered 200"
else
  bad "initialize /mcp/$SVC" "HTTP 200" "HTTP $code: $initBody"
fi

if [ -n "$sid" ]; then
  ok "the bridge opened a session ($sid)"
else
  bad "session on initialize" "an Mcp-Session-Id header" "none"
fi

code=$(httpCode "$body" -X POST "$GATEWAY/mcp/$SVC" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json" \
  -H "Mcp-Session-Id: $sid" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')
toolsBody=$(cat "$body")
firstTool=$(rpcField result tools 0 name < <(echo "$toolsBody"))
if [ "$code" = "200" ] && [ -n "$firstTool" ]; then
  ok "tools/list on /mcp/$SVC: $firstTool (among others)"
else
  bad "tools/list /mcp/$SVC" "HTTP 200 with at least one tool" "HTTP $code: $toolsBody"
fi

if [ -n "$firstTool" ]; then
  code=$(httpCode "$body" -X POST "$GATEWAY/mcp/$SVC" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json" \
    -H "Mcp-Session-Id: $sid" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"$firstTool\",\"arguments\":{}}}")
  callBody=$(cat "$body")
  # No se comprueba el RESULTADO de la herramienta (depende del servidor de
  # terceros y de sus argumentos, que aquí no se conocen de antemano): solo que
  # el gateway la ejecutó y contestó, en vez de devolver un error de transporte.
  if [ "$code" = "200" ]; then
    ok "tools/call $firstTool on /mcp/$SVC answered 200"
  else
    bad "tools/call $firstTool /mcp/$SVC" "HTTP 200" "HTTP $code: $callBody"
  fi
fi

# 3b. Por /mcp/_all: las tres meta-herramientas del agregador.
code=$(initSession "$body" "$hdr" -H "Authorization: Bearer $TOKEN" "$GATEWAY/mcp/_all")
allInitBody=$(cat "$body")
allSid=$(sessionOf "$hdr")
if [ "$code" = "200" ] && [ -n "$allSid" ]; then
  ok "initialize on /mcp/_all answered 200 with a session"
else
  bad "initialize /mcp/_all" "HTTP 200 with a session" "HTTP $code: $allInitBody"
fi

code=$(httpCode "$body" -X POST "$GATEWAY/mcp/_all" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json" \
  -H "Mcp-Session-Id: $allSid" \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')
allToolsBody=$(cat "$body")
contiene "$allToolsBody" "find_tools" && ok "/mcp/_all tools/list offers the meta-tools (find_tools)" \
  || bad "tools/list /mcp/_all" "the find_tools meta-tool" "HTTP $code: $allToolsBody"

code=$(httpCode "$body" -X POST "$GATEWAY/mcp/_all" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json" \
  -H "Mcp-Session-Id: $allSid" \
  -d "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"call_tool\",\"arguments\":{\"name\":\"find_tools\",\"arguments\":{\"query\":\"file\",\"service\":\"$SVC\"}}}}")
findBody=$(cat "$body")
if [ "$code" = "200" ]; then
  ok "call_tool find_tools on /mcp/_all answered 200"
else
  bad "call_tool find_tools /mcp/_all" "HTTP 200" "HTTP $code: $findBody"
fi

# ── 4. sin token: 401 ─────────────────────────────────────────────────────────
step "4. No token"
code=$(httpCode "$body" -X POST "$GATEWAY/mcp/$SVC" \
  -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}')
[ "$code" = "401" ] && ok "without Authorization, the gateway answers 401" \
  || bad "no-token request" "HTTP 401" "HTTP $code: $(cat "$body")"
rm -f "$body" "$hdr"

# ── 5. health / heal salen con código 0 ──────────────────────────────────────
step "5. Health and heal"
out=$($KLING mcp health 2>&1); code=$?
[ "$code" = "0" ] && ok "kling mcp health exited 0" \
  || bad "kling mcp health" "exit code 0" "exit $code: $out"

out=$($KLING mcp heal 2>&1); code=$?
[ "$code" = "0" ] && ok "kling mcp heal exited 0" \
  || bad "kling mcp heal" "exit code 0" "exit $code: $out"

# ── 6. refresh-bridge es idempotente ─────────────────────────────────────────
step "6. refresh-bridge"
# El daemon se niega (409) a tocar una imagen que usa una microVM viva, y el
# gateway dejó instancias del servicio de las pruebas de arriba. Se retiran
# primero: el snapshot se queda, y la siguiente petición las recrea.
for m in $($KLING ps -a -json 2>/dev/null | python3 -c 'import json,sys; s=sys.argv[1]; [print(m["id"]) for m in json.load(sys.stdin) if (m.get("labels") or {}).get("service")==s]' "$SVC"); do
  $KLING rm "$m" >/dev/null 2>&1
done
out=$($KLING mcp refresh-bridge "$SVC" 2>&1)
first="$out"
out=$($KLING mcp refresh-bridge "$SVC" 2>&1)
contiene "$out" "already up to date" && ok "second refresh-bridge says 'already up to date'" \
  || bad "refresh-bridge (2nd run)" "\"already up to date\"" "$out (first run was: $first)"

# ── 7. link a un kling-bridge local, y unlink ────────────────────────────────
step "7. Link / unlink to a local server"
# Los binarios locales se pueden pasar ya compilados (BRIDGE_BIN, STDIO_BIN):
# el host del daemon no tiene por qué tener Go instalado. Sin ellos y sin Go,
# esta parte se salta en vez de abortar la prueba entera.
export GOWORK=off
if [ -n "${BRIDGE_BIN:-}" ] && [ -n "${STDIO_BIN:-}" ]; then
  ok "using the prebuilt kling-bridge and stdio server"
elif command -v go >/dev/null 2>&1; then
  BRIDGE_BIN=$(mktemp /tmp/e2e-kling-bridge.XXXXXX)
  STDIO_BIN=$(mktemp /tmp/e2e-stdio-server.XXXXXX)
  if go build -o "$STDIO_BIN" "$ROOT/examples/stdio-server" 2>/tmp/e2e-build.log \
     && go build -o "$BRIDGE_BIN" "$ROOT/cmd/kling-bridge" 2>>/tmp/e2e-build.log; then
    ok "built kling-bridge and the example stdio server"
    COMPILADOS=1
  else
    bad "go build kling-bridge/stdio-server" "both binaries" "$(cat /tmp/e2e-build.log)"
  fi
else
  BRIDGE_BIN=""; STDIO_BIN=""
fi

if [ -x "$BRIDGE_BIN" ] && [ -x "$STDIO_BIN" ]; then
  "$BRIDGE_BIN" -listen "$BRIDGE_ADDR" -- "$STDIO_BIN" >/tmp/e2e-bridge.log 2>&1 &
  BRIDGE_PID=$!
  # Sin sleep fijo a ciegas: se sondea el puerto en vez de adivinar cuánto
  # tarda en arrancar un binario recién compilado.
  ready=0
  for _ in $(seq 1 50); do
    curl -s -o /dev/null "http://$BRIDGE_ADDR/mcp" && { ready=1; break; }
    sleep 0.1
  done
  if [ "$ready" = "1" ] || kill -0 "$BRIDGE_PID" 2>/dev/null; then
    ok "local kling-bridge listening on $BRIDGE_ADDR"
  else
    bad "local kling-bridge" "listening on $BRIDGE_ADDR" "$(cat /tmp/e2e-bridge.log)"
  fi

  out=$($KLING mcp link "$LINKSVC" "http://$BRIDGE_ADDR/mcp" 2>&1)
  contiene "$out" "$LINKSVC" && ok "kling mcp link registered $LINKSVC" \
    || bad "kling mcp link" "confirmation naming $LINKSVC" "$out"

  out=$($KLING mcp list 2>&1)
  contiene "$out" "$LINKSVC" && ok "the link shows up in kling mcp list" \
    || bad "kling mcp list (after link)" "a line for $LINKSVC" "$out"

  # A través del gateway: /mcp/<link> se enruta al servidor externo, sin pasar
  # por ninguna microVM.
  body2=$(mktemp)
  code=$(httpCode "$body2" -X POST "$GATEWAY/mcp/$LINKSVC" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}')
  linkBody=$(cat "$body2")
  rm -f "$body2"
  [ "$code" = "200" ] && ok "the gateway routes /mcp/$LINKSVC to the linked server" \
    || bad "initialize /mcp/$LINKSVC" "HTTP 200" "HTTP $code: $linkBody"

  out=$($KLING mcp unlink "$LINKSVC" 2>&1)
  contiene "$out" "$LINKSVC" && ok "kling mcp unlink removed $LINKSVC" \
    || bad "kling mcp unlink" "confirmation naming $LINKSVC" "$out"

  out=$($KLING mcp list 2>&1)
  if contiene "$out" "$LINKSVC"; then
    bad "kling mcp list (after unlink)" "$LINKSVC gone" "still listed: $out"
  else
    ok "the link is gone from kling mcp list"
  fi

  kill "$BRIDGE_PID" >/dev/null 2>&1
  BRIDGE_PID=""
else
  echo "  (skipping link/unlink: no Go here and no BRIDGE_BIN/STDIO_BIN given)"
fi

# ── resumen ──────────────────────────────────────────────────────────────────
printf "\n\033[1m%d ok · %d fail(s)\033[0m\n" "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
