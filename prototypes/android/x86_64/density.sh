#!/bin/bash
# density.sh — cuántos teléfonos caben en este anfitrión Linux (Firecracker) y
# cuánto cuesta cada clon extra. docs/proxmox.md.
#
#   x86_64/density.sh measure               una foto: PSS/RSS/privada de cada firecracker
#   x86_64/density.sh run [-max N] [-settle S]
#                                           teléfonos de uno en uno desde el dorado,
#                                           midiendo tras cada uno, hasta N o hasta que
#                                           salte una guarda (ver "guardas")
#   x86_64/density.sh verify [-par N] [tel...]  --verify-cache de N en N (≤ 4)
#   x86_64/density.sh safe                  ¿se puede añadir algo ahora?
#   x86_64/density.sh clean                 borra los teléfonos de PHONE_PREFIX
#   x86_64/density.sh health                ¿siguen sanos todos? (adb, boot_completed,
#                                           system_server, logcat -b crash)
#
# Por qué PSS y no RSS: los clones de un dorado mapean su mem.file MAP_PRIVATE,
# así que las páginas que nadie ha escrito son caché de página COMPARTIDA entre
# todos (Shared_Clean en smaps). RSS las cuenta en cada proceso; PSS las
# reparte. El coste de un clon extra es lo que sube la suma de PSS (≈ su
# Private_Dirty: lo que el invitado ha escrito), y lo que baja MemAvailable.
#
# Usa phone.sh (PHONE_* se respetan: PHONE_MEM, PHONE_GOLDEN, PHONE_PREFIX...)
# contra el daemon del sistema (PHONE_SOCK=/run/kling.sock por defecto).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PHONE="$HERE/../phone.sh"
export PHONE_SOCK="${PHONE_SOCK:-/run/kling.sock}"
export PHONE_ROOT="${PHONE_ROOT:-/var/lib/kindling-phone}"
export KLING_HOST="unix://$PHONE_SOCK"
OUT="${OUT:-$PHONE_ROOT/density}"
# Solo se miden y tocan los teléfonos de este prefijo: en el CT puede haber
# teléfonos de otros (p. ej. los phone-N del usuario).
PREFIX="${PHONE_PREFIX:-phone}"
export PHONE_PREFIX="$PREFIX"

die() { printf 'density.sh: %s\n' "$*" >&2; exit 1; }
kib() { awk -v k="$1" '$1 == k":" { print $2 }' "$2" 2>/dev/null || echo 0; }
meminfo() { awk -v k="$1" '$1 == k":" { print int($2 / 1024) }' /proc/meminfo; }

# pids: "nombre pid" de las máquinas con la etiqueta de teléfono que corren.
pids() {
  kling ps -json | python3 -c '
import json, sys
p = sys.argv[1] + "-"
for m in json.load(sys.stdin) or []:
    if not m["name"].startswith(p) or not m["name"][len(p):].isdigit():
        continue
    if (m.get("labels") or {}).get("kindling.phone") and m.get("state") in ("running", "paused") and m.get("pid"):
        print(m["name"], m["pid"], m["state"])' "$PREFIX"
}

# measure: una línea por teléfono y el total. MiB.
measure() {
  local n pid st f rss pss priv shc tot_pss=0 tot_rss=0 tot_priv=0 c=0
  printf '%-14s %8s %7s %8s %8s %9s %9s\n' NAME PID STATE RSS PSS PRIV_DIRTY SHR_CLEAN
  while read -r n pid st; do
    f="/proc/$pid/smaps_rollup"
    [ -r "$f" ] || continue
    rss=$(( $(kib Rss "$f") / 1024 )); pss=$(( $(kib Pss "$f") / 1024 ))
    priv=$(( $(kib Private_Dirty "$f") / 1024 )); shc=$(( $(kib Shared_Clean "$f") / 1024 ))
    printf '%-14s %8s %7s %8s %8s %9s %9s\n' "$n" "$pid" "$st" "$rss" "$pss" "$priv" "$shc"
    tot_pss=$((tot_pss + pss)); tot_rss=$((tot_rss + rss)); tot_priv=$((tot_priv + priv)); c=$((c + 1))
  done < <(pids)
  printf 'phones=%d  sum_rss=%d MiB  sum_pss=%d MiB  sum_priv_dirty=%d MiB  MemAvailable=%d MiB  Cached=%d MiB  SwapFree=%d MiB\n' \
    "$c" "$tot_rss" "$tot_pss" "$tot_priv" "$(meminfo MemAvailable)" "$(meminfo Cached)" "$(meminfo SwapFree)"
}

# salud de un teléfono: adb contesta, boot_completed=1, system_server vivo y
# sin SIGILL ni caída de system_server en logcat -b crash.
sano() {
  local s out
  s="$("$PHONE" adb "$1" 2>/dev/null)" || { echo "$1: no adb"; return 1; }
  out="$(timeout 30 adb -s "$s" shell 'getprop sys.boot_completed; pidof system_server >/dev/null || echo NO_SS;
    logcat -d -b crash | grep -cE "SIGILL|SIGSEGV|IN SYSTEM PROCESS"' 2>&1 | tr -d '\r' | tr '\n' ' ')"
  case "$out" in "1 0 ") return 0 ;; esac
  echo "$1: $out"; return 1
}

health() {
  local n pid st bad=0 c=0
  while read -r n pid st; do
    [ "$st" = running ] || continue
    c=$((c + 1)); sano "$n" </dev/null || bad=$((bad + 1))
  done < <(pids)
  echo "health: $c running, $bad unhealthy"
  [ "$bad" -eq 0 ]
}

# ── guardas: el CT comparte anfitrión con otras VMs ──────────────────────────
# El 2026-09-28 un `squeeze` de 24 clones más --verify-cache en todos a la vez
# dejó el CT sin memoria ni swap, colgado (sin OOM), y cargó el host (load 73,
# swap del host). Reglas desde entonces (docs/proxmox.md, sección 4):
#   - nunca `kling machine squeeze` con clones de un mismo dorado (no hay -squeeze);
#   - antes de cada teléfono o comprobación: MemAvailable ≥ MIN_AVAIL_MIB (1024),
#     swap del CT sin crecer y la presión de memoria DEL CT (su cgroup, no la
#     del host que enseña /proc/pressure en un LXC) por debajo de MAX_PSI;
#   - --verify-cache de a lo sumo 4 a la vez.
# Los teléfonos de otros (otro prefijo) cuentan en MemAvailable pero no se
# miden ni se tocan.
MIN_AVAIL_MIB="${MIN_AVAIL_MIB:-1024}"
MAX_PSI="${MAX_PSI:-5}"            # some avg10 (%) de memory.pressure del cgroup del CT
MAX_SWAP_MIB="${MAX_SWAP_MIB:-16}" # swap usada del CT por encima de la de partida
SWAP0="$(( $(meminfo SwapTotal) - $(meminfo SwapFree) ))"
psi() {
  local f=/sys/fs/cgroup/memory.pressure
  [ -r "$f" ] || f=/proc/pressure/memory
  awk '$1 == "some" { split($2, a, "="); print int(a[2]) }' "$f"
}
# seguro: 0 si se puede seguir; si no, imprime por qué.
seguro() {
  local av sw p
  av="$(meminfo MemAvailable)"; sw=$(( $(meminfo SwapTotal) - $(meminfo SwapFree) - SWAP0 )); p="$(psi)"
  if [ "$av" -lt "$MIN_AVAIL_MIB" ]; then echo "MemAvailable $av MiB < $MIN_AVAIL_MIB"; return 1; fi
  if [ "$sw" -gt "$MAX_SWAP_MIB" ]; then echo "the CT is swapping ($sw MiB more than at start)"; return 1; fi
  if [ "$p" -ge "$MAX_PSI" ]; then echo "memory pressure of the CT some avg10=$p % >= $MAX_PSI"; return 1; fi
  return 0
}

row() {  # n,sum_pss,sum_rss,sum_priv,avail,swap_used,psi
  measure | awk -v n="$1" -v sw="$(( $(meminfo SwapTotal) - $(meminfo SwapFree) ))" -v p="$(psi)" '/^phones=/ {
    for (f = 1; f <= NF; f++) { split($f, kv, "="); v[kv[1]] = kv[2] + 0 }
    print n "," v["sum_pss"] "," v["sum_rss"] "," v["sum_priv_dirty"] "," v["MemAvailable"] "," sw "," p }'
}

run() {
  local max=40 settle=20 i why
  while [ $# -gt 0 ]; do
    case "$1" in
      -max) max="$2"; shift 2 ;;
      -settle) settle="$2"; shift 2 ;;
      -squeeze) die "-squeeze was removed: with clones of one golden it turns shared pages into private ones and filled the CT (docs/proxmox.md)" ;;
      *) die "run: unknown argument $1" ;;
    esac
  done
  mkdir -p "$OUT"
  local csv
  csv="$OUT/density-$(date +%Y%m%d-%H%M%S)-mem${PHONE_MEM:-1536}.csv"
  echo "n,sum_pss_mib,sum_rss_mib,sum_priv_dirty_mib,mem_available_mib,swap_used_mib,psi_some10" >"$csv"
  echo "==> base: $(measure | tail -n 1)  (prefix $PREFIX, MIN_AVAIL_MIB=$MIN_AVAIL_MIB, MAX_PSI=$MAX_PSI)"
  row 0 >>"$csv"
  for i in $(seq 1 "$max"); do
    if ! why="$(seguro)"; then echo "==> stop at $((i - 1)) phones: $why"; break; fi
    "$PHONE" up -n 1 || { echo "==> phone $i failed"; break; }
    sleep "$settle"
    measure | tail -n 1 | tee -a "$OUT/measure.log"
    row "$i" >>"$csv"
  done
  measure
  health || true
  echo "csv: $csv"
}

# verify [-par N] [teléfonos...]: android-sh --verify-cache de a lo sumo N a la
# vez (4), parando si una guarda salta. Leer ~1,3 GB por la caché del invitado
# escribe páginas de su RAM: en un clon eso son páginas PRIVADAS nuevas en el
# anfitrión (se imprime cuánto baja MemAvailable por tanda).
verify() {
  local par=4 m why bad=0 n=0 a0
  local -a todos b
  if [ "${1:-}" = -par ]; then par="$2"; shift 2; fi
  [ "$par" -ge 1 ] && [ "$par" -le 4 ] || die "-par must be 1..4"
  if [ $# -gt 0 ]; then todos=("$@"); else mapfile -t todos < <(pids | awk '$3 == "running" { print $1 }'); fi
  mkdir -p "$OUT/verify"
  while [ ${#todos[@]} -gt 0 ]; do
    if ! why="$(seguro)"; then echo "==> verify stopped with ${#todos[@]} left: $why"; break; fi
    a0="$(meminfo MemAvailable)"
    b=("${todos[@]:0:$par}"); todos=("${todos[@]:$par}")
    for m in "${b[@]}"; do
      ( kling exec -timeout 600s "$m" -- android-sh --verify-cache >"$OUT/verify/$m.txt" 2>&1; echo "rc=$?" >>"$OUT/verify/$m.txt" ) &
    done
    wait
    for m in "${b[@]}"; do
      n=$((n + 1))
      if grep -q '^rc=0$' "$OUT/verify/$m.txt"; then echo "$m: $(grep '^files=' "$OUT/verify/$m.txt")"
      else bad=$((bad + 1)); echo "$m: FAIL"; grep -E 'MISMATCH|files=|android-sh' "$OUT/verify/$m.txt" | head -5; fi
    done
    echo "   batch of ${#b[@]}: MemAvailable $a0 -> $(meminfo MemAvailable) MiB, psi $(psi)"
  done
  echo "verify: $n checked, $bad with mismatches"
  [ "$bad" -eq 0 ]
}

# clean: borra SOLO los teléfonos de este prefijo (nunca phone.sh rm -a, que
# se llevaría los de otros).
clean() {
  local m
  for m in $(pids | awk '{ print $1 }'); do "$PHONE" rm "$m"; done
}

case "${1:-}" in
  measure) measure ;;
  run) shift; run "$@" ;;
  verify) shift; verify "$@" ;;
  clean) clean ;;
  safe) if why="$(seguro)"; then echo "safe: MemAvailable $(meminfo MemAvailable) MiB, psi $(psi)"; else echo "NOT safe: $why"; exit 1; fi ;;
  health) health ;;
  *) sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
