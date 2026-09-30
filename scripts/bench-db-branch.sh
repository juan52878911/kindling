#!/usr/bin/env bash
# Banco de kling db branch: lo que tarda `git checkout` con el gancho puesto.
#
# Crea un repo git temporal, le da a main una copia del golden GOLDEN, instala
# el gancho y mide N veces tres casos, de extremo a extremo (el reloj envuelve
# el `git checkout` entero, gancho incluido):
#
#   existing   volver a una rama que ya tiene copia (main <-> other)
#   new-spare  git checkout -b desde main con la copia de reserva lista
#   new-fork   git checkout -b con las reservas apagadas (KLING_DB_BRANCH_SPARE=0):
#              el fork en caliente de la copia de main
#
# Entre medida y medida espera a que acabe el trabajo de segundo plano del
# gancho (congelar las otras ramas, sacar la reserva): mide lo que espera quien
# hace el checkout, no una cola de checkouts encadenados. Cada rama nueva se
# borra (copia incluida) antes de la siguiente, para no llenar el disco.
#
#   GOLDEN=pg N=20 ./scripts/bench-db-branch.sh
#   CASES="existing new-spare" N=5 OUT=/tmp/b ./scripts/bench-db-branch.sh
#   PAUSE_S=30 CASES=new-fork ./scripts/bench-db-branch.sh   deja respirar al disco
#
# PAUSE_S (0) son los segundos de espera tras cada medida, además de la del
# trabajo de fondo: sin ella, cada fork (vuelca 1 GiB con el golden pg) compite
# con la escritura a disco de los congelados de la vuelta anterior.
#
# Deja en OUT: <caso>.csv (iteración, ms), trace-<caso>-<i>.txt (la traza por
# fases de KLING_DB_TRACE) y summary.txt (p50/p95/min/max por caso). No
# descarta ni repite ninguna medida.
set -euo pipefail

GOLDEN="${GOLDEN:-pg}"
N="${N:-20}"
CASES="${CASES:-existing new-spare new-fork}"
OUT="${OUT:-./bench-db-branch-$(date +%Y%m%d-%H%M%S)}"
KLING="${KLING:-kling}"
PAUSE_S="${PAUSE_S:-0}"

die() { echo "bench-db-branch: $*" >&2; exit 1; }
command -v git >/dev/null || die "needs git"
command -v "$KLING" >/dev/null || die "no $KLING in PATH (set KLING)"

# Milisegundos de reloj de pared: $EPOCHREALTIME en bash 5; si no (el bash 3.2
# de macOS), perl.
now_ms() {
  if [ -n "${EPOCHREALTIME:-}" ]; then
    local t=${EPOCHREALTIME/[.,]/}
    echo $((t / 1000))
  else
    perl -MTime::HiRes=time -e 'printf "%d\n", time * 1000'
  fi
}

# idle espera a que no quede ningún `kling db branch -settle` (el trabajo de
# segundo plano del gancho), como mucho 5 minutos.
idle() {
  local i
  for i in $(seq 1 3000); do
    pgrep -f "branch -settle" >/dev/null 2>&1 || return 0
    sleep 0.1
  done
  die "the background work of the hook did not finish"
}

mkdir -p "$OUT"
OUT=$(cd "$OUT" && pwd)
REPO=$(mktemp -d "${TMPDIR:-/tmp}/bench-db-branch.XXXXXX")
cleanup() {
  cd "$REPO" 2>/dev/null || return
  git checkout -q main >/dev/null 2>&1 || true
  idle || true
  for b in $(git for-each-ref --format='%(refname:short)' refs/heads); do
    "$KLING" db branch -rm "$b" >/dev/null 2>&1 || true
  done
  cd / && rm -rf "$REPO"
}
trap cleanup EXIT

cd "$REPO"
git init -q -b main
git -c user.name=bench -c user.email=bench@localhost commit -q --allow-empty -m init
"$KLING" db branch -golden "$GOLDEN" >/dev/null
"$KLING" db branch hook install >/dev/null
export KLING_DB_TRACE=1

# checkout <caso> <i> <args de git checkout...>: mide uno y lo apunta.
checkout() {
  local c=$1 i=$2
  shift 2
  local t0 t1
  t0=$(now_ms)
  git checkout -q "$@" 2>"$OUT/trace-$c-$i.txt"
  t1=$(now_ms)
  echo "$i,$((t1 - t0))" >>"$OUT/$c.csv"
  idle
  sleep "$PAUSE_S"
}

summary() {
  local c=$1
  [ -s "$OUT/$c.csv" ] || return 0
  cut -d, -f2 "$OUT/$c.csv" | sort -n | awk -v c="$c" '
    { v[NR] = $1 }
    END {
      p50 = v[int((NR + 1) / 2)]; i95 = int(NR * 0.95 + 0.999); if (i95 > NR) i95 = NR
      printf "%-10s n=%d  p50 %d ms  p95 %d ms  min %d ms  max %d ms\n", c, NR, p50, v[i95], v[1], v[NR]
    }'
}

{
  echo "# $(date -u +%Y-%m-%dT%H:%M:%SZ)  host $(uname -n) $(uname -sm)"
  echo "# $("$KLING" version 2>&1 | tr '\n' ' ')"
  echo "# golden $GOLDEN  N=$N  PAUSE_S=$PAUSE_S"
} >"$OUT/summary.txt"

for c in $CASES; do
  : >"$OUT/$c.csv"
  case "$c" in
  existing)
    git checkout -q -b other 2>/dev/null
    idle
    for i in $(seq 1 "$N"); do
      if [ $((i % 2)) = 1 ]; then checkout "$c" "$i" main; else checkout "$c" "$i" other; fi
    done
    git checkout -q main 2>/dev/null
    idle
    ;;
  new-spare | new-fork)
    if [ "$c" = new-fork ]; then export KLING_DB_BRANCH_SPARE=0; else unset KLING_DB_BRANCH_SPARE; fi
    # La reserva se saca al salir de main: una vuelta antes de empezar.
    git checkout -q -b warm 2>/dev/null
    idle
    git checkout -q main 2>/dev/null
    idle
    "$KLING" db branch -rm warm >/dev/null
    git branch -q -D warm
    for i in $(seq 1 "$N"); do
      checkout "$c" "$i" -b "b$i"
      git checkout -q main 2>/dev/null
      idle
      "$KLING" db branch -rm "b$i" >/dev/null
      git branch -q -D "b$i"
    done
    unset KLING_DB_BRANCH_SPARE
    ;;
  *) die "unknown case $c" ;;
  esac
  summary "$c" | tee -a "$OUT/summary.txt"
done
echo "raw data in $OUT"
