#!/usr/bin/env bash
# CI: base de datos desechable por PR
#
# Prepara una copia de Postgres propia del PR, creada desde una plantilla
# golden, corre el comando con DATABASE_URL en su entorno y la destruye al
# salir. Idempotente: si la copia ya existe (reintento del job), la resetea
# (otra máquina, otra contraseña; el mismo ttl que tenía).
#
# Uso:
#   ci-pr-db.sh -golden GOLDEN -pr N [-ttl D] [-keep] [-H HOST] -- comando y args...
#
# Env (los flags ganan):
#   GOLDEN      Plantilla (requerida)
#   PR          Número de PR (requerido): la copia se llama pr-<PR>
#   KLING       Binario kling (defecto: kling)
#   KLING_HOST  Endpoint del daemon (defecto: el contexto activo)
#   TTL         TTL de la copia al crearla (defecto: 2h)
#   KEEP        Si es 1, no borra la copia al final
#
# La contraseña: DATABASE_URL sale de `kling db connect <copia> -dsn` directo a
# una variable, con la traza del shell apagada (set +x) mientras tanto, y solo
# viaja por el entorno del comando. Nunca en argv, ni en un fichero, ni en
# stdout. Los mensajes van a stderr.
#
# El daemon: -H se exporta como KLING_HOST, así kling y kling db (también el rm
# de la trampa) hablan con el mismo.
#
# Ejemplo:
#   GOLDEN=pg-golden ./ci-pr-db.sh -pr 42 -- pytest tests/
#
set -uo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }
log() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*" >&2; }

KLING="${KLING:-kling}"
GOLDEN="${GOLDEN:-}"
PR="${PR:-}"
TTL="${TTL:-2h}"
KEEP="${KEEP:-0}"

need() { [ "$2" -ge 2 ] || die "$1 needs a value"; }
while [ $# -gt 0 ]; do
  case "$1" in
    -golden) need "$1" $#; GOLDEN="$2"; shift 2 ;;
    -pr)     need "$1" $#; PR="$2"; shift 2 ;;
    -ttl)    need "$1" $#; TTL="$2"; shift 2 ;;
    -H)      need "$1" $#; export KLING_HOST="$2"; shift 2 ;;
    -keep)   KEEP=1; shift ;;
    --)      shift; break ;;
    *)       die "unknown flag: $1" ;;
  esac
done

[ -n "$GOLDEN" ] || die "GOLDEN is required (-golden or env)"
[ -n "$PR" ] || die "PR is required (-pr or env)"
case "$PR" in
  *[!A-Za-z0-9._-]*) die "PR must be letters, digits, '.', '_' or '-': $PR" ;;
esac
[ $# -ge 1 ] || die "no command given (use -- before the command)"
command -v "$KLING" >/dev/null 2>&1 || die "kling not found: $KLING"

COPY_NAME="pr-$PR"

# timeout no existe en macOS: si no está, sin límite.
with_timeout() {
  local secs="$1"; shift
  if command -v timeout >/dev/null 2>&1; then
    timeout "$secs" "$@"
  else
    "$@"
  fi
}

# TOUCHED: esta ejecución creó o reseteó la copia, y la trampa la borra.
TOUCHED=0
cleanup() {
  local rc=$?
  trap - EXIT INT TERM
  unset DATABASE_URL
  if [ "$TOUCHED" = 1 ]; then
    if [ "$KEEP" = 1 ]; then
      log "KEEP=1: $COPY_NAME not removed (kling db rm $COPY_NAME)"
    else
      log "removing $COPY_NAME..."
      # kling db rm: la máquina Y su contraseña del host.
      with_timeout 120 "$KLING" db rm "$COPY_NAME" >/dev/null 2>&1 \
        || log "warning: could not remove $COPY_NAME (kling db rm $COPY_NAME)"
    fi
  fi
  exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

log "ci-pr-db: PR=$PR GOLDEN=$GOLDEN TTL=$TTL"

# Crea o resetea. up y reset vuelven cuando la copia está lista (o fallan y no
# dejan nada a medias).
TOUCHED=1
if "$KLING" inspect "$COPY_NAME" >/dev/null 2>&1; then
  log "$COPY_NAME exists: resetting it..."
  "$KLING" db reset "$COPY_NAME" >/dev/null || die "failed to reset $COPY_NAME"
else
  log "creating $COPY_NAME from $GOLDEN..."
  "$KLING" db up "$GOLDEN" -name "$COPY_NAME" -ttl "$TTL" >/dev/null || die "failed to create $COPY_NAME"
fi

# Lista: kling db connect (sin -dsn) exige lo mismo que -dsn (en marcha, lista,
# contraseña de ESTE id en el host) sin imprimir la clave.
tries=30
until "$KLING" db connect "$COPY_NAME" >/dev/null 2>&1; do
  tries=$((tries - 1))
  [ "$tries" -gt 0 ] || die "$COPY_NAME is not ready"
  sleep 1
done
log "$COPY_NAME is ready"

# DATABASE_URL, con la traza apagada: set -x (o CI_DEBUG_TRACE) la imprimiría.
case $- in *x*) xtrace=1; set +x ;; *) xtrace=0 ;; esac
DATABASE_URL="$("$KLING" db connect "$COPY_NAME" -dsn </dev/null 2>/dev/null)" || DATABASE_URL=""
if [ -z "$DATABASE_URL" ]; then
  [ "$xtrace" = 0 ] || set -x
  die "failed to get DATABASE_URL from $COPY_NAME"
fi
export DATABASE_URL
[ "$xtrace" = 0 ] || set -x

log "running: $*"
"$@"
rc=$?
log "command exited with $rc"
exit "$rc"
