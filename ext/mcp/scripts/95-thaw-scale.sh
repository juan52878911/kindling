#!/usr/bin/env bash
# Despertar a escala: M sesiones MCP a la vez contra N servicios congelados
# (o calientes), a través de un gateway real y un daemon real con KVM. El
# método, las reglas de honestidad y cómo leer la tabla están en
# docs/thaw-at-scale.md; esto es solo quien monta y recorre la matriz.
#
# Corre EN el host Linux del daemon, como un usuario que puede usar `kling` y
# sudo sin contraseña (sudo solo para construir la imagen y leer el PSI del
# daemon). El host no necesita Go: los binarios se compilan en otro sitio y se
# copian al lado de este script (ver docs/thaw-at-scale.md, «Reproducir»).
#
#   ./95-thaw-scale.sh                       la matriz por defecto
#   NS="1 10" MS="1 10" R=1 ./95-thaw-scale.sh    una pasada corta
#   MODES=frozen REPS=rep ./95-thaw-scale.sh      solo una variante
#
# Qué hace, en orden:
#   1. Preflight: KVM, PSI < 5, RAM y disco suficientes, ningún tb- ajeno.
#   2. Imagen tb-echo (el servidor de ejemplo examples/mcp/stdio-server,
#      estático, detrás de kling-bridge). Idempotente: si ya está, no se toca.
#   3. Servicios tb-0..N-1 (norep, memoria MEM_NOREP) y tb-r0..N-1 (rep,
#      MEM_REP: una sesión por instancia, así que cada sesión de más es una
#      réplica). También idempotentes.
#   4. Un gateway PROPIO en 127.0.0.1:$PORT, con un token de un solo uso por
#      variable de entorno (nunca por argv) y -max-replicas $MAXREP.
#   5. Para cada celda (variante × N × M × modo) R repeticiones:
#        frozen: una ronda de cebado, espera a que el gateway congele todo,
#                ronda medida (con -settle: tiempo hasta cero).
#        warm:   ronda de cebado y, sin esperar, ronda medida.
#      Entre celdas: cero microVMs tb- vivas y PSI < 5.
#      Una celda cuya estimación pase del 70 % de la RAM disponible (o del
#      disco) NO se corre: queda en la tabla como "skipped", con el motivo.
#   6. Limpieza (trap) y comprobación contra la línea base.
#
# El prefijo tb- es de esta prueba: al salir se borran TODAS las máquinas y
# plantillas tb- y la imagen tb-echo (salvo KEEP=1).
#
# Honestidad: toda ronda medida va a la tabla, también las que fallan. Más de
# un 1 % de sesiones fallidas marca la ronda DEGRADED (lo decide el propio
# kling-mcpbench). Nada se descarta ni se repite hasta que salga bonito.
#
# Env:
#   KLING (kling), KLING_MCP (kling-mcp; el de la rama, con -max-replicas),
#   BENCH (./kling-mcpbench), BRIDGE (./kling-bridge, ELF para el invitado),
#   STDIO (./stdio-server, ELF estático), SOCK (/run/kling.sock),
#   KLING_ROOT (/var/lib/kindling), BASE_IMAGE (min),
#   NS ("1 10 50"), MS ("1 10 50 200"), MODES ("frozen warm"), REPS ("norep rep"),
#   R (3), CALLS (5), TIMEOUT (60s), IDLE (10s, -idle del gateway),
#   SETTLE (120s), MAXREP (16), MEM_NOREP (1024M), MEM_REP (256M), PSI_MAX (5,
#   umbral de PSI some avg10 para empezar y entre celdas; queda en el resultado),
#   IMG_SCRIPT (80-mcp-image.sh al lado de este script),
#   PORT (18180), OUT (docs/bench-data/thaw-scale-<fecha> en el repo, o al lado
#   del script si se copió suelto), KEEP=1 (no limpia),
#   FORCE=1 (sigue aunque haya tb- de antes: los considera suyos y los borra).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"

KLING="${KLING:-kling}"
KLING_MCP="${KLING_MCP:-kling-mcp}"
BENCH="${BENCH:-$HERE/kling-mcpbench}"
BRIDGE="${BRIDGE:-$HERE/kling-bridge}"
STDIO="${STDIO:-$HERE/stdio-server}"
SOCK="${SOCK:-/run/kling.sock}"
ROOT="${KLING_ROOT:-/var/lib/kindling}"
NS="${NS:-1 10 50}"
MS="${MS:-1 10 50 200}"
MODES="${MODES:-frozen warm}"
REPS="${REPS:-norep rep}"
R="${R:-3}"
CALLS="${CALLS:-5}"
TIMEOUT="${TIMEOUT:-60s}"
IDLE="${IDLE:-10s}"
SETTLE="${SETTLE:-120s}"
MAXREP="${MAXREP:-16}"
PSI_MAX="${PSI_MAX:-5}"
MEM_NOREP="${MEM_NOREP:-1024M}"
MEM_REP="${MEM_REP:-256M}"
PORT="${PORT:-18180}"
KEEP="${KEEP:-0}"
FORCE="${FORCE:-0}"
IMG=tb-echo
STAMP="$(date -u +%Y%m%d-%H%M%S)"
# En el repo, los datos van a docs/bench-data; copiado suelto al lab, al lado.
if [ -d "$REPO/docs/bench-data" ]; then
  OUT="${OUT:-$REPO/docs/bench-data/thaw-scale-$STAMP}"
else
  OUT="${OUT:-$HERE/thaw-scale-$STAMP}"
fi
IMG_SCRIPT="${IMG_SCRIPT:-$HERE/80-mcp-image.sh}"
GW="http://127.0.0.1:$PORT"
WORK="$(mktemp -d /tmp/thaw-scale.XXXXXX)"
GWPID=""

log()  { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die()  { echo "ERROR: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing $1"; }

# mib convierte 256M / 1G / 512 (MiB) a MiB, como units.MiBVar del CLI.
mib() {
  case "$1" in
    *G|*g) echo $(( ${1%[Gg]} * 1024 )) ;;
    *M|*m) echo "${1%[Mm]}" ;;
    *)     echo "$1" ;;
  esac
}

# per_instance replica gwMaxSessions (pkg/scheduler): 64 MiB por sesión, 192
# reservados, mínimo 1, tope 32. Solo para ESTIMAR cuántas máquinas pedirá una
# celda; lo que de verdad pasó lo mide kling-mcpbench.
per_instance() {
  local m=$1 n
  [ "$m" -le 0 ] && { echo 1; return; }
  n=$(( (m - 192) / 64 ))
  [ "$n" -lt 1 ] && n=1
  [ "$n" -gt 32 ] && n=32
  echo "$n"
}

psi_some() { awk '/^some/ { for (i = 2; i <= NF; i++) if ($i ~ /^avg10=/) { sub("avg10=", "", $i); print $i } }' /proc/pressure/memory 2>/dev/null || echo -1; }
avail_mib() { awk '/^MemAvailable:/ { print int($2 / 1024) }' /proc/meminfo; }
disk_free_mib() { df -Pm "$ROOT" | awk 'NR == 2 { print $4 }'; }

# tb_machines lista "<id> <estado>" de las máquinas de los servicios de esta
# prueba: nombre tb-, o etiqueta service / from tb-.
tb_machines() {
  "$KLING" ps -a -json 2>/dev/null | python3 -c '
import json, sys
try:
    ms = json.load(sys.stdin) or []
except Exception:
    ms = []
for m in ms:
    svc = (m.get("labels") or {}).get("service", "")
    if any(x.startswith("tb-") for x in (m.get("name", ""), svc, m.get("from", ""))):
        print(m["id"], m.get("state", ""))
'
}
live_tb() { tb_machines | awk '$2 == "running" || $2 == "paused" || $2 == "starting" { n++ } END { print n + 0 }'; }

# wait_calm espera cero microVMs tb- vivas y PSI < 5, hasta $1 segundos.
wait_calm() {
  local limit=$1 t=0 live psi
  while :; do
    live=$(live_tb); psi=$(psi_some)
    if [ "$live" = "0" ] && awk -v p="$psi" -v m="$PSI_MAX" 'BEGIN { exit !(p < m) }'; then
      return 0
    fi
    if [ "$t" -ge "$limit" ]; then
      log "not calm after ${limit}s: $live live tb- machine(s), PSI some avg10 $psi"
      return 1
    fi
    sleep 2; t=$((t + 2))
  done
}

cleanup() {
  local rc=$?
  [ -n "$GWPID" ] && kill "$GWPID" 2>/dev/null && wait "$GWPID" 2>/dev/null
  if [ "$KEEP" = "1" ]; then
    echo; echo "KEEP=1: not cleaning up. Left: tb- services and machines, image $IMG, work dir $WORK"
    exit $rc
  fi
  echo; log "cleaning up..."
  for id in $(tb_machines | awk '{ print $1 }'); do "$KLING" rm "$id" >/dev/null 2>&1; done
  for s in $("$KLING" template ls -q 2>/dev/null | grep '^tb-'); do "$KLING" template rm -f "$s" >/dev/null 2>&1; done
  "$KLING" image rm -f "$IMG" >/dev/null 2>&1
  rm -rf "$WORK"

  # Línea base: ni una máquina tb- ni una plantilla tb- de sobra, y la memoria
  # disponible de vuelta cerca de donde estaba (±10 %: la caché de páginas se
  # mueve sola; esto busca fugas de gigas, no de megas).
  local left snaps now
  left=$(tb_machines | wc -l | tr -d ' ')
  snaps=$("$KLING" template ls -q 2>/dev/null | grep -c '^tb-')
  now=$(avail_mib)
  [ "$left" = "0" ] || { echo "BASELINE: $left tb- machine(s) left behind"; rc=1; }
  [ "$snaps" = "0" ] || { echo "BASELINE: $snaps tb- template(s) left behind"; rc=1; }
  if [ -n "${BASE_AVAIL:-}" ] && [ "$now" -lt $(( BASE_AVAIL * 9 / 10 )) ]; then
    echo "BASELINE: MemAvailable $now MiB, was $BASE_AVAIL MiB before the run"; rc=1
  fi
  [ "$rc" = "0" ] && log "baseline ok: no tb- leftovers, MemAvailable $now MiB (was ${BASE_AVAIL:-?})"
  exit $rc
}
trap cleanup EXIT
trap 'exit 130' INT TERM

# ── 1. preflight ──────────────────────────────────────────────────────────────
need "$KLING"; need python3; need sudo; need curl
[ -x "$BENCH" ]  || die "no $BENCH: cross-compile ext/mcp/cmd/kling-mcpbench and copy it here (docs/thaw-at-scale.md)"
command -v "$KLING_MCP" >/dev/null 2>&1 || [ -x "$KLING_MCP" ] || die "no $KLING_MCP"
"$KLING_MCP" serve -h 2>&1 | grep -q -- '-max-replicas' \
  || die "$KLING_MCP has no -max-replicas: copy the kling-mcp built from this branch and set KLING_MCP"
[ -c /dev/kvm ] || die "no /dev/kvm"
[ -r /proc/pressure/memory ] || die "no PSI (/proc/pressure/memory): the bench needs it"
sudo -n true 2>/dev/null || die "needs passwordless sudo (image build, daemon environment)"

if [ "$FORCE" != "1" ] && [ -n "$(tb_machines)" ]; then
  die "there are tb- machines that this run did not create (see kling ps -a); remove them or FORCE=1"
fi

psi=$(psi_some)
awk -v p="$psi" -v m="$PSI_MAX" 'BEGIN { exit !(p < m) }' || die "host under memory pressure before starting (PSI some avg10 $psi >= $PSI_MAX; set PSI_MAX to change it)"

NMAX=0; for n in $NS; do [ "$n" -gt "$NMAX" ] && NMAX=$n; done
MNR=$(mib "$MEM_NOREP"); MR=$(mib "$MEM_REP")
BASE_AVAIL=$(avail_mib)
need_disk=3072
for rep in $REPS; do
  case "$rep" in
    norep) need_disk=$(( need_disk + NMAX * MNR )) ;;
    rep)   need_disk=$(( need_disk + NMAX * MR )) ;;
    *) die "unknown REPS entry $rep (norep|rep)" ;;
  esac
done
free_disk=$(disk_free_mib)
[ "$free_disk" -ge "$need_disk" ] || die "disk: $free_disk MiB free under $ROOT, need $need_disk (N×mem of the goldens + 3 GiB)"
[ "$BASE_AVAIL" -ge 2048 ] || die "only $BASE_AVAIL MiB available"

# El KLING_MAX_MEM_PRESSURE del daemon, para la cabecera del informe.
PSI_LIMIT=$(systemctl show -p Environment kling 2>/dev/null | tr ' ' '\n' | sed -n 's/^KLING_MAX_MEM_PRESSURE=//p')
[ -f /etc/default/kling ] && [ -z "$PSI_LIMIT" ] && PSI_LIMIT=$(sudo sed -n 's/^KLING_MAX_MEM_PRESSURE=//p' /etc/default/kling)
PSI_LIMIT="${PSI_LIMIT:-20 (daemon default)}"

mkdir -p "$OUT" || die "can't create $OUT"
log "output: $OUT"
log "host: $(nproc) CPU, MemAvailable $BASE_AVAIL MiB, disk free $free_disk MiB, PSI $psi, PSI limit $PSI_LIMIT"

# ── 2. imagen ─────────────────────────────────────────────────────────────────
if sudo test -f "$ROOT/images/$IMG.ext4"; then
  log "image $IMG already built"
else
  [ -f "$BRIDGE" ] || die "no $BRIDGE (make bridge in ext/mcp, or cross-compile cmd/kling-bridge)"
  [ -f "$STDIO" ]  || die "no $STDIO (CGO_ENABLED=0 go build ./examples/mcp/stdio-server)"
  [ -f "$IMG_SCRIPT" ] || die "no $IMG_SCRIPT (copy ext/mcp/scripts/80-mcp-image.sh next to this script)"
  mkdir -p "$WORK/img/opt/mcp"
  install -m755 "$STDIO" "$WORK/img/opt/mcp/server"
  log "building image $IMG"
  # El log es del usuario a propósito: la redirección la abre este shell, no sudo.
  # shellcheck disable=SC2024
  sudo env KLING_ROOT="$ROOT" BRIDGE="$BRIDGE" BASE_IMAGE="${BASE_IMAGE:-min}" \
    bash "$IMG_SCRIPT" stdio "$IMG" -d "$WORK/img" -- /opt/mcp/server >"$WORK/image.log" 2>&1 \
    || { cat "$WORK/image.log"; die "image build failed"; }
fi

# ── 3. servicios ──────────────────────────────────────────────────────────────
have_tpl() { "$KLING" template ls -q 2>/dev/null | grep -qx "$1"; }
svc_prefix() { [ "$1" = rep ] && echo tb-r || echo tb-; }
svc_mem()    { [ "$1" = rep ] && echo "$MEM_REP" || echo "$MEM_NOREP"; }
for rep in $REPS; do
  pre=$(svc_prefix "$rep"); mem=$(svc_mem "$rep")
  for ((i = 0; i < NMAX; i++)); do
    have_tpl "$pre$i" && continue
    log "importing $pre$i (mem $mem)"
    "$KLING_MCP" import "$pre$i" -image "$IMG" -mem "$mem" -egress none >"$WORK/import.log" 2>&1 \
      || { cat "$WORK/import.log"; die "import of $pre$i failed"; }
  done
done
# La importación deja sus máquinas temporales retiradas; lo que quede vivo
# (nada, en un host sano) no debe contar en la primera celda.
wait_calm 120 || die "tb- machines still alive after importing"

# ── 4. gateway propio ─────────────────────────────────────────────────────────
KLING_GATEWAY_TOKEN="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
export KLING_GATEWAY_TOKEN
# -hosts fija el daemon: sin él, el gateway usaría mcp.hosts de la
# configuración del usuario, que puede apuntar a otro sitio.
"$KLING_MCP" serve -listen "127.0.0.1:$PORT" -idle "$IDLE" -max-replicas "$MAXREP" \
  -hosts "bench=unix://$SOCK" >"$OUT/gateway.log" 2>&1 &
GWPID=$!
for _ in $(seq 1 100); do
  curl -sf -o /dev/null "$GW/healthz" && break
  kill -0 "$GWPID" 2>/dev/null || { cat "$OUT/gateway.log"; die "gateway died"; }
  sleep 0.1
done
curl -sf -o /dev/null "$GW/healthz" || die "gateway not answering on $GW"
log "gateway on $GW (idle $IDLE, max-replicas $MAXREP)"

# ── 5. matriz ─────────────────────────────────────────────────────────────────
RESULTS="$OUT/results.md"
{
  echo "# Thaw at scale — $STAMP"
  echo
  echo "NS=\"$NS\" MS=\"$MS\" MODES=\"$MODES\" REPS=\"$REPS\" R=$R CALLS=$CALLS TIMEOUT=$TIMEOUT"
  echo "IDLE=$IDLE SETTLE=$SETTLE MAXREP=$MAXREP PSI_MAX=$PSI_MAX MEM_NOREP=$MEM_NOREP MEM_REP=$MEM_REP"
  echo
  "$BENCH" -md-header
} >"$RESULTS"

bench() {
  "$BENCH" -gateway "$GW" -sock "$SOCK" -timeout "$TIMEOUT" -calls "$CALLS" \
    -psi-limit "$PSI_LIMIT" -max-sessions 1000 "$@"
}

runs=0; degraded=0; skipped=0; failed=0
for rep in $REPS; do
  pre=$(svc_prefix "$rep"); memm=$(mib "$(svc_mem "$rep")"); per=$(per_instance "$memm")
  for n in $NS; do
    for m in $MS; do
      # Estimación: sesiones por servicio (reparto rotatorio), instancias que
      # pedirán (con el tope de réplicas), y su memoria configurada, que es lo
      # que reserva el daemon al admitir.
      machines=0; capped=0
      for ((i = 0; i < n && i < m; i++)); do
        spp=$(( m / n + (i < m % n ? 1 : 0) ))
        k=$(( (spp + per - 1) / per ))
        if [ "$MAXREP" -gt 0 ] && [ "$k" -gt "$MAXREP" ]; then k=$MAXREP; capped=1; fi
        machines=$(( machines + k ))
      done
      est=$(( machines * memm ))
      avail=$(avail_mib); dfree=$(disk_free_mib)
      for mode in $MODES; do
        cell="$rep-$mode-n$n-m$m"
        note=""; [ "$capped" = 1 ] && note=" (capped at $MAXREP replicas: expect 503s)"
        if [ "$est" -gt $(( avail * 70 / 100 )) ] || [ "$(( est + 3072 ))" -gt "$dfree" ]; then
          reason="estimated $machines machine(s) × $memm MiB = $est MiB; available $avail MiB (70% = $(( avail * 70 / 100 ))), disk free $dfree MiB"
          log "SKIP $cell: $reason"
          printf '| %s | %d | %d | skipped | | | | | | | | %d (est) | | %d | | | skipped: %s |\n' \
            "$cell" "$n" "$m" "$machines" "$avail" "$reason" >>"$RESULTS"
          printf '{"label":"%s","status":"skipped","reason":"%s"}\n' "$cell" "$reason" >"$OUT/$cell.skipped.json"
          skipped=$((skipped + 1))
          continue
        fi
        svcs="${pre}0..$((n - 1))"
        for ((r = 1; r <= R; r++)); do
          wait_calm 300 || log "WARNING: $cell r$r starts without a calm host (recorded in the log)"
          log "$cell r$r: priming ($m session(s) over $n service(s))$note"
          bench -services "$svcs" -sessions "$m" -calls 0 -label "$cell-prime" -sock "" \
            >/dev/null 2>"$OUT/$cell-r$r.prime.log"
          if [ "$mode" = frozen ]; then
            # El gateway congela lo que lleva -idle sin uso; se espera a que no
            # quede ni una viva, que es el punto de partida que se quiere medir.
            wait_calm 300 || log "WARNING: $cell r$r: instances not frozen after 300s"
            settle="$SETTLE"
          else
            settle=0s
          fi
          log "$cell r$r: measuring"
          if bench -services "$svcs" -sessions "$m" -settle "$settle" -label "$cell-r$r" \
               -json "$OUT/$cell-r$r.json" -md >>"$RESULTS" 2>"$OUT/$cell-r$r.log"; then
            runs=$((runs + 1))
            grep -q '"status": "DEGRADED"' "$OUT/$cell-r$r.json" && degraded=$((degraded + 1))
            tail -n +1 "$OUT/$cell-r$r.log" | head -8
          else
            failed=$((failed + 1))
            log "$cell r$r: kling-mcpbench failed: $(tail -1 "$OUT/$cell-r$r.log")"
            printf '| %s-r%d | %d | %d | error | | | | | | | | | | | | | error: %s |\n' \
              "$cell" "$r" "$n" "$m" "$(tail -1 "$OUT/$cell-r$r.log" | tr '|' '/')" >>"$RESULTS"
          fi
        done
      done
    done
  done
done

echo
cat "$RESULTS"
echo
log "$runs measured run(s), $degraded DEGRADED, $skipped skipped cell(s), $failed bench error(s)"
log "raw JSON and logs in $OUT"
[ "$failed" = 0 ]
