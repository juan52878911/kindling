#!/usr/bin/env bash
# Test de estrés: lotes de N microVMs midiendo RAM, disco, CPU y fallos.
# Ejecutar EN el host del daemon (usa el socket local, sin ruido de SSH).
#
#   ./60-stress.sh 10 20 50
#
# Cada lote: arranca en paralelo -> mide -> congela todo -> mide -> descongela
# todo -> mide -> limpia. Los fallos se cuentan, no abortan: encontrar el techo
# es el objetivo del test.
set -uo pipefail

BATCHES=("${@:-10 20 50}")
[ $# -gt 0 ] && BATCHES=("$@")
MEM_MIB="${MEM_MIB:-256}"
ROOT="${KLING_ROOT:-/var/lib/kindling}"
KLING="${KLING:-kling}"

command -v "$KLING" >/dev/null || { echo "no encuentro kling" >&2; exit 1; }

mem_used()  { free -m | awk '/^Mem:/ {print $3}'; }
mem_avail() { free -m | awk '/^Mem:/ {print $7}'; }
load1()     { cut -d' ' -f1 /proc/loadavg; }
# pgrep -c y grep -c salen con código 1 cuando no hay coincidencias, así que un
# `|| echo 0` detrás imprimiría DOS ceros. Hay que capturar el fallo, no encadenar.
fc_count()  { local n; n=$(pgrep -c firecracker 2>/dev/null) || n=0; echo "$n"; }
fc_rss()    { ps -eo rss,comm 2>/dev/null | awk '$2=="firecracker"{s+=$1} END {printf "%d", s/1024}'; }
disk_mb()   { du -sm "$ROOT/machines" 2>/dev/null | cut -f1 || echo 0; }

snapshot_line() { # etiqueta
  printf "  %-22s RAM=%5sMiB libre=%5sMiB  fc=%3s procs  RSS=%5sMiB  disco=%5sMiB  load=%s\n" \
    "$1" "$(mem_used)" "$(mem_avail)" "$(fc_count)" "$(fc_rss)" "$(disk_mb)" "$(load1)"
}

cleanup_all() {
  local ids
  ids=$($KLING ps -a -json 2>/dev/null | sed 's/},{/}\n{/g' | grep -oE '"id":"[a-f0-9]+"' | cut -d'"' -f4)
  for id in $ids; do $KLING rm "$id" >/dev/null 2>&1; done
  pkill -f "firecracker --api-sock" 2>/dev/null
  sleep 1
}

echo "== test de estrés kindling =="
echo "   RAM por máquina: ${MEM_MIB} MiB | host: $(nproc) cores, $(free -m | awk '/^Mem:/{print $2}') MiB"
echo

for N in "${BATCHES[@]}"; do
  echo "── lote de $N ──────────────────────────────────────────────"
  cleanup_all
  snapshot_line "reposo"

  # arranque en paralelo: es lo que estresa de verdad
  t0=$(date +%s%N)
  for i in $(seq 1 "$N"); do
    $KLING run -name "s${N}-${i}" -mem "$MEM_MIB" >/dev/null 2>&1 &
  done
  wait
  t1=$(date +%s%N)

  ok=$($KLING ps -json 2>/dev/null | grep -o '"state":"running"' | wc -l | tr -d ' ')
  echo "  arranque: $(( (t1-t0)/1000000 )) ms para $N  |  vivas: $ok/$N  |  fallos: $((N-ok))"
  sleep 5   # que terminen de bootear antes de medir
  snapshot_line "todas en marcha"

  # congelar todo
  t0=$(date +%s%N)
  for i in $(seq 1 "$N"); do $KLING freeze "s${N}-${i}" >/dev/null 2>&1 & done
  wait
  t1=$(date +%s%N)
  warm=$($KLING ps -json 2>/dev/null | grep -o '"state":"warm"' | wc -l | tr -d ' ')
  echo "  freeze:   $(( (t1-t0)/1000000 )) ms  |  warm: $warm/$N"
  snapshot_line "todas congeladas"

  # descongelar todo
  t0=$(date +%s%N)
  for i in $(seq 1 "$N"); do $KLING thaw "s${N}-${i}" >/dev/null 2>&1 & done
  wait
  t1=$(date +%s%N)
  back=$($KLING ps -json 2>/dev/null | grep -o '"state":"running"' | wc -l | tr -d ' ')
  echo "  thaw:     $(( (t1-t0)/1000000 )) ms  |  running: $back/$N"
  snapshot_line "todas descongeladas"

  # OOM killer: la señal más clara de haber tocado techo
  oom=$(dmesg 2>/dev/null | grep -ci "out of memory\|oom-kill") || oom=0
  [ "$oom" -gt 0 ] && echo "  ⚠ OOM killer activo: $oom menciones en dmesg"
  echo
done

cleanup_all
echo "== fin =="
