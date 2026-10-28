#!/bin/sh
# N restores simultáneos del mismo snapshot: cada uno ejecuta un comando por la
# consola (si se da), sigue vivo y a los STATS ms imprime su phys_footprint.
# Uso: n-restores.sh SNAPDIR N [TEXTO_ENTRADA MARCA]
#   n-restores.sh /tmp/snap-sh 6 'echo $((6*7))x\n' 42x
set -e
K="$(cd "$(dirname "$0")" && pwd)/kling-hv"
SNAP=$1
N=${2:-6}
IN=$3
MARK=$4
STATS=${STATS:-4000}
OUT=$(mktemp -d)
i=1
while [ "$i" -le "$N" ]; do
  if [ -n "$IN" ]; then
    "$K" restore "$SNAP" -i "$IN" --mark "$MARK" --on-mark stats --stats-after "$STATS" \
      --exit-after $((STATS + 500)) </dev/null >"$OUT/$i.log" 2>&1 &
  else
    "$K" restore "$SNAP" --stats-after "$STATS" --exit-after $((STATS + 500)) </dev/null >"$OUT/$i.log" 2>&1 &
  fi
  i=$((i + 1))
done
wait
i=1
while [ "$i" -le "$N" ]; do
  r=$(grep '\] estado restaurado' "$OUT/$i.log" | awk '{print $2}' | tr -d ']')
  m=$(grep -o 'stats pedidas: phys_footprint=[0-9.]*' "$OUT/$i.log" | tail -1 | cut -d= -f2)
  e=$(grep -o 'stats pedidas: .*' "$OUT/$i.log" | tail -1 | sed 's/.*external=\([0-9.]*\).*/\1/')
  t=$(grep -o 'marca [^ ]* vista a [0-9.]*' "$OUT/$i.log" | awk '{print $NF}')
  echo "restore $i: restaurado a ${r} ms, marca a ${t:--} ms, phys_footprint ${m} MiB, external ${e} MiB"
  i=$((i + 1))
done
for f in "$OUT"/*.log; do grep -o 'stats pedidas: phys_footprint=[0-9.]*' "$f" | tail -1 | cut -d= -f2; done |
  awk '{s += $1} END {printf "suma phys_footprint: %.2f MiB (%d procesos)\n", s, NR}'
echo "logs en $OUT"
