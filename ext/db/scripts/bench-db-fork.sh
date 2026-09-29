#!/usr/bin/env bash
# Base de datos desechable por test: kindling (run -from / fork) contra docker
# y contra CREATE DATABASE ... TEMPLATE, en el MISMO host. El método y las
# reglas de honestidad (toda ronda al informe, DEGRADED por encima del 1 %,
# celdas que no caben = skipped, nada de repetir hasta que salga bonito) están
# en docs/thaw-at-scale.md; el binario que mide es kling-dbbench.
#
# Corre EN el host Linux del daemon, con un usuario que puede usar `kling` y
# `docker`. El host no necesita Go: kling-dbbench se compila en otro sitio
# (GOOS=linux GOARCH=amd64 go build -o kling-dbbench ./cmd/kling-dbbench, desde
# ext/db) y se copia al lado de este script.
#
#   GOLDEN=pg-golden DBBENCH_PASSWORD=... ./bench-db-fork.sh
#   MODES=docker,template TEMPLATE_ADDR=127.0.0.1:5432 DBBENCH_PASSWORD=... ./bench-db-fork.sh
#   NS=1,8 R=1 GOLDEN=pg-golden ./bench-db-fork.sh          una pasada corta
#
# Qué necesita cada modo:
#   kindling-run   GOLDEN: plantilla con Postgres escuchando y el seed cargado
#                  (100000 filas en items, o lo que digas con TABLE/EXPECT). En
#                  macOS la plantilla debe llevar la etiqueta kling.ports=5432.
#   kindling-fork  FORK_SRC: sandbox vivo con el seed (kling sandbox fork).
#   docker         docker y la imagen DOCKER_IMAGE (se baja fuera de la medida).
#   template       TEMPLATE_ADDR: un Postgres ya arrancado y un rol que pueda
#                  CREATE DATABASE (PG13+ por DROP ... WITH (FORCE)).
# La contraseña va SIEMPRE por entorno (DBBENCH_PASSWORD), nunca por argumento.
#
# Qué hace: preflight (herramientas, RAM, disco y PSI los comprueba el propio
# binario), la medida, y una limpieza con trap que borra TODO lo que empiece
# por el prefijo dbb (contenedores, máquinas y bases) aunque el binario muera.
#
# Env: BENCH (./kling-dbbench), KLING, DOCKER, MODES (kindling-run,docker,template),
#   NS (1,8,32), R (3), TIMEOUT (60s), GOLDEN, FORK_SRC, TEMPLATE_ADDR,
#   DOCKER_IMAGE (postgres:16-alpine), DB_USER (postgres), DB_NAME (postgres),
#   TABLE (items), EXPECT (100000), SEED_SQL, PG_PORT (5432), PSI_MAX (5),
#   DISK_PATH (donde viven las copias; vacío = no se mide el disco),
#   KLING_RUN_ARGS (p. ej. -mem,256M), OUT (dir de salida), KEEP=1 (no limpia).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

BENCH="${BENCH:-$HERE/kling-dbbench}"
KLING="${KLING:-kling}"
DOCKER="${DOCKER:-docker}"
MODES="${MODES:-kindling-run,docker,template}"
NS="${NS:-1,8,32}"
R="${R:-3}"
TIMEOUT="${TIMEOUT:-60s}"
GOLDEN="${GOLDEN:-}"
FORK_SRC="${FORK_SRC:-}"
TEMPLATE_ADDR="${TEMPLATE_ADDR:-}"
DOCKER_IMAGE="${DOCKER_IMAGE:-postgres:16-alpine}"
DB_USER="${DB_USER:-postgres}"
DB_NAME="${DB_NAME:-postgres}"
TABLE="${TABLE:-items}"
EXPECT="${EXPECT:-100000}"
SEED_SQL="${SEED_SQL:-}"
PG_PORT="${PG_PORT:-5432}"
PSI_MAX="${PSI_MAX:-5}"
DISK_PATH="${DISK_PATH:-}"
KLING_RUN_ARGS="${KLING_RUN_ARGS:-}"
KEEP="${KEEP:-0}"
PREFIX=dbb
STAMP="$(date -u +%Y%m%d-%H%M%S)"
if [ -d "$HERE/../../../docs/bench-data" ]; then
  OUT="${OUT:-$HERE/../../../docs/bench-data/db-fork-$STAMP}"
else
  OUT="${OUT:-$HERE/db-fork-$STAMP}"
fi

log() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() { echo "ERROR: $*" >&2; exit 1; }
has() { case ",$MODES," in *",$1,"*) return 0 ;; *) return 1 ;; esac; }

[ -x "$BENCH" ] || die "missing $BENCH (build: cd ext/db && GOOS=linux go build -o kling-dbbench ./cmd/kling-dbbench)"
if has template && [ -z "${DBBENCH_PASSWORD:-}" ]; then die "template mode needs DBBENCH_PASSWORD in the environment"; fi
if has kindling-run; then
  command -v "$KLING" >/dev/null 2>&1 || die "missing $KLING"
  [ -n "$GOLDEN" ] || die "kindling-run needs GOLDEN=<template>"
fi
if has kindling-fork; then
  command -v "$KLING" >/dev/null 2>&1 || die "missing $KLING"
  [ -n "$FORK_SRC" ] || die "kindling-fork needs FORK_SRC=<sandbox>"
fi
has docker && { command -v "$DOCKER" >/dev/null 2>&1 || die "missing $DOCKER"; }
has template && [ -z "$TEMPLATE_ADDR" ] && die "template needs TEMPLATE_ADDR=host:port"

# Ningún resto de una prueba anterior: los borraría esta limpieza y sería
# tratar como propio algo que no lo es.
leftovers() {
  if has docker; then "$DOCKER" ps -aq --filter "name=^${PREFIX}_" 2>/dev/null; fi
  if has kindling-run || has kindling-fork; then
    "$KLING" ps -a -q 2>/dev/null | while read -r id; do
      [ -n "$id" ] && "$KLING" inspect "$id" 2>/dev/null | grep -q "\"name\": \"${PREFIX}_" && echo "$id"
    done
  fi
}
if [ -n "$(leftovers)" ] && [ "${FORCE:-0}" != 1 ]; then
  die "leftovers named ${PREFIX}_* from an earlier run: remove them or FORCE=1"
fi

# Limpieza: contenedores y máquinas con el prefijo. Las bases de template las
# borra el propio binario (necesita hablar con Postgres); si el binario murió,
# quedan bases ${PREFIX}_* que se ven con \l.
cleanup() {
  [ "$KEEP" = 1 ] && { log "KEEP=1: nothing is cleaned"; return; }
  if has docker; then
    ids="$("$DOCKER" ps -aq --filter "name=^${PREFIX}_" 2>/dev/null)"
    [ -n "$ids" ] && "$DOCKER" rm -f -v $ids >/dev/null 2>&1
  fi
  if has kindling-run || has kindling-fork; then
    ids="$("$KLING" ps -a -q 2>/dev/null | while read -r id; do
      "$KLING" inspect "$id" 2>/dev/null | grep -q "\"name\": \"${PREFIX}_" && echo "$id"
    done)"
    [ -n "$ids" ] && "$KLING" rm -f $ids >/dev/null 2>&1
  fi
  return 0
}
trap cleanup EXIT INT TERM

mkdir -p "$OUT"
log "kling-dbbench: modes=$MODES N=$NS R=$R  ->  $OUT"

args=(-modes "$MODES" -n "$NS" -r "$R" -timeout "$TIMEOUT"
  -kling "$KLING" -docker "$DOCKER" -docker-image "$DOCKER_IMAGE"
  -user "$DB_USER" -db "$DB_NAME" -table "$TABLE" -expect "$EXPECT"
  -pg-port "$PG_PORT" -psi-max "$PSI_MAX" -prefix "$PREFIX"
  -json "$OUT/dbbench.json" -md "$OUT/dbbench.md")
[ -n "$GOLDEN" ] && args+=(-golden "$GOLDEN")
[ -n "$FORK_SRC" ] && args+=(-fork-src "$FORK_SRC")
[ -n "$TEMPLATE_ADDR" ] && args+=(-template-addr "$TEMPLATE_ADDR")
[ -n "$SEED_SQL" ] && args+=(-seed-sql "$SEED_SQL")
[ -n "$DISK_PATH" ] && args+=(-disk-path "$DISK_PATH")
[ -n "$KLING_RUN_ARGS" ] && args+=(-kling-run-args "$KLING_RUN_ARGS")
# Banderas extra del binario (p. ej. -est-mem-mib 64 tras medir una pasada): se pasan tal cual.
args+=("$@")

"$BENCH" "${args[@]}" 2> >(tee "$OUT/dbbench.log" >&2)
rc=$?
log "done (exit $rc): $OUT/dbbench.json  $OUT/dbbench.md"
exit $rc
