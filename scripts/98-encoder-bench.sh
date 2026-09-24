#!/usr/bin/env bash
# Mide un codificador de frases (kind embed) servido desde su dorado: memoria
# del dorado, restauración hasta el primer vector, latencia en caliente de una
# frase, el ciclo congelar/descongelar y la memoria de N réplicas. Sirve para
# Linux (Firecracker) y macOS (vz). Cifras en docs/codificador.md.
#
#   scripts/98-encoder-bench.sh enc-e5
#
# Variables: KLING (binario), RUNS (restauraciones, 5), WARM (peticiones en
# caliente, 200), REPLICAS (1..N, 3), PREFIX (nombre de las máquinas, enc-b).
set -euo pipefail

SNAP=${1:?usage: $0 <golden snapshot of an encoder>}
KLING=${KLING:-kling}
RUNS=${RUNS:-5}
WARM=${WARM:-200}
REPLICAS=${REPLICAS:-3}
PREFIX=${PREFIX:-enc-b}

made=()
cleanup() { for m in "${made[@]}"; do "$KLING" rm "$m" >/dev/null 2>&1 || true; done; }
trap cleanup EXIT

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
# stats: mediana (mín–máx, n) de números, uno por línea.
stats() { sort -n | awk '{a[NR]=$1} END {if (NR==0) {print "n/a"; exit} m=(NR%2)?a[(NR+1)/2]:(a[NR/2]+a[NR/2+1])/2; printf "%s (%s–%s, n=%d)", m, a[1], a[NR], NR}'; }
# pct: percentil p (0..100) de números, uno por línea.
pct() { sort -n | awk -v p="$1" '{a[NR]=$1} END {i=int(p/100*(NR-1))+1; printf "%.1f", a[i]}'; }
row() { printf "  %-46s %s\n" "$1" "$2"; }
step() { printf "\n%s\n" "$1"; }
addr() { # dirección del puerto 8000 de una máquina: IP en Linux, reenvío en macOS
  local j; j="$("$KLING" inspect "$1")"
  local f; f="$(jq -r '.forwards["8000"] // empty' <<<"$j")"
  if [ -n "$f" ]; then echo "$f"; else echo "$(jq -r .ip <<<"$j"):8000"; fi
}
embed1() { # embed1 <máquina> <texto>: una petición por el proxy del daemon
  "$KLING" models embed -json "$1" "$2"
}

info="$("$KLING" snapshots -json | jq --arg s "$SNAP" '.[]|select(.name==$s)')"
[ -n "$info" ] || { echo "no snapshot $SNAP" >&2; exit 1; }
model="$(jq -r '.labels["von.model"]' <<<"$info")"
step "Encoder $model  (snapshot $SNAP, $(jq -r .vcpus <<<"$info") vCPU, $(jq -r .mem_mib <<<"$info") MiB, $(uname -s))"
row "snapshot memory file (allocated)" "$(jq -r '.mem_bytes/1048576|floor' <<<"$info") MiB"

step "1. Restore to first vector (kling run -from + one embedding), $RUNS runs"
thaw=(); first=(); req=()
for i in $(seq "$RUNS"); do
  m="$PREFIX-t$i"; made+=("$m")
  t0=$(now_ms)
  out="$("$KLING" run -from "$SNAP" -name "$m" 2>&1)" || { row "run $i" "FAILED: $out"; continue; }
  a="$(embed1 "$m" "turn on the living room lights")" || { row "embed $i" "FAILED"; continue; }
  t1=$(now_ms)
  thaw+=("$(sed -n 's/.* in \([0-9]*\) ms.*/\1/p' <<<"$out")")
  first+=("$((t1 - t0))")
  req+=("$(jq -r .request_ms <<<"$a")")
  "$KLING" rm "$m" >/dev/null 2>&1
done
row "restore (daemon's thaw_ms), p50" "$(printf '%s\n' "${thaw[@]}" | stats) ms"
row "run -from to first vector, p50" "$(printf '%s\n' "${first[@]}" | stats) ms"
row "  of which the first request (daemon proxy)" "$(printf '%s\n' "${req[@]}" | stats) ms"

step "2. Warm latency, one short command per request, direct HTTP ($WARM requests)"
m="$PREFIX-w"; made+=("$m")
"$KLING" run -from "$SNAP" -name "$m" >/dev/null
a="$(addr "$m")"
embed1 "$m" "hola" >/dev/null
texts=("query: turn on the living room lights" "query: hace muchísimo calor en el salón" "query: baja un poco la persiana del dormitorio"
  "query: it's way too loud in here, can you do something" "query: pon la temperatura a veintidós grados")
lat=()
for i in $(seq "$WARM"); do
  t="${texts[$((i % ${#texts[@]}))]}"
  s="$(curl -s -o /dev/null -w '%{time_total}' -H 'Content-Type: application/json' \
    -d "$(jq -cn --arg t "$t" '{input:[$t]}')" "http://$a/v1/embeddings")"
  lat+=("$(awk -v s="$s" 'BEGIN{printf "%.2f", s*1000}')")
done
row "p50 / p90 / p99" "$(printf '%s\n' "${lat[@]}" | pct 50) / $(printf '%s\n' "${lat[@]}" | pct 90) / $(printf '%s\n' "${lat[@]}" | pct 99) ms"

step "3. Scale to zero and back (freeze, thaw, first vector), $RUNS runs"
fz=(); th=(); fv=()
for i in $(seq "$RUNS"); do
  t0=$(now_ms); "$KLING" freeze "$m" >/dev/null; t1=$(now_ms)
  "$KLING" thaw "$m" >/dev/null; t2=$(now_ms)
  embed1 "$m" "apaga la luz de la cocina" >/dev/null; t3=$(now_ms)
  fz+=("$((t1 - t0))"); th+=("$((t2 - t1))"); fv+=("$((t3 - t1))")
done
row "freeze, p50" "$(printf '%s\n' "${fz[@]}" | stats) ms"
row "thaw (kling thaw returns), p50" "$(printf '%s\n' "${th[@]}" | stats) ms"
row "thaw to first vector, p50" "$(printf '%s\n' "${fv[@]}" | stats) ms"
"$KLING" rm "$m" >/dev/null 2>&1

step "4. Memory of 1..$REPLICAS replicas (after one request each)"
printf "  %-10s %-16s %-18s %s\n" "replicas" "total MiB" "per replica" "sum of RSS MiB (Linux)"
for n in $(seq "$REPLICAS"); do
  m="$PREFIX-r$n"; made+=("$m")
  "$KLING" run -from "$SNAP" -name "$m" >/dev/null || { echo "  replica $n FAILED"; break; }
  embed1 "$m" "enciende la luz" >/dev/null
  top="$("$KLING" top -json)"
  tot="$(jq -r --arg p "$PREFIX-r" '[.machines[]|select(.name|startswith($p))|.pss_mib]|add' <<<"$top")"
  per="$(jq -r --arg p "$PREFIX-r" '[.machines[]|select(.name|startswith($p))|.pss_mib]|map(tostring)|join(" ")' <<<"$top")"
  rss="n/a"
  if [ "$(uname -s)" = Linux ]; then
    rss=0
    for pid in $(jq -r --arg p "$PREFIX-r" '.machines[]|select(.name|startswith($p))|.pid' <<<"$top"); do
      k=$(awk '/^VmRSS/{print $2}' "/proc/$pid/status" 2>/dev/null || echo 0)
      rss=$((rss + ${k:-0} / 1024))
    done
  fi
  printf "  %-10s %-16s %-18s %s\n" "$n" "$tot" "$per" "$rss"
done
