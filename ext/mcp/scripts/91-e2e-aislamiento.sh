#!/usr/bin/env bash
# Prueba de extremo a extremo del aislamiento por sesión (mcp.isolation=session)
# contra un daemon y un gateway REALES, en el host del daemon.
#
# Lo que los tests de Go no pueden ver: que la sesión B de verdad no lee el
# fichero que la sesión A escribió en /tmp de su microVM, que A lo sigue viendo
# tras un freeze/thaw de su máquina, y que al cerrar A su máquina —y con ella su
# overlay— desaparece del disco del host. Mide además lo que cuesta.
#
#   ./scripts/91-e2e-aislamiento.sh                 gateway en 127.0.0.1:8080
#   GATEWAY=http://127.0.0.1:18180 ./91-...sh       otro gateway
#   KLING_MCP=~/kcow/kling-mcp ./91-...sh           el binario que trae
#                                                   `isolation` (por defecto,
#                                                   `kling mcp`)
#   SVC=cowfs ./91-...sh                            un servicio ya importado de
#                                                   io.github.domdomegg/filesystem-mcp
#                                                   con /tmp; sin SVC se importa
#                                                   uno y se borra al final
#   EXPIRE=150 ./91-...sh                           espera 150 s sin tocar la
#                                                   sesión B y comprueba que su
#                                                   máquina se destruyó (pon algo
#                                                   mayor que el -session-ttl
#                                                   del gateway más idle/3)
#   N=5 ./91-...sh                                  sesiones para medir
#   IDLE_WAIT=45 ./91-...sh                         además del `kling freeze`,
#                                                   espera a que el segador del
#                                                   gateway congele la sesión A
#                                                   (algo mayor que su -idle más
#                                                   idle/3) y comprueba el thaw
#
# Necesita sudo para leer $KLING_ROOT/machines (lo que ocupa cada sesión) y el
# token del gateway. Cada comprobación dice qué esperaba y qué obtuvo; un fallo
# no aborta el resto.
set -uo pipefail

KLING="${KLING:-kling}"
KLING_MCP="${KLING_MCP:-$KLING mcp}"
GATEWAY="${GATEWAY:-http://127.0.0.1:8080}"
KROOT="${KLING_ROOT:-/var/lib/kindling}"
N="${N:-5}"
EXPIRE="${EXPIRE:-0}"
IDLE_WAIT="${IDLE_WAIT:-0}"
KEEP="${KEEP:-0}"
IMPORTADO=0
if [ -z "${SVC:-}" ]; then
  SVC="e2e-cow-$$"
  IMPORTADO=1
fi

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFAIL\033[0m  %s\n     expected: %s\n     got:      %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }
note() { printf "        %s\n" "$1"; }
# Sin tuberías con grep -q: con pipefail, el SIGPIPE del productor da por
# fallida una tubería que encontró el texto (ver 90-e2e.sh).
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }

need() { command -v "$1" >/dev/null || { echo "missing $1" >&2; exit 1; }; }
need "$KLING"; need curl; need python3

if [ -n "${KLING_GATEWAY_TOKEN:-}" ]; then
  TOKEN="$KLING_GATEWAY_TOKEN"
else
  TOKEN=$(sudo cut -d= -f2 /etc/kling/gateway.env 2>/dev/null | tr -d '\r\n')
fi
[ -n "$TOKEN" ] || { echo "no gateway token: set KLING_GATEWAY_TOKEN or make /etc/kling/gateway.env readable" >&2; exit 1; }

TMP=$(mktemp -d)
SESIONES=()
cleanup() {
  for s in "${SESIONES[@]:-}"; do
    [ -n "$s" ] && curl -s -o /dev/null -X DELETE -H "Authorization: Bearer $TOKEN" -H "Mcp-Session-Id: $s" "$GATEWAY/mcp/$SVC"
  done
  rm -rf "$TMP"
  if [ "$KEEP" = "1" ] || [ "$IMPORTADO" = "0" ]; then
    $KLING_MCP isolation "$SVC" service >/dev/null 2>&1
    return
  fi
  for m in $($KLING ps -a 2>/dev/null | awk -v n="$SVC" '$0 ~ n {print $1}'); do
    $KLING rm "$m" >/dev/null 2>&1
  done
  $KLING template rm "$SVC" >/dev/null 2>&1
  $KLING image rm -f "$SVC" >/dev/null 2>&1
}
trap cleanup EXIT

# ── utilidades MCP ────────────────────────────────────────────────────────────

# abrir hace un initialize y deja en $SID la sesión y en $T_INIT los segundos.
abrir() {
  local hdr="$TMP/h" body="$TMP/b"
  T_INIT=$(curl -s -D "$hdr" -o "$body" -w '%{time_total}' -X POST "$GATEWAY/mcp/$SVC" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json" \
    -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e-cow","version":"1"}}}')
  SID=$(grep -i '^mcp-session-id:' "$hdr" | tr -d '\r' | awk '{print $2}')
  [ -n "$SID" ] && SESIONES+=("$SID")
  # El cliente confirma: sin esto algunos servidores no aceptan tools/call.
  [ -n "$SID" ] && curl -s -o /dev/null -X POST "$GATEWAY/mcp/$SVC" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Mcp-Session-Id: $SID" \
    -d '{"jsonrpc":"2.0","method":"notifications/initialized"}'
  return 0
}

# llamar <sid> <herramienta> <json args>: deja la respuesta en $RESP y el
# código HTTP en $CODE.
llamar() {
  CODE=$(curl -s -o "$TMP/r" -w '%{http_code}' -X POST "$GATEWAY/mcp/$SVC" \
    -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" -H "Accept: application/json" \
    -H "Mcp-Session-Id: $1" \
    -d "{\"jsonrpc\":\"2.0\",\"id\":$RANDOM,\"method\":\"tools/call\",\"params\":{\"name\":\"$2\",\"arguments\":$3}}")
  RESP=$(cat "$TMP/r")
}

cerrar() {
  curl -s -o /dev/null -w '%{http_code}' -X DELETE "$GATEWAY/mcp/$SVC" \
    -H "Authorization: Bearer $TOKEN" -H "Mcp-Session-Id: $1"
}

# aisladas lista, ordenados, los ids de las máquinas aisladas del servicio.
aisladas() {
  $KLING ps -a -json 2>/dev/null | python3 -c '
import json, sys
svc = sys.argv[1]
try:
    ms = json.load(sys.stdin)
except Exception:
    ms = []
for m in ms:
    l = m.get("labels") or {}
    if l.get("isolated") == "true" and l.get("service") == svc:
        print(m["id"], m.get("state", ""))
' "$SVC" | sort
}

estado() { aisladas | awk -v id="$1" '$1 == id {print $2}'; }

# kib dice lo que ocupa en disco el directorio de una máquina, en KiB.
kib() { sudo du -sk "$KROOT/machines/$1" 2>/dev/null | awk '{print $1}'; }
mib() { awk -v k="$1" 'BEGIN {printf "%.1f", k / 1024}'; }

memAvailKiB() { awk '/MemAvailable/ {print $2}' /proc/meminfo; }

mediana() { python3 -c 'import statistics,sys; v=[float(x) for x in sys.argv[1:]]; print(f"{statistics.median(v)*1000:.0f}")' "$@"; }

# ── 0. servicio ───────────────────────────────────────────────────────────────
step "0. Service $SVC"
if [ "$IMPORTADO" = "1" ]; then
  out=$($KLING_MCP add io.github.domdomegg/filesystem-mcp -as "$SVC" -arg /tmp 2>&1)
  contiene "$out" "Done." && ok "imported filesystem-mcp as $SVC" \
    || { bad "kling mcp add" "Done." "$out"; exit 1; }
fi
FICHERO="/tmp/cow-$$-$RANDOM.txt"
SECRETO="secreto-de-A-$RANDOM"

# ── 1. el hueco: en modo service, B lee lo que A escribió ────────────────────
# Sin esta línea base, que B no vea el fichero en modo session no probaría
# nada: podría no verlo por cualquier otro motivo.
#
# A cierra ANTES de que B abra, que es la pregunta del hilo ("¿la siguiente
# sesión ve los residuos de la anterior?") y lo que asegura que las dos caen
# en la misma instancia: con 256 MiB cabe una sesión por instancia, y dos
# simultáneas acabarían en réplicas distintas.
step "1. Baseline, isolation=service: sessions share the disk"
$KLING_MCP isolation "$SVC" service >/dev/null
sleep 16 # el gateway relee mcp.isolation cada 15 s
abrir; A0=$SID
llamar "$A0" create "{\"path\":\"$FICHERO\",\"content\":\"$SECRETO\"}"
cerrar "$A0" >/dev/null
abrir; B0=$SID
llamar "$B0" view "{\"path\":\"$FICHERO\"}"
if contiene "$RESP" "$SECRETO"; then
  ok "service: the next session B reads the file session A left behind (the leak this fixes)"
else
  bad "service mode baseline" "B sees $SECRETO" "$RESP"
fi
cerrar "$B0" >/dev/null
# Los tiempos del initialize con la instancia ya despierta: es lo que cuesta
# una sesión nueva hoy (un proceso del servidor nuevo dentro de la instancia).
T_SERVICE=()
for i in $(seq 1 "$N"); do abrir; T_SERVICE+=("$T_INIT"); cerrar "$SID" >/dev/null; done
# La primaria compartida se retira para que el modo session empiece limpio.
for m in $($KLING ps 2>/dev/null | awk -v n="$SVC" '$0 ~ n {print $1}'); do $KLING rm "$m" >/dev/null 2>&1; done

# ── 2. modo session: B no ve lo de A ─────────────────────────────────────────
step "2. isolation=session: one microVM per session"
$KLING_MCP isolation "$SVC" session >/dev/null
sleep 16
antes=$(aisladas | wc -l)
memAntes=$(memAvailKiB)
abrir; A=$SID; T_A=$T_INIT
abrir; B=$SID
if [ -n "$A" ] && [ -n "$B" ] && [ "$A" != "$B" ]; then
  ok "two sessions opened (A=${A:0:8} B=${B:0:8})"
else
  bad "initialize in session mode" "two distinct sessions" "A=$A B=$B"
fi
n=$(( $(aisladas | wc -l) - antes ))
[ "$n" = "2" ] && ok "each session has its own isolated machine (2 new)" \
  || bad "isolated machines" "2 new" "$n: $(aisladas)"

llamar "$A" create "{\"path\":\"$FICHERO\",\"content\":\"$SECRETO\"}"
[ "$CODE" = "200" ] && ! contiene "$RESP" '"isError":true' && ok "A wrote $FICHERO" \
  || bad "create in A" "HTTP 200 without isError" "HTTP $CODE: $RESP"
llamar "$B" view "{\"path\":\"$FICHERO\"}"
if contiene "$RESP" "$SECRETO"; then
  bad "session B" "does NOT see A's file" "$RESP"
else
  ok "B does not see A's file ($(printf %s "$RESP" | head -c 90)...)"
fi
llamar "$A" view "{\"path\":\"$FICHERO\"}"
contiene "$RESP" "$SECRETO" && ok "A sees its own file" || bad "A reads its file" "$SECRETO" "$RESP"

# ── 3. freeze/thaw: A vuelve a SU máquina con su fichero ─────────────────────
step "3. Freeze and thaw"
mapfile -t ids < <(aisladas | awk '{print $1}')
RUN_KIB=$(kib "${ids[0]}")
for id in "${ids[@]}"; do $KLING freeze "$id" >/dev/null 2>&1; done
congeladas=0
for id in "${ids[@]}"; do
  case "$(estado "$id")" in warm|frozen) congeladas=$((congeladas+1));; esac
done
[ "$congeladas" = "${#ids[@]}" ] && ok "both machines frozen (${#ids[@]})" \
  || bad "kling freeze" "${#ids[@]} warm" "$congeladas: $(aisladas)"
FROZEN_KIB=$(kib "${ids[0]}")
llamar "$A" view "{\"path\":\"$FICHERO\"}"
contiene "$RESP" "$SECRETO" && ok "after thaw, A still sees its file (same session, same machine)" \
  || bad "A after thaw" "$SECRETO" "HTTP $CODE: $RESP"
llamar "$B" view "{\"path\":\"$FICHERO\"}"
contiene "$RESP" "$SECRETO" && bad "B after thaw" "does NOT see A's file" "$RESP" \
  || ok "after thaw, B still does not see it"
n=$(( $(aisladas | wc -l) - antes ))
[ "$n" = "2" ] && ok "thaw reused the same machines (still 2)" \
  || bad "machines after thaw" "2" "$n: $(aisladas)"

# El congelado de verdad es el del segador por inactividad, no un freeze a
# mano: ese es el que antes se llevaba también la ruta de la sesión.
if [ "$IDLE_WAIT" -gt 0 ]; then
  sleep "$IDLE_WAIT"
  dormidas=0
  for id in "${ids[@]}"; do
    case "$(estado "$id")" in warm|frozen|paused) dormidas=$((dormidas+1));; esac
  done
  [ "$dormidas" = "${#ids[@]}" ] && ok "the gateway's reaper put both to sleep after ${IDLE_WAIT}s idle" \
    || bad "reaper" "${#ids[@]} asleep" "$dormidas: $(aisladas)"
  llamar "$A" view "{\"path\":\"$FICHERO\"}"
  contiene "$RESP" "$SECRETO" && ok "after the reaper, A still sees its file" \
    || bad "A after the reaper" "$SECRETO" "HTTP $CODE: $RESP"
fi

# ── 4. cerrar A libera su capa en el host ────────────────────────────────────
step "4. Closing A frees its layer on the host"
antesIds=$(aisladas | awk '{print $1}')
code=$(cerrar "$A")
[ "$code" = "204" ] || [ "$code" = "200" ] && ok "DELETE of A answered $code" || bad "DELETE A" "204" "$code"
despuesIds=$(aisladas | awk '{print $1}')
idA=$(comm -23 <(echo "$antesIds") <(echo "$despuesIds"))
if [ -n "$idA" ] && [ "$(echo "$idA" | wc -l)" = "1" ]; then
  ok "A's machine ${idA:0:8} is gone from kling ps"
  if sudo test -e "$KROOT/machines/$idA"; then
    bad "A's machine directory" "removed" "$KROOT/machines/$idA still exists"
  else
    ok "its directory (overlay.ext4, memory dump) is gone from $KROOT/machines"
  fi
else
  bad "machine of A after DELETE" "exactly one gone" "before: $antesIds / after: $despuesIds"
fi
llamar "$A" view "{\"path\":\"$FICHERO\"}"
[ "$CODE" = "404" ] && ok "the closed session answers 404" || bad "closed session" "404" "HTTP $CODE"
llamar "$B" view "{\"path\":\"/tmp\"}"
[ "$CODE" = "200" ] && ok "B keeps working" || bad "B after closing A" "HTTP 200" "HTTP $CODE: $RESP"
# Y la sesión SIGUIENTE tampoco hereda nada: es el caso de la línea base.
abrir; C=$SID
llamar "$C" view "{\"path\":\"$FICHERO\"}"
contiene "$RESP" "$SECRETO" && bad "next session C" "does NOT see A's file" "$RESP" \
  || ok "the next session C does not see what A left behind"
cerrar "$C" >/dev/null

# ── 5. coste ──────────────────────────────────────────────────────────────────
step "5. Cost of a session"
memAntes=$(memAvailKiB)
antesN=$(aisladas | wc -l)
T_SESSION=("$T_A")
EXTRA=()
for i in $(seq 1 "$N"); do
  abrir; T_SESSION+=("$T_INIT"); EXTRA+=("$SID")
  llamar "$SID" view "{\"path\":\"/tmp\"}"
done
sleep 2
memDespues=$(memAvailKiB)
nuevas=$(( $(aisladas | wc -l) - antesN ))
[ "$nuevas" = "$N" ] && ok "$N more sessions, $N more machines" || bad "cost sessions" "$N machines" "$nuevas"
note "RAM per awake session (MemAvailable delta / $N): $(mib $(( (memAntes - memDespues) / N ))) MiB"
note "disk per session: running $(mib "$RUN_KIB") MiB, frozen $(mib "$FROZEN_KIB") MiB (du of $KROOT/machines/<id>)"
note "first initialize, service mode (instance awake): median $(mediana "${T_SERVICE[@]}") ms over $N"
note "first initialize, session mode (new microVM):    median $(mediana "${T_SESSION[@]}") ms over ${#T_SESSION[@]}"
for s in "${EXTRA[@]}"; do cerrar "$s" >/dev/null; done

# ── 6. caducidad: B sin tocar se destruye sola ───────────────────────────────
if [ "$EXPIRE" -gt 0 ]; then
  step "6. Expiry: B untouched for ${EXPIRE}s"
  idB=$(aisladas | awk '{print $1}' | head -1)
  sleep "$EXPIRE"
  if [ -z "$(estado "$idB")" ] && ! sudo test -e "$KROOT/machines/$idB"; then
    ok "B's machine ${idB:0:8} was destroyed by -session-ttl"
  else
    bad "expiry of B" "machine gone" "state $(estado "$idB")"
  fi
  llamar "$B" view "{\"path\":\"/tmp\"}"
  [ "$CODE" = "404" ] && ok "the expired session answers 404" || bad "expired session" "404" "HTTP $CODE"
fi

echo
printf "%d ok, %d failed\n" "$pass" "$fail"
[ "$fail" = "0" ]
