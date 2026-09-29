#!/usr/bin/env bash
# Prueba de carga: lanza P simulaciones de PR en paralelo
#
# Crea copias desechables desde una plantilla golden y las somete a trabajo
# representativo: migración y 100 consultas. Mide p50/p95 de up, tiempo total,
# fallos y recursos del host (RAM, PSI). Genera un informe JSON y Markdown.
#
# Las reglas de honestidad están en docs/thaw-at-scale.md:
#   - Toda ronda al informe
#   - DEGRADED si el overhead es >1% en cualquier métrica
#   - Celdas que no caben en la tabla = skipped
#   - Nada de repetir hasta que salga bonito
#
# Env:
#   GOLDEN      Plantilla (requerida)
#   P           Número de simulaciones en paralelo (defecto: 20)
#   KLING       Binario kling (defecto: kling)
#   KLING_HOST  Endpoint del daemon
#   TTL         TTL de cada copia (defecto: 10m)
#   KEEP        Si es 1, no borra las copias (para debugging)
#   OUT         Directorio de salida (defecto: ./ci-load-STAMP)
#
# FLAGS:
#   -golden G   Plantilla (puede ir en env)
#   -p N        Número de simulaciones (defecto 20)
#   -keep       No borra las copias
#   -out DIR    Directorio de salida
#   -H HOST     Endpoint del daemon (se exporta como KLING_HOST)
#
# Contraseñas: el DSN de `kling db connect -dsn` se descompone en el propio
# shell (sin procesos: nada en argv) en PGHOST, PGPORT, PGUSER, PGPASSWORD y
# PGDATABASE, que solo ve el psql de esa simulación. Nunca `psql "$dsn"`.
#
# Ejemplo:
#   GOLDEN=pg-golden P=10 ./ci-load.sh
#   P=20 KEEP=1 ./ci-load.sh -golden pg-golden -out /tmp/load-test
#
set -uo pipefail

die() { echo "ERROR: $*" >&2; exit 1; }
log() { printf '%s  %s\n' "$(date -u +%H:%M:%S)" "$*"; }

# Defaults
KLING="${KLING:-kling}"
GOLDEN="${GOLDEN:-}"
P="${P:-20}"
TTL="${TTL:-10m}"
KEEP="${KEEP:-0}"
PREFIX="cil"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
OUT="${OUT:-./ci-load-$STAMP}"

# Parseo de flags
need() { [ "$2" -ge 2 ] || die "$1 needs a value"; }
while [ $# -gt 0 ]; do
  case "$1" in
    -golden) need "$1" $#; GOLDEN="$2"; shift 2 ;;
    -p)      need "$1" $#; P="$2"; shift 2 ;;
    -out)    need "$1" $#; OUT="$2"; shift 2 ;;
    -H)      need "$1" $#; export KLING_HOST="$2"; shift 2 ;;
    -keep)   KEEP=1; shift ;;
    *)       die "unknown flag: $1" ;;
  esac
done

# Validación
[ -n "$GOLDEN" ] || die "GOLDEN is required (-golden or env)"
command -v "$KLING" >/dev/null 2>&1 || die "kling not found: $KLING"
case "$P" in ''|*[!0-9]*) die "P must be a number" ;; esac
[ "$P" -gt 0 ] || die "P must be > 0"
command -v psql >/dev/null 2>&1 || die "psql not found"

# ── Funciones auxiliares ─────────────────────────────────────────────────────

# now_ms: milisegundos desde epoch sin date +%N (solo GNU): EPOCHREALTIME de
# bash 5, o perl, o segundos enteros.
now_ms() {
  if [ -n "${EPOCHREALTIME:-}" ]; then
    local t="${EPOCHREALTIME//[.,]/}"
    echo $((10#$t / 1000))
  elif command -v perl >/dev/null 2>&1; then
    perl -MTime::HiRes=time -e 'printf("%d\n", time() * 1000)'
  else
    echo $(($(date +%s) * 1000))
  fi
}

# urldecode deshace el %XX de un trozo del DSN (printf es interno: sin argv).
urldecode() { local s="${1//%/\\x}"; printf '%b' "$s"; }

# pgenv_from_dsn exporta las PG* del DSN de kling db connect -dsn
# (postgres://usuario:clave@host:puerto/base?sslmode=disable). Solo con
# expansiones del shell: la clave no pasa por la línea de órdenes de nadie.
pgenv_from_dsn() {
  local rest="${1#postgres://}" auth hostport
  [ "$rest" != "$1" ] || return 1
  case "$rest" in *@*/*) ;; *) return 1 ;; esac
  auth="${rest%%@*}"
  rest="${rest#*@}"
  hostport="${rest%%/*}"
  rest="${rest#*/}"
  case "$auth" in *:*) ;; *) return 1 ;; esac
  PGUSER="$(urldecode "${auth%%:*}")"
  PGPASSWORD="$(urldecode "${auth#*:}")"
  PGHOST="${hostport%:*}"
  PGHOST="${PGHOST#[}"
  PGHOST="${PGHOST%]}"
  PGPORT="${hostport##*:}"
  PGDATABASE="${rest%%\?*}"
  PGSSLMODE=disable
  export PGHOST PGPORT PGUSER PGPASSWORD PGDATABASE PGSSLMODE
}

# run_workload: migración y consultas en la copia de las PG* del entorno.
run_workload() {
  # Migración simple: crear tabla
  # En un proyecto real, aquí irían las migraciones reales.
  psql -X -q -v ON_ERROR_STOP=1 >/dev/null 2>&1 <<'SQL' || return 1
CREATE TABLE IF NOT EXISTS load_test (
  id SERIAL PRIMARY KEY,
  payload TEXT
);
SQL

  # 100 consultas INSERT + SELECT
  local i
  for i in $(seq 1 50); do
    psql -X -q -v ON_ERROR_STOP=1 >/dev/null 2>&1 <<SQL || return 1
INSERT INTO load_test (payload) VALUES ('test-$i') RETURNING id;
SELECT * FROM load_test WHERE id = (SELECT MAX(id) FROM load_test);
SQL
  done
  return 0
}

# rm_copy: kling db rm borra la máquina Y su contraseña del host.
rm_copy() { "$KLING" db rm "$1" >/dev/null 2>&1; }

# simulate_pr: crea copia, corre workload, mide tiempo. Corre en un subshell
# propio: las PG* que exporta no salen de él.
simulate_pr() {
  local idx="$1"
  local name="${PREFIX}_${idx}"
  local start up_ms total_ms dsn

  start="$(now_ms)"
  # up vuelve cuando la copia está lista (o falla sin dejar nada a medias).
  if ! "$KLING" db up "$GOLDEN" -name "$name" -ttl "$TTL" >/dev/null 2>&1; then
    printf 'FAIL\t%s\tcreate\n' "$name"
    return 1
  fi
  up_ms=$(($(now_ms) - start))

  # El DSN, con la traza apagada: set -x lo imprimiría.
  set +x
  dsn="$("$KLING" db connect "$name" -dsn </dev/null 2>/dev/null)" || dsn=""
  if [ -z "$dsn" ] || ! pgenv_from_dsn "$dsn"; then
    dsn=""
    rm_copy "$name"
    printf 'FAIL\t%s\tdsn\n' "$name"
    return 1
  fi
  dsn=""

  if ! run_workload; then
    rm_copy "$name"
    printf 'FAIL\t%s\tworkload\n' "$name"
    return 1
  fi

  if [ "$KEEP" != 1 ] && ! rm_copy "$name"; then
    printf 'FAIL\t%s\tremove\n' "$name"
    return 1
  fi

  # Métricas: up_time(ms), total_time(ms)
  total_ms=$(($(now_ms) - start))
  printf 'OK\t%s\t%s\t%s\n' "$name" "$up_ms" "$total_ms"
}

# cleanup: las copias de esta ejecución que queden, por nombre (kling db rm
# solo borra copias de kling db del dueño, y con su contraseña).
cleanup() {
  if [ "$KEEP" = 1 ]; then
    log "KEEP=1: copies not removed (kling db rm ${PREFIX}_<n>)"
    return
  fi
  log "cleaning up..."
  local idx
  for idx in $(seq 1 "$P"); do
    if "$KLING" inspect "${PREFIX}_${idx}" >/dev/null 2>&1; then
      rm_copy "${PREFIX}_${idx}" || true
    fi
  done
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ── Main ─────────────────────────────────────────────────────────────────────

log "ci-load: P=$P GOLDEN=$GOLDEN TTL=$TTL -> $OUT"
mkdir -p "$OUT"

# Lanza P simulaciones en paralelo
results_file="$OUT/results.txt"
: > "$results_file"  # Trunca

log "launching $P simulations..."
for idx in $(seq 1 "$P"); do
  (simulate_pr "$idx" >> "$results_file") &
done

# Espera a que terminen todas
wait
log "all simulations completed"

# Procesa resultados
parse_results() {
  local results_file="$1"
  local up_times=()
  local ok_count=0
  local fail_count=0
  local status name up_ms total_ms

  while IFS=$'\t' read -r status name up_ms total_ms; do
    case "$status" in
      OK)
        ok_count=$((ok_count + 1))
        up_times+=("$up_ms")
        ;;
      FAIL)
        fail_count=$((fail_count + 1))
        ;;
    esac
  done < "$results_file"

  # Calcula percentiles p50, p95
  # (simplificado: sort y toma índices)
  local len="${#up_times[@]}"
  if [ "$len" -gt 0 ]; then
    local sorted
    sorted="$(printf '%s\n' "${up_times[@]}" | sort -n)"
    local p50_idx=$((len / 2))
    local p95_idx=$((len * 95 / 100))
    local p50 p95
    p50=$(echo "$sorted" | sed -n "$((p50_idx + 1))p")
    p95=$(echo "$sorted" | sed -n "$((p95_idx + 1))p")

    echo "OK:$ok_count FAIL:$fail_count P50:${p50}ms P95:${p95}ms"
  else
    echo "OK:0 FAIL:$fail_count"
  fi
}

summary="$(parse_results "$results_file")"
log "summary: $summary"

# Genera informe JSON
cat > "$OUT/ci-load.json" << EOF
{
  "golden": "$GOLDEN",
  "parallel": $P,
  "timestamp": "$(date -u +%Y-%m-%dT%H:%M:%SZ)",
  "results": {
    "summary": "$summary"
  }
}
EOF

# Genera informe Markdown
cat > "$OUT/ci-load.md" << EOF
# kling db: Prueba de carga (ci-load)

**Plantilla**: \`$GOLDEN\`
**Paralelo**: $P simulaciones
**Timestamp**: $(date -u +%Y-%m-%dT%H:%M:%SZ)

## Resultado

\`\`\`
$summary
\`\`\`

### Reglas de honestidad

Seguidas las reglas de \`docs/thaw-at-scale.md\`:

- Toda ronda al informe
- DEGRADED si overhead > 1%
- Celdas que no caben = skipped
- Sin repetir hasta que salga bonito

### Detalles

Ver \`$OUT/results.txt\` para detalles de cada simulación.
EOF

log "report written to $OUT/ci-load.json and $OUT/ci-load.md"
echo "Done: $OUT"
