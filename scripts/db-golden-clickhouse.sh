#!/usr/bin/env bash
# kling db, ClickHouse (#118): un ClickHouse CALIENTE congelado como plantilla.
#
#   scripts/db-golden-clickhouse.sh image [-tag 25.8]      importa la imagen clickhouse
#   scripts/db-golden-clickhouse.sh build [opciones] <nombre> arranca, carga y congela
#
# (o db-golden.sh image|build -engine clickhouse ..., que llega aquí.)
#
# La imagen es la oficial de Docker (clickhouse/clickhouse-server, glibc),
# importada sin Docker con kling image import; el servidor lo arranca build.
# `build` arranca una microVM desde ella (sin red, con exec), configura y
# arranca clickhouse-server escuchando en la IP del invitado y en el loopback,
# crea la base y el usuario de la aplicación, carga datos de prueba si se
# piden y guarda la máquina VIVA como plantilla <nombre> con la etiqueta
# kling.db.engine=clickhouse: `kling db up <nombre>` da copias con el servidor
# ya caliente. Método y límites: docs/db-engines.md.
#
# Opciones de build:
#   -seed-rows N      N filas sintéticas en <base>.seed_events
#   -role U           usuario de la aplicación (app)  -database B   base (appdb)
#   -image I          imagen (clickhouse)             -mem M        RAM de la VM (1G)
#   -cpus N           vCPUs (2)                       -state DIR    dónde va la contraseña
#   -keep             no borrar la máquina de preparación tras guardar
#
# LA CONTRASEÑA del usuario de la aplicación se genera aquí y se guarda SOLO
# en el host ($STATE/<nombre>/password, 0600). Al invitado va su SHA-256 (lo
# que ClickHouse guarda con IDENTIFIED WITH sha256_hash), nunca la clave.
#
# EL ADMINISTRADOR es el usuario default, sin clave y solo desde el loopback
# del invitado (users.d): quien llega ahí ya es root dentro. Lo usan kling db
# (rotación) y doctor. display_secrets_in_show_and_select está encendido para
# que SHOW CREATE USER enseñe el hash guardado y se pueda comprobar.
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

die() { echo "db-golden-clickhouse: $*" >&2; exit 1; }
say() { echo "db-golden-clickhouse: $*"; }

SEED_ROWS_MAX=50000000

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

# sha256_file <fichero>: el SHA-256 en hexadecimal (minúsculas) de su contenido.
sha256_file() {
  local h
  if command -v sha256sum >/dev/null 2>&1; then
    h="$(sha256sum < "$1" | cut -d' ' -f1)"
  elif command -v shasum >/dev/null 2>&1; then
    h="$(shasum -a 256 < "$1" | cut -d' ' -f1)"
  elif command -v openssl >/dev/null 2>&1; then
    h="$(openssl dgst -sha256 -r < "$1" | cut -d' ' -f1)"
  else
    die "hace falta sha256sum, shasum u openssl en el host para calcular el hash de la contraseña"
  fi
  [[ "$h" =~ ^[0-9a-f]{64}$ ]] || die "no se pudo calcular el hash de la contraseña"
  printf '%s' "$h"
}

# El cliente del administrador dentro de la máquina; las consultas por stdin.
CH='exec clickhouse-client --host 127.0.0.1 --multiquery'

cmd_image() {
  local tag=25.8
  if [ "${1:-}" = -tag ]; then tag="${2:?falta valor}"; fi
  [[ "$tag" =~ ^[0-9]+(\.[0-9]+){0,3}$ ]] || die "-tag no válido: $tag (25.8, 24.8...)"
  say "importando clickhouse/clickhouse-server:$tag como clickhouse (el servidor lo arranca build)"
  "${K[@]}" image import "clickhouse/clickhouse-server:$tag" -name clickhouse -replace -entrypoint sleep -- infinity
}

cmd_build() {
  local seed_rows=0 role=app db=appdb image=clickhouse
  local mem=1G cpus=2 state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}" name=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -seed-rows)  seed_rows="${2:?falta valor}"; shift 2 ;;
      -role)       role="${2:?falta valor}"; shift 2 ;;
      -database)   db="${2:?falta valor}"; shift 2 ;;
      -image)      image="${2:?falta valor}"; shift 2 ;;
      -mem)        mem="${2:?falta valor}"; shift 2 ;;
      -cpus)       cpus="${2:?falta valor}"; shift 2 ;;
      -state)      state="${2:?falta valor}"; shift 2 ;;
      -keep)       BUILD_KEEP=1; shift ;;
      -migrations|-seed|-seed-mb|-as-super|-template|-from)
                   die "$1 no existe para ClickHouse: usa -seed-rows, o carga los datos en una copia y guárdala" ;;
      -*)          die "opción desconocida: $1" ;;
      *)           [ -z "$name" ] || die "sobra un argumento: $1"; name="$1"; shift ;;
    esac
  done
  [ -n "$name" ] || die "uso: db-golden-clickhouse.sh build [opciones] <nombre>"

  # Todo lo que acaba en SQL o en una línea de shell se valida aquí.
  [[ "$name" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "nombre no válido: $name"
  [[ "$role" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "usuario no válido: $role"
  [[ "$db" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "base no válida: $db"
  [ "$role" != default ] || die "el usuario de la aplicación no puede ser default (es el administrador)"
  case "$db" in system|default|information_schema|INFORMATION_SCHEMA) die "la base de la aplicación no puede ser $db" ;; esac
  [[ "$image" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "imagen no válida: $image"
  [[ "$mem" =~ ^[0-9]+[MG]?$ ]] || die "-mem no válido: $mem"
  [[ "$cpus" =~ ^[0-9]+$ ]] || die "-cpus no válido: $cpus"
  [[ "$seed_rows" =~ ^[0-9]+$ ]] || die "-seed-rows no válido: $seed_rows"
  [ "$seed_rows" -le "$SEED_ROWS_MAX" ] || die "-seed-rows máximo $SEED_ROWS_MAX"

  local m="$name-build" sdir="$state/$name"
  mkdir -p "$sdir"
  chmod 700 "$state" "$sdir"
  BUILD_TMP="$(mktemp -d)"
  BUILD_M="$m"
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  "${K[@]}" rm -f "$m" >/dev/null 2>&1 || true

  # Contraseña: 192 bits en hex, a un fichero 0600; al invitado, su SHA-256.
  od -An -tx1 -N24 /dev/urandom | tr -d ' \n' > "$BUILD_TMP/password"
  [ "$(wc -c < "$BUILD_TMP/password" | tr -d ' ')" -eq 48 ] || die "no se pudo generar la contraseña"
  local hash
  hash="$(sha256_file "$BUILD_TMP/password")"

  local labels=(-label "kling.db.engine=clickhouse" -label "kling.db.role=$role" -label "kling.db.database=$db")
  say "arrancando $m desde la imagen $image (egress none)"
  "${K[@]}" run -name "$m" -image "$image" -egress none -allow-exec \
    -cpus "$cpus" -mem "$mem" -cpu-pct 100 "${labels[@]}" >/dev/null

  local i
  for i in $(seq 1 60); do
    if "${K[@]}" exec -timeout 5s "$m" -- true >/dev/null 2>&1; then break; fi
    [ "$i" -lt 60 ] || die "el agente del invitado no contesta"
    sleep 1
  done

  local cmdline ip
  cmdline="$("${K[@]}" exec "$m" -- cat /proc/cmdline)"
  ip="$(printf '%s' "$cmdline" | tr ' ' '\n' | sed -n 's/^ip=\([0-9.]*\)::.*/\1/p' | head -n1)"
  [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "no encuentro la IP del invitado en /proc/cmdline"

  say "configuración y arranque de clickhouse-server en $ip"
  # Los datos van en la raíz de la máquina (overlay), NO en un volumen: Freeze
  # desmonta los volúmenes y Fork rechaza los de escritura.
  "${K[@]}" exec -i -timeout 5m "$m" -- sh -s <<SETUP
set -eu
C=/etc/clickhouse-server
# La de la imagen escucha en todas las direcciones: fuera.
rm -f \$C/config.d/docker_related_config.xml
cat > \$C/config.d/zz-kling-db.xml <<'XML'
<clickhouse>
  <listen_host>127.0.0.1</listen_host>
  <listen_host>$ip</listen_host>
  <display_secrets_in_show_and_select>1</display_secrets_in_show_and_select>
  <logger>
    <level>warning</level>
    <log>/var/log/clickhouse-server/clickhouse-server.log</log>
    <errorlog>/var/log/clickhouse-server/clickhouse-server.err.log</errorlog>
  </logger>
</clickhouse>
XML
# default: el administrador, sin clave, solo desde el propio invitado.
cat > \$C/users.d/zz-kling-db.xml <<'XML'
<clickhouse>
  <users>
    <default>
      <networks replace="replace"><ip>127.0.0.1</ip><ip>::1</ip></networks>
      <access_management>1</access_management>
      <show_named_collections_secrets>0</show_named_collections_secrets>
    </default>
  </users>
</clickhouse>
XML
mkdir -p /var/log/clickhouse-server /var/lib/clickhouse
chown -R clickhouse:clickhouse /var/log/clickhouse-server /var/lib/clickhouse \$C
su -s /bin/sh clickhouse -c "clickhouse-server --config-file=\$C/config.xml --daemon --pid-file=/var/lib/clickhouse/server.pid"
i=0
until clickhouse-client --host 127.0.0.1 --query "SELECT 1" >/dev/null 2>&1; do
  i=\$((i + 1))
  [ "\$i" -lt 90 ] || { tail -n 30 /var/log/clickhouse-server/clickhouse-server.err.log 2>/dev/null; exit 1; }
  sleep 1
done
SETUP

  say "base $db y usuario $role (al invitado va el hash)"
  if ! "${K[@]}" exec -i "$m" -- sh -c "$CH" >/dev/null 2>&1 <<SQL
CREATE DATABASE $db;
CREATE USER $role IDENTIFIED WITH sha256_hash BY '$hash' HOST ANY;
GRANT ALL ON $db.* TO $role;
SQL
  then
    die "no se pudo crear la base o el usuario (salida omitida)"
  fi
  local guardado
  # El hash que guarda (ClickHouse lo enseña en mayúsculas), comparado con el nuestro.
  guardado="$("${K[@]}" exec -i "$m" -- sh -c "$CH" <<<"SHOW CREATE USER $role SETTINGS format_display_secrets_in_show_and_select = 1 FORMAT TSVRaw;" |
    sed -n "s/.*sha256_hash BY '\([0-9A-Fa-f]*\)'.*/\1/p" | tr 'A-F' 'a-f')"
  [ "$guardado" = "$hash" ] || die "el usuario no quedó con su hash"

  if [ "$seed_rows" -gt 0 ]; then
    say "cargando $seed_rows filas sintéticas en $db.seed_events"
    quiet "${K[@]}" exec -i -timeout 30m "$m" -- sh -c "$CH" <<SQL
CREATE TABLE $db.seed_events (id UInt64, account UInt32, payload String, created_at DateTime)
  ENGINE = MergeTree ORDER BY (account, id);
INSERT INTO $db.seed_events
  SELECT number, number % 1000, repeat(hex(MD5(toString(number))), 3), toDateTime('2026-01-01 00:00:00') - (number % 86400)
  FROM numbers($seed_rows);
OPTIMIZE TABLE $db.seed_events FINAL;
SQL
  fi

  # Un dorado no puede llevar consultas en marcha de nadie más.
  local running
  running="$("${K[@]}" exec -i "$m" -- sh -c "$CH" <<<"SELECT count() FROM system.processes WHERE query_id <> queryID();")"
  [ "$running" = 0 ] || die "quedan $running consultas en marcha; no se congela"
  quiet "${K[@]}" exec -i "$m" -- sh -c "$CH" <<<"SYSTEM FLUSH LOGS;"

  say "guardando la máquina viva como plantilla $name"
  "${K[@]}" save -replace -warm=false "$m" "$name"

  install -m 600 "$BUILD_TMP/password" "$sdir/password.new"
  mv -f "$sdir/password.new" "$sdir/password"
  {
    echo "ENGINE=clickhouse"
    echo "DBUSER=$role"
    echo "DBNAME=$db"
    echo "GUEST_IP=$ip"
    echo "TEMPLATE=$name"
  } > "$sdir/conn.env.new"
  mv -f "$sdir/conn.env.new" "$sdir/conn.env"

  BUILD_OK=1
  say "listo: plantilla $name (clickhouse)"
  say "  contraseña: $sdir/password (0600, solo en este host)"
  say "  una copia:  kling db up $name -name <copia>   ·   kling db connect <copia> -dsn"
}

case "${1:-}" in
  image) shift; cmd_image "$@" ;;
  build) shift; cmd_build "$@" ;;
  *) sed -n '2,34p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
