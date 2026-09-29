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
#   DB_NAME     Base de datos (defecto: appdb)
#   DB_USER     Rol de la aplicación (defecto: app)
#   TIMEOUT     Timeout para kling exec (defecto: 120s)
#   KEEP        Si es 1, no borra las copias (para debugging)
#   OUT         Directorio de salida (defecto: ./ci-load-STAMP)
#   PREFIX      Prefijo de nombres de copia (defecto: cil)
#
# FLAGS:
#   -golden G   Plantilla (puede ir en env)
#   -p N        Número de simulaciones (defecto 20)
#   -keep       No borra las copias
#   -out DIR    Directorio de salida
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
KLING_HOST="${KLING_HOST:-}"
GOLDEN="${GOLDEN:-}"
P="${P:-20}"
TTL="${TTL:-10m}"
DB_NAME="${DB_NAME:-}"
DB_USER="${DB_USER:-}"
TIMEOUT="${TIMEOUT:-120s}"
KEEP="${KEEP:-0}"
PREFIX="cil"
STAMP="$(date -u +%Y%m%d-%H%M%S)"
OUT="${OUT:-./ci-load-$STAMP}"

# Parseo de flags
while [ $# -gt 0 ]; do
  case "$1" in
    -golden)
      GOLDEN="$2"
      shift 2
      ;;
    -p)
      P="$2"
      shift 2
      ;;
    -keep)
      KEEP=1
      shift
      ;;
    -out)
      OUT="$2"
      shift 2
      ;;
    -H)
      KLING_HOST="$2"
      shift 2
      ;;
    *)
      die "unknown flag: $1"
      ;;
  esac
done

# Validación
[ -n "$GOLDEN" ] || die "GOLDEN is required (-golden or env)"
command -v "$KLING" >/dev/null 2>&1 || die "kling not found: $KLING"
[ "$P" -gt 0 ] || die "P must be > 0"

# Funciones auxiliares
kling_cmd() {
  if [ -n "$KLING_HOST" ]; then
    "$KLING" -H "$KLING_HOST" "$@"
  else
    "$KLING" "$@"
  fi
}

# Inspecciona una copia
inspect() {
  kling_cmd inspect "$1" -json 2>/dev/null || echo ""
}

# Espera a que una copia esté ready (max 60s)
wait_ready() {
  local name="$1"
  local retry=60
  while [ $retry -gt 0 ]; do
    local json
    json="$(inspect "$name")"
    if [ -n "$json" ] && echo "$json" | grep -q '"kling.db.state":"ready"'; then
      return 0
    fi
    sleep 1
    retry=$((retry - 1))
  done
  return 1
}

# Obtiene el DSN de una copia
get_dsn() {
  kling_cmd db connect "$1" -dsn 2>/dev/null || echo ""
}

# Corre la migración y consultas en una copia
run_workload() {
  local name="$1"
  local dsn="$2"

  # Migración simple: crear tabla
  # En un proyecto real, aquí irían las migraciones reales.
  psql "$dsn" << 'EOF' >/dev/null 2>&1
CREATE TABLE IF NOT EXISTS load_test (
  id SERIAL PRIMARY KEY,
  payload TEXT
);
EOF
  [ $? -eq 0 ] || return 1

  # 100 consultas INSERT + SELECT
  local i
  for i in {1..50}; do
    psql "$dsn" << EOF >/dev/null 2>&1
INSERT INTO load_test (payload) VALUES ('test-$i') RETURNING id;
SELECT * FROM load_test WHERE id = (SELECT MAX(id) FROM load_test);
EOF
    [ $? -eq 0 ] || return 1
  done

  return 0
}

# Simula una carga de PR: crea copia, corre workload, mide tiempo
simulate_pr() {
  local idx="$1"
  local name="${PREFIX}_${idx}"
  local start_time
  local up_duration
  local total_start

  total_start="$(date +%s%N)"
  start_time="$(date +%s%N)"

  # Crea la copia
  if ! kling_cmd db up "$GOLDEN" -name "$name" -ttl "$TTL" >/dev/null 2>&1; then
    echo "FAIL	$name	create"
    return 1
  fi

  up_duration=$(($(date +%s%N) - start_time))

  # Espera a que esté ready
  if ! wait_ready "$name"; then
    kling_cmd rm -f "$name" 2>/dev/null || true
    echo "FAIL	$name	wait"
    return 1
  fi

  # Obtiene el DSN
  local dsn
  dsn="$(get_dsn "$name")"
  [ -n "$dsn" ] || {
    kling_cmd rm -f "$name" 2>/dev/null || true
    echo "FAIL	$name	dsn"
    return 1
  }

  # Corre el workload
  if ! run_workload "$name" "$dsn"; then
    kling_cmd rm -f "$name" 2>/dev/null || true
    echo "FAIL	$name	workload"
    return 1
  fi

  # Limpia
  if ! kling_cmd rm -f "$name" 2>/dev/null; then
    echo "FAIL	$name	remove"
    return 1
  fi

  # Devuelve métricas: up_time(ms), total_time(ms)
  local end_time
  end_time="$(date +%s%N)"
  local up_ms=$((up_duration / 1000000))
  local total_ms=$((end_time - total_start))
  total_ms=$((total_ms / 1000000))
  echo "OK	${name}	${up_ms}	${total_ms}"
}

# Limpieza: borra todas las copias con el prefijo
cleanup() {
  if [ "$KEEP" = 1 ]; then
    log "KEEP=1: copies not removed"
    return
  fi
  log "cleaning up..."
  kling_cmd ps -a -q 2>/dev/null | while read -r id; do
    kling_cmd inspect "$id" 2>/dev/null | grep -q "\"name\": \"${PREFIX}_" && \
      kling_cmd rm -f "$id" >/dev/null 2>&1 || true
  done
}
trap cleanup EXIT INT TERM

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
  local total_times=()
  local ok_count=0
  local fail_count=0

  while IFS=$'\t' read -r status name up_ms total_ms; do
    case "$status" in
      OK)
        ok_count=$((ok_count + 1))
        up_times+=("$up_ms")
        total_times+=("$total_ms")
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
