#!/usr/bin/env bash
# kling db, SQLite (#65): una base SQLite dentro de una microVM, congelada como
# plantilla.
#
#   scripts/db-golden-sqlite.sh image                     construye la imagen sqlite
#   scripts/db-golden-sqlite.sh build [opciones] <nombre> crea, carga y congela
#
# (o db-golden.sh image|build -engine sqlite ..., que llega aquí.)
#
# SQLite no tiene servidor: la base es un fichero. El modelo honesto es que la
# copia es una microVM con /var/lib/kling-db/<base>.sqlite y el cliente
# sqlite3, y se entra con kling exec o kling shell (kling db connect -sqlite).
# No hay red ni contraseña que rotar: quien puede ejecutar en la máquina puede
# leer la base, y eso lo decide el daemon (el dueño de la máquina). Método y
# límites: docs/db-engines.md.
#
# Opciones de build:
#   -migrations DIR   ficheros *.sql, en orden alfabético
#   -seed FILE        SQL de datos, después de las migraciones
#   -seed-mb N        en vez de -seed: datos sintéticos de ~N MiB
#   -database B       nombre de la base (appdb): el fichero es B.sqlite
#   -image I          imagen (sqlite)                 -mem M        RAM de la VM (256M)
#   -from T           plantilla con sqlite3 instalado en vez de -image (macOS)
#   -cpus N           vCPUs (1)                       -state DIR    dónde va conn.env
#   -keep             no borrar la máquina de preparación tras guardar
#
# Entorno: KLING = comando kling (con sus banderas, p.ej. "kling -H ssh://lab").
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

die() { echo "db-golden-sqlite: $*" >&2; exit 1; }
say() { echo "db-golden-sqlite: $*"; }

SEED_MB_MAX=250

BUILD_M=""
BUILD_TMP=""
BUILD_OK=0
BUILD_KEEP=0

cleanup() {
  [ -z "$BUILD_TMP" ] || rm -rf "$BUILD_TMP"
  if [ "$BUILD_OK" -eq 1 ] && [ "$BUILD_KEEP" -eq 1 ]; then return; fi
  [ -z "$BUILD_M" ] || "${K[@]}" rm -f "$BUILD_M" >/dev/null 2>&1 || true
}

quiet() {
  local out
  out="$("$@" 2>&1)" || { printf '%s\n' "$out" >&2; return 1; }
}

cmd_image() {
  [ -f "$HERE/recipes/sqlite.recipe.json" ] || die "falta scripts/recipes/sqlite.recipe.json"
  say "construyendo la imagen sqlite (Alpine + sqlite); tarda unos minutos"
  "${K[@]}" image build sqlite -builder base -base min -grow 512 -spec "$HERE/recipes/sqlite.recipe.json"
  "${K[@]}" image recipe sqlite
}

cmd_build() {
  local migrations="" seed="" seed_mb=0 db=appdb image=sqlite from=""
  local mem=256M cpus=1 state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}" name=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -migrations) migrations="${2:?falta valor}"; shift 2 ;;
      -seed)       seed="${2:?falta valor}"; shift 2 ;;
      -seed-mb)    seed_mb="${2:?falta valor}"; shift 2 ;;
      -database)   db="${2:?falta valor}"; shift 2 ;;
      -image)      image="${2:?falta valor}"; shift 2 ;;
      -from)       from="${2:?falta valor}"; shift 2 ;;
      -mem)        mem="${2:?falta valor}"; shift 2 ;;
      -cpus)       cpus="${2:?falta valor}"; shift 2 ;;
      -state)      state="${2:?falta valor}"; shift 2 ;;
      -keep)       BUILD_KEEP=1; shift ;;
      -as-super)   shift ;; # no hay roles en SQLite
      -role)       die "-role no existe para SQLite: no hay usuarios ni contraseña" ;;
      -*)          die "opción desconocida: $1" ;;
      *)           [ -z "$name" ] || die "sobra un argumento: $1"; name="$1"; shift ;;
    esac
  done
  [ -n "$name" ] || die "uso: db-golden-sqlite.sh build [opciones] <nombre>"

  # Todo lo que acaba en una ruta o en una línea de shell se valida aquí.
  [[ "$name" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "nombre no válido: $name"
  [[ "$db" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "base no válida: $db"
  [[ "$image" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "imagen no válida: $image"
  [ -z "$from" ] || [[ "$from" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "plantilla no válida: $from"
  [[ "$mem" =~ ^[0-9]+[MG]?$ ]] || die "-mem no válido: $mem"
  [[ "$cpus" =~ ^[0-9]+$ ]] || die "-cpus no válido: $cpus"
  [[ "$seed_mb" =~ ^[0-9]+$ ]] || die "-seed-mb no válido: $seed_mb"
  [ "$seed_mb" -le "$SEED_MB_MAX" ] || die "-seed-mb máximo $SEED_MB_MAX: el overlay de la máquina es de 512 MiB"
  [ -z "$seed" ] || [ "$seed_mb" -eq 0 ] || die "-seed y -seed-mb son excluyentes"
  [ -z "$seed" ] || [ -f "$seed" ] || die "no existe el seed: $seed"
  local files=() f
  if [ -n "$migrations" ]; then
    [ -d "$migrations" ] || die "no existe el directorio de migraciones: $migrations"
    while IFS= read -r f; do files+=("$f"); done < <(find "$migrations" -maxdepth 1 -type f -name '*.sql' | LC_ALL=C sort)
    [ "${#files[@]}" -gt 0 ] || die "no hay ficheros .sql en $migrations"
  fi
  [ -z "$seed" ] || files+=("$seed")

  local m="$name-build" sdir="$state/$name" path="/var/lib/kling-db/$db.sqlite"
  mkdir -p "$sdir"
  chmod 700 "$state" "$sdir"
  BUILD_TMP="$(mktemp -d)"
  BUILD_M="$m"
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  "${K[@]}" rm -f "$m" >/dev/null 2>&1 || true

  # La etiqueta del motor viaja con la plantilla y la heredan las copias.
  local labels=(-label "kling.db.engine=sqlite" -label "kling.db.database=$db")
  if [ -n "$from" ]; then
    say "arrancando $m desde la plantilla $from (egress none)"
    "${K[@]}" run -name "$m" -from "$from" -egress none -allow-exec "${labels[@]}" >/dev/null
  else
    say "arrancando $m desde la imagen $image (egress none)"
    "${K[@]}" run -name "$m" -image "$image" -egress none -allow-exec \
      -cpus "$cpus" -mem "$mem" -cpu-pct 100 "${labels[@]}" >/dev/null
  fi

  local i
  for i in $(seq 1 60); do
    if "${K[@]}" exec -timeout 5s "$m" -- true >/dev/null 2>&1; then break; fi
    [ "$i" -lt 60 ] || die "el agente del invitado no contesta"
    sleep 1
  done

  say "base $path en el overlay"
  # En la raíz de la máquina (overlay), NO en un volumen: Freeze desmonta los
  # volúmenes y Fork rechaza los de escritura. El diario es el de siempre
  # (DELETE): sin -wal ni -shm que congelar a medias ni que estorben a una
  # lectura -readonly.
  "${K[@]}" exec -i "$m" -- sh -s <<SETUP
set -eu
command -v sqlite3 >/dev/null || { echo "no hay sqlite3 en la imagen"; exit 1; }
install -d -m 0700 /var/lib/kling-db
umask 077
printf 'PRAGMA user_version;\n' | sqlite3 -bail "$path" >/dev/null
chmod 600 "$path"
SETUP

  if [ "$seed_mb" -gt 0 ]; then
    # ~330 bytes por fila con su índice.
    local rows=$(( seed_mb * 1048576 / 330 ))
    cat > "$BUILD_TMP/seed-synth.sql" <<SQL
CREATE TABLE seed_events (
  id INTEGER PRIMARY KEY,
  account INTEGER NOT NULL,
  payload TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX seed_events_account_idx ON seed_events (account);
WITH RECURSIVE s(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM s WHERE n < $rows)
INSERT INTO seed_events SELECT n, n % 1000, hex(randomblob(96)), datetime('2026-01-01', '-' || (n % 86400) || ' seconds') FROM s;
SQL
    files+=("$BUILD_TMP/seed-synth.sql")
  fi

  local n=0 dest
  "${K[@]}" exec "$m" -- mkdir -p /var/lib/dbgolden/in
  for f in "${files[@]+"${files[@]}"}"; do
    n=$((n + 1))
    dest="/var/lib/dbgolden/in/$(printf '%03d' "$n").sql"
    say "aplicando $(basename "$f")"
    quiet "${K[@]}" cp "$f" "$m:$dest"
    quiet "${K[@]}" exec -timeout 30m "$m" -- sh -c "sqlite3 -bail $path < $dest"
  done

  say "ANALYZE y comprobaciones antes de congelar"
  quiet "${K[@]}" exec -i -timeout 30m "$m" -- sqlite3 -bail "$path" <<<"ANALYZE;"
  local check
  check="$("${K[@]}" exec -i -timeout 30m "$m" -- sqlite3 -bail -readonly "$path" <<<"PRAGMA quick_check;")"
  [ "$check" = ok ] || die "PRAGMA quick_check: $check"

  local usage size
  usage="$("${K[@]}" exec "$m" -- sh -c "df -P / | awk 'NR==2 {gsub(\"%\",\"\",\$5); print \$5}'")"
  [[ "$usage" =~ ^[0-9]+$ ]] || die "no se pudo leer el uso del disco"
  [ "$usage" -lt 85 ] || die "el overlay está al ${usage}%: no queda margen para las copias"
  size="$("${K[@]}" exec "$m" -- sh -c "du -k $path | cut -f1")"
  say "base $db: ${size} KiB · disco del overlay al ${usage}%"

  quiet "${K[@]}" exec "$m" -- sh -c 'rm -rf /var/lib/dbgolden; sync'

  say "guardando la máquina como plantilla $name"
  "${K[@]}" save -replace -warm=false "$m" "$name"

  {
    echo "ENGINE=sqlite"
    echo "DBNAME=$db"
    echo "TEMPLATE=$name"
  } > "$sdir/conn.env.new"
  mv -f "$sdir/conn.env.new" "$sdir/conn.env"

  BUILD_OK=1
  say "listo: plantilla $name (sqlite, $path)"
  say "  una copia:  kling db up $name -name <copia>"
}

case "${1:-}" in
  image) shift; cmd_image "$@" ;;
  build) shift; cmd_build "$@" ;;
  *) sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
