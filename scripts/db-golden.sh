#!/usr/bin/env bash
# kling db, nodo N1: un Postgres CALIENTE congelado como plantilla dorada.
#
#   scripts/db-golden.sh image                     construye la imagen pg16
#   scripts/db-golden.sh build [opciones] <nombre> arranca, carga y congela
#   ... image|build -engine mysql ...             lo mismo con MariaDB (db-golden-mysql.sh)
#   ... image|build -engine redis|sqlite ...      Redis o SQLite (db-golden-redis.sh, -sqlite.sh)
#
# `build` arranca una microVM desde la imagen pg16 (sin red, con exec), crea el
# cluster en el overlay de la máquina, arranca Postgres escuchando en la IP del
# invitado con SCRAM, aplica migraciones y seed, hace CHECKPOINT y guarda la
# máquina VIVA como plantilla <nombre> (kling save): `kling run -from <nombre>`
# devuelve una base con el proceso ya caliente. Método y límites: docs/db-golden.md.
#
# Opciones de build:
#   -migrations DIR   ficheros *.sql, en orden alfabético (como el rol de la app)
#   -seed FILE        SQL de datos, después de las migraciones
#   -seed-mb N        en vez de -seed: datos sintéticos de ~N MiB (generate_series)
#   -as-super         migraciones y seed como superusuario (CREATE EXTENSION...)
#   -role R           rol de la aplicación (app)     -database B   base (appdb)
#   -image I          imagen (pg16)                  -mem M        RAM de la VM (1G)
#   -from T           plantilla con Postgres instalado en vez de -image (macOS: ver docs/db-golden.md)
#   -cpus N           vCPUs (2)                      -state DIR    dónde va la contraseña
#   -keep             no borrar la máquina de preparación tras guardar
#
# Entorno: KLING = comando kling (con sus banderas, p.ej. "kling -H ssh://lab").
#
# La contraseña del rol se genera aquí, se guarda SOLO en el host
# ($STATE/<nombre>/password, 0600) y viaja al invitado por stdin: nunca en
# argv ni en pantalla.
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

die() { echo "db-golden: $*" >&2; exit 1; }
say() { echo "db-golden: $*"; }

# Cuánto cabe: el overlay por máquina es de 512 MiB (defaultOverlayMiB) y ahí
# viven el cluster (~40 MiB tras initdb), el WAL y los datos.
SEED_MB_MAX=250

# Estado del trap de limpieza. Globales a propósito: el trap de EXIT corre
# cuando las variables locales de cmd_build ya no existen.
BUILD_M=""
BUILD_TMP=""
BUILD_OK=0
BUILD_KEEP=0

cleanup() {
  [ -z "$BUILD_TMP" ] || rm -rf "$BUILD_TMP"
  if [ "$BUILD_OK" -eq 1 ] && [ "$BUILD_KEEP" -eq 1 ]; then return; fi
  [ -z "$BUILD_M" ] || "${K[@]}" rm -f "$BUILD_M" >/dev/null 2>&1 || true
}

# quiet corre un comando y solo enseña su salida si falla.
quiet() {
  local out
  out="$("$@" 2>&1)" || { printf '%s\n' "$out" >&2; return 1; }
}

cmd_image() {
  [ -f "$HERE/recipes/pg16.recipe.json" ] || die "falta scripts/recipes/pg16.recipe.json"
  say "construyendo la imagen pg16 (Alpine + postgresql16); tarda unos minutos"
  "${K[@]}" image build pg16 -builder base -base min -grow 512 -spec "$HERE/recipes/pg16.recipe.json"
  "${K[@]}" image recipe pg16
}

cmd_build() {
  local migrations="" seed="" seed_mb=0 as_super=0 role=app db=appdb image=pg16 from=""
  local mem=1G cpus=2 state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}" name=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -migrations) migrations="${2:?falta valor}"; shift 2 ;;
      -seed)       seed="${2:?falta valor}"; shift 2 ;;
      -seed-mb)    seed_mb="${2:?falta valor}"; shift 2 ;;
      -as-super)   as_super=1; shift ;;
      -role)       role="${2:?falta valor}"; shift 2 ;;
      -database)   db="${2:?falta valor}"; shift 2 ;;
      -image)      image="${2:?falta valor}"; shift 2 ;;
      -from)       from="${2:?falta valor}"; shift 2 ;;
      -mem)        mem="${2:?falta valor}"; shift 2 ;;
      -cpus)       cpus="${2:?falta valor}"; shift 2 ;;
      -state)      state="${2:?falta valor}"; shift 2 ;;
      -keep)       BUILD_KEEP=1; shift ;;
      -*)          die "opción desconocida: $1" ;;
      *)           [ -z "$name" ] || die "sobra un argumento: $1"; name="$1"; shift ;;
    esac
  done
  [ -n "$name" ] || die "uso: db-golden.sh build [opciones] <nombre>"

  # Todo lo que acaba dentro de SQL o de una línea de shell se valida aquí: son
  # identificadores simples, sin comillas que escapar.
  [[ "$name" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "nombre no válido: $name"
  [[ "$role" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "rol no válido: $role"
  [[ "$db" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "base no válida: $db"
  [ "$role" != postgres ] || die "el rol de la aplicación no puede ser postgres"
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
    # LC_ALL=C: el orden no puede depender del locale de quien lo lanza.
    while IFS= read -r f; do files+=("$f"); done < <(find "$migrations" -maxdepth 1 -type f -name '*.sql' | LC_ALL=C sort)
    [ "${#files[@]}" -gt 0 ] || die "no hay ficheros .sql en $migrations"
  fi
  [ -z "$seed" ] || files+=("$seed")

  local m="$name-build" sdir="$state/$name"
  mkdir -p "$sdir"
  chmod 700 "$state" "$sdir"
  BUILD_TMP="$(mktemp -d)"
  BUILD_M="$m"
  # Limpieza al salir, por éxito o por fallo. En fallo NO se toca la contraseña
  # anterior (solo se pisa al final) ni la plantilla vieja si no llegó a guardarse.
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  # Idempotencia: una máquina de preparación de un intento anterior se retira.
  "${K[@]}" rm -f "$m" >/dev/null 2>&1 || true

  # Contraseña: 192 bits en hex (sin caracteres que escapar). Va a un fichero
  # 0600 y no a una variable exportada.
  od -An -tx1 -N24 /dev/urandom | tr -d ' \n' > "$BUILD_TMP/password"
  [ "$(wc -c < "$BUILD_TMP/password" | tr -d ' ')" -eq 48 ] || die "no se pudo generar la contraseña"

  if [ -n "$from" ]; then
    # Desde una plantilla con Postgres ya instalado (p. ej. en macOS, donde no se
    # pueden construir imágenes: toolchain + apk add postgresql16, guardada con
    # kling save). La memoria y las CPU son las del snapshot; el egress se fuerza
    # a none aunque la plantilla tuviera salida.
    say "arrancando $m desde la plantilla $from (egress none)"
    "${K[@]}" run -name "$m" -from "$from" -egress none -allow-exec >/dev/null
  else
    say "arrancando $m desde la imagen $image (egress none)"
    "${K[@]}" run -name "$m" -image "$image" -egress none -allow-exec \
      -cpus "$cpus" -mem "$mem" -cpu-pct 100 >/dev/null
  fi

  local i
  for i in $(seq 1 60); do
    if "${K[@]}" exec -timeout 5s "$m" -- true >/dev/null 2>&1; then break; fi
    [ "$i" -lt 60 ] || die "el agente del invitado no contesta"
    sleep 1
  done

  # La IP del invitado sale de su línea de comandos (ip=A::GW:MASK::eth0:off):
  # es la misma en todas las máquinas y por eso el dorado sirve para todas.
  local cmdline ip
  cmdline="$("${K[@]}" exec "$m" -- cat /proc/cmdline)"
  ip="$(printf '%s' "$cmdline" | tr ' ' '\n' | sed -n 's/^ip=\([0-9.]*\)::.*/\1/p' | head -n1)"
  [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "no encuentro la IP del invitado en /proc/cmdline"

  say "initdb en el overlay y arranque de Postgres en $ip"
  # PGDATA va en la raíz de la máquina (overlay), NO en un volumen: Freeze
  # desmonta los volúmenes y Fork rechaza los de escritura (puedeRamificarse).
  "${K[@]}" exec -i -timeout 5m "$m" -- sh -s <<SETUP
set -eu
D=/var/lib/postgresql/data
mkdir -p /run/postgresql /var/log/postgresql /var/lib/dbgolden/in
chmod 755 /var/lib/dbgolden /var/lib/dbgolden/in
chown postgres:postgres /run/postgresql /var/log/postgresql
install -d -o postgres -g postgres -m 0700 "\$D"
su -s /bin/sh postgres -c "initdb -D \$D -E UTF8 --locale=C.UTF-8 --auth-local=peer --auth-host=scram-sha-256" >/var/log/initdb.out 2>&1 || { cat /var/log/initdb.out; exit 1; }
cat > "\$D/pg_hba.conf" <<'HBA'
# Solo el rol de la aplicación entra por red, y con SCRAM. El superusuario,
# solo por el socket local y como el usuario del sistema postgres.
local all postgres peer
host  all $role 0.0.0.0/0 scram-sha-256
HBA
cat >> "\$D/postgresql.conf" <<'CONF'
listen_addresses = '$ip'
unix_socket_directories = '/run/postgresql'
password_encryption = 'scram-sha-256'
max_connections = 50
shared_buffers = 128MB
# Sin /dev/shm en el invitado: la memoria dinámica compartida va a ficheros.
dynamic_shared_memory_type = mmap
huge_pages = off
# Sin réplicas ni archivado: menos WAL en un overlay de 512 MiB.
wal_level = minimal
max_wal_senders = 0
min_wal_size = 32MB
max_wal_size = 96MB
timezone = 'UTC'
log_timezone = 'UTC'
# Que la contraseña no acabe en el log si una sentencia falla.
log_min_error_statement = panic
log_statement = none
logging_collector = off
# Auditoría de conexiones (kling db audit): quién entró, a qué base y desde
# dónde. Solo metadatos; sin log_statement no hay SQL ni datos en el log.
log_connections = on
log_disconnections = on
log_line_prefix = '%m [%p] u=%u d=%d h=%h '
CONF
chown postgres:postgres "\$D/pg_hba.conf" "\$D/postgresql.conf"
# El postmaster hereda las tuberías del exec: a fichero y sin stdin, o el
# exec no termina nunca.
su -s /bin/sh postgres -c "pg_ctl -D \$D -l /var/log/postgresql/pg.log -w -t 90 start" </dev/null >/var/log/pgctl.out 2>&1 || { cat /var/log/pgctl.out; tail -n 30 /var/log/postgresql/pg.log; exit 1; }
SETUP

  say "creando rol $role y base $db (la contraseña viaja por stdin)"
  # Si falla, NO se enseña la salida: el error de psql cita la sentencia, y la
  # sentencia lleva la contraseña.
  if ! {
    printf "CREATE ROLE %s LOGIN;\n" "$role"
    printf "ALTER ROLE %s PASSWORD '%s';\n" "$role" "$(cat "$BUILD_TMP/password")"
    printf "CREATE DATABASE %s OWNER %s;\n" "$db" "$role"
  } | "${K[@]}" exec -i "$m" -- su -s /bin/sh postgres -c "psql -X -q -v ON_ERROR_STOP=1 -d postgres" >/dev/null 2>&1; then
    die "no se pudo crear el rol o la base (salida omitida a propósito; mira /var/log/postgresql/pg.log con -keep)"
  fi

  if [ "$seed_mb" -gt 0 ]; then
    # ~330 bytes por fila con sus dos índices: la cifra real se imprime luego.
    local rows=$(( seed_mb * 1048576 / 330 ))
    cat > "$BUILD_TMP/seed-synth.sql" <<SQL
CREATE TABLE seed_events (
  id bigint PRIMARY KEY,
  account int NOT NULL,
  payload text NOT NULL,
  created_at timestamptz NOT NULL
);
INSERT INTO seed_events
  SELECT g, (g % 1000)::int, repeat(md5(g::text), 6),
         now() - (g % 86400) * interval '1 second'
  FROM generate_series(1, $rows) AS g;
CREATE INDEX seed_events_account_idx ON seed_events (account);
ANALYZE seed_events;
SQL
    files+=("$BUILD_TMP/seed-synth.sql")
  fi

  local n=0 dest who="PGOPTIONS='-c role=$role'"
  [ "$as_super" -eq 0 ] || who="PGOPTIONS=''"
  for f in "${files[@]}"; do
    n=$((n + 1))
    dest="/var/lib/dbgolden/in/$(printf '%03d' "$n").sql"
    say "aplicando $(basename "$f")"
    quiet "${K[@]}" cp "$f" "$m:$dest"
    # kling cp conserva el modo del host (0600 bajo nuestro umask) y el dueño root:
    # postgres no lo podría leer. Migraciones y seed no son secretos.
    quiet "${K[@]}" exec "$m" -- chmod 644 "$dest"
    quiet "${K[@]}" exec -timeout 30m "$m" -- su -s /bin/sh postgres -c \
      "$who psql -X -q -v ON_ERROR_STOP=1 -d $db -f $dest"
  done

  say "VACUUM, CHECKPOINT y comprobaciones antes de congelar"
  quiet "${K[@]}" exec -timeout 30m "$m" -- su -s /bin/sh postgres -c \
    "psql -X -q -v ON_ERROR_STOP=1 -d $db -c 'VACUUM (ANALYZE)'"
  quiet "${K[@]}" exec "$m" -- su -s /bin/sh postgres -c \
    "psql -X -q -v ON_ERROR_STOP=1 -d postgres -c CHECKPOINT"

  # Un dorado no puede llevar clientes conectados: sus sockets y sus claves de
  # cancelación se repartirían idénticos a todas las copias.
  local clients
  clients="$("${K[@]}" exec "$m" -- su -s /bin/sh postgres -c \
    "psql -X -At -d postgres -c \"SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid()\"")"
  [ "$clients" = 0 ] || die "quedan $clients conexiones de cliente abiertas; no se congela"

  local usage
  usage="$("${K[@]}" exec "$m" -- sh -c "df -P / | awk 'NR==2 {gsub(\"%\",\"\",\$5); print \$5}'")"
  [[ "$usage" =~ ^[0-9]+$ ]] || die "no se pudo leer el uso del disco"
  [ "$usage" -lt 85 ] || die "el overlay está al ${usage}%: no queda margen para las copias"
  local size
  size="$("${K[@]}" exec "$m" -- su -s /bin/sh postgres -c \
    "psql -X -At -d postgres -c \"SELECT pg_size_pretty(pg_database_size('$db'))\"")"
  say "base $db: $size · disco del overlay al ${usage}%"

  # Fuera los ficheros de entrada (pueden llevar datos) y a disco lo pendiente.
  quiet "${K[@]}" exec "$m" -- sh -c 'rm -rf /var/lib/dbgolden; sync'

  say "guardando la máquina viva como plantilla $name"
  "${K[@]}" save -replace -warm=false "$m" "$name"

  # La contraseña se escribe al final y en un solo paso: un intento fallido no
  # deja el fichero a medias ni pisa el de la plantilla anterior.
  install -m 600 "$BUILD_TMP/password" "$sdir/password.new"
  mv -f "$sdir/password.new" "$sdir/password"
  {
    echo "PGUSER=$role"
    echo "PGDATABASE=$db"
    echo "GUEST_IP=$ip"
    echo "TEMPLATE=$name"
  } > "$sdir/conn.env.new"
  mv -f "$sdir/conn.env.new" "$sdir/conn.env"

  BUILD_OK=1
  say "listo: plantilla $name"
  say "  contraseña: $sdir/password (0600, solo en este host)"
  say "  instanciar:  ${K[*]} run -from $name -name <copia>"
  say "  tras cada thaw/fork: scripts/db-post-thaw.sh <copia>"
  say "  comprobaciones en el lab: scripts/db-golden-verify.sh $name"
}

# -engine mysql (en cualquier sitio tras image o build) pasa todo a
# db-golden-mysql.sh, su hermano para MariaDB (docs/mysql.md); -engine redis y
# -engine sqlite, a db-golden-redis.sh y db-golden-sqlite.sh
# (docs/db-engines.md); -engine postgres es lo de siempre.
engine=postgres
args=()
while [ $# -gt 0 ]; do
  case "$1" in
    -engine)   engine="${2:?falta valor}"; shift 2 ;;
    -engine=*) engine="${1#-engine=}"; shift ;;
    *)         args+=("$1"); shift ;;
  esac
done
case "$engine" in
  postgres) ;;
  mysql|mariadb) exec bash "$HERE/db-golden-mysql.sh" ${args[@]+"${args[@]}"} ;;
  redis) exec bash "$HERE/db-golden-redis.sh" ${args[@]+"${args[@]}"} ;;
  sqlite) exec bash "$HERE/db-golden-sqlite.sh" ${args[@]+"${args[@]}"} ;;
  *) die "motor desconocido: $engine (postgres, mysql, redis o sqlite)" ;;
esac
set -- ${args[@]+"${args[@]}"}

case "${1:-}" in
  image) shift; cmd_image "$@" ;;
  build) shift; cmd_build "$@" ;;
  *) sed -n '2,30p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
