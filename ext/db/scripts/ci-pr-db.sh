#!/usr/bin/env bash
# CI: base de datos desechable por PR
#
# Prepara un entorno de CI con una copia de Postgres única para cada PR, creada
# desde una plantilla golden. La copia vive el tiempo del job y se destruye
# después. Idempotento: si la copia ya existe (p.ej. por reintentos del job),
# la resetea. DATABASE_URL va a un fichero 0600 nunca en stdout/logs.
#
# Uso:
#   ci-pr-db.sh -golden GOLDEN -pr N -keep -- comando y args...
#
# Env:
#   GOLDEN      Plantilla (requerida)
#   PR          Número de PR (requerida)
#   KLING       Binario kling (defecto: kling)
#   KLING_HOST  Endpoint del daemon (por defecto, contexto activo)
#   TTL         TTL de la copia (defecto: 2h)
#   DB_NAME     Base de datos (defecto: appdb, de la plantilla)
#   DB_USER     Rol de la aplicación (defecto: app, de la plantilla)
#   KEEP        Si es 1, no borra la copia al final
#   TIMEOUT     Timeout para kling exec (defecto: 60s)
#
# FLAGS:
#   -golden G   Plantilla (puede ir en env)
#   -pr N       Número de PR (puede ir en env)
#   -keep       No borra la copia al final (para debugging)
#   -ttl D      TTL de la copia (defecto 2h, sobrescribe env TTL)
#   -H HOST     Endpoint del daemon
#   --          Fin de flags, empieza el comando
#
# Si algo falla, la copia se destruye en la trampa EXIT. DATABASE_URL se
# guarda en un fichero temporal 0600 que el script elimina al salir.
#
# Ejemplo:
#   GOLDEN=pg-golden ./ci-pr-db.sh -pr 42 -- \
#     migration-runner /run/db.env && pytest tests/
#
set -uo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }
log() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }

# Defaults
KLING="${KLING:-kling}"
KLING_HOST="${KLING_HOST:-}"
GOLDEN="${GOLDEN:-}"
PR="${PR:-}"
TTL="${TTL:-2h}"
DB_NAME="${DB_NAME:-}"
DB_USER="${DB_USER:-}"
KEEP="${KEEP:-0}"
TIMEOUT="${TIMEOUT:-60s}"

# Parseo de flags
while [ $# -gt 0 ]; do
  case "$1" in
    -golden)
      GOLDEN="$2"
      shift 2
      ;;
    -pr)
      PR="$2"
      shift 2
      ;;
    -keep)
      KEEP=1
      shift
      ;;
    -ttl)
      TTL="$2"
      shift 2
      ;;
    -H)
      KLING_HOST="$2"
      shift 2
      ;;
    --)
      shift
      break
      ;;
    *)
      die "unknown flag: $1"
      ;;
  esac
done

# Validación
[ -n "$GOLDEN" ] || die "GOLDEN is required (-golden or env)"
[ -n "$PR" ] || die "PR is required (-pr or env)"
command -v "$KLING" >/dev/null 2>&1 || die "kling not found: $KLING"

# Nombre de la copia: pr-{PR}
COPY_NAME="pr-$PR"

# Fichero temporal para DATABASE_URL (0600, se borra en la trampa)
DB_ENV_FILE=""
cleanup() {
  [ -n "$DB_ENV_FILE" ] && rm -f "$DB_ENV_FILE"
  if [ "$KEEP" != 1 ]; then
    log "removing $COPY_NAME..."
    # Contexto propio para que no se cancele con Ctrl-C
    timeout 60 "$KLING" rm -f "$COPY_NAME" 2>/dev/null || true
  else
    log "KEEP=1: $COPY_NAME not removed"
  fi
}
trap cleanup EXIT INT TERM

# Kling con -H si se pasó
kling_cmd() {
  if [ -n "$KLING_HOST" ]; then
    "$KLING" -H "$KLING_HOST" "$@"
  else
    "$KLING" "$@"
  fi
}

# Inspecciona la copia para obtener host, puerto y rol/base
inspect() {
  # Devuelve JSON en línea; se parsea con jq si está disponible, o manualmente
  kling_cmd inspect "$COPY_NAME" -json 2>/dev/null || echo ""
}

# Obtiene DATABASE_URL de la copia.
get_database_url() {
  # Lee el DSN de la copia (clave nunca en stdout salvo aquí y a un fichero 0600)
  kling_cmd db connect "$COPY_NAME" -dsn 2>/dev/null || return 1
}

# Crea o resetea la copia
setup_copy() {
  log "checking $COPY_NAME..."
  json="$(inspect)"
  if [ -n "$json" ]; then
    log "$COPY_NAME exists: resetting..."
    if ! kling_cmd db reset "$COPY_NAME" -ttl "$TTL" >/dev/null 2>&1; then
      die "failed to reset $COPY_NAME"
    fi
  else
    log "creating $COPY_NAME from $GOLDEN..."
    if ! kling_cmd db up "$GOLDEN" -name "$COPY_NAME" -ttl "$TTL" >/dev/null 2>&1; then
      die "failed to create $COPY_NAME"
    fi
  fi
  # Espera a que esté ready
  retry=30
  while [ $retry -gt 0 ]; do
    json="$(inspect)"
    if [ -n "$json" ] && echo "$json" | grep -q '"kling.db.state":"ready"'; then
      log "$COPY_NAME is ready"
      return 0
    fi
    sleep 1
    retry=$((retry - 1))
  done
  die "timeout waiting for $COPY_NAME to be ready"
}

# Obtiene el DSN de la copia y lo guarda en un fichero 0600
setup_db_env() {
  log "obtaining DATABASE_URL..."
  local dsn
  dsn="$(get_database_url)" || die "failed to get DATABASE_URL from $COPY_NAME"

  # Crea fichero temporal con permisos 0600
  DB_ENV_FILE="$(mktemp)"
  chmod 0600 "$DB_ENV_FILE"
  echo "export DATABASE_URL='$dsn'" > "$DB_ENV_FILE"
  # En CI: el fichero se sobreescribe en el export, nunca aparece en logs
}

# ── Main ─────────────────────────────────────────────────────────────────────

if [ $# -lt 1 ]; then
  die "no command given (use -- before the command)"
fi

log "ci-pr-db: PR=$PR GOLDEN=$GOLDEN TTL=$TTL"

setup_copy
setup_db_env

# Exporta DATABASE_URL en el entorno del comando, nunca en stdout
export DATABASE_URL
DATABASE_URL="$(cat "$DB_ENV_FILE" | grep -oP "(?<=DATABASE_URL=').*(?=')" || true)"
[ -n "$DATABASE_URL" ] || die "DATABASE_URL is empty"

log "running: $*"
"$@"
rc=$?
log "command exited with $rc"
exit $rc
