#!/usr/bin/env bash
# kling db, MySQL/MariaDB (#64): un MariaDB CALIENTE congelado como plantilla.
#
#   scripts/db-golden-mysql.sh image                     construye la imagen mariadb
#   scripts/db-golden-mysql.sh build [opciones] <nombre> arranca, carga y congela
#
# (o db-golden.sh image|build -engine mysql ..., que llega aquí.)
#
# `build` arranca una microVM desde la imagen mariadb (Alpine; sin red, con
# exec), crea el datadir en el overlay de la máquina, arranca MariaDB
# escuchando en la IP del invitado, crea el usuario y la base de la
# aplicación, aplica migraciones y seed y guarda la máquina VIVA como
# plantilla <nombre> con la etiqueta kling.db.engine=mysql: `kling db up
# <nombre>` da copias con el servidor ya caliente. Método y límites:
# docs/mysql.md.
#
# Opciones de build:
#   -migrations DIR   ficheros *.sql, en orden alfabético (como root: ver abajo)
#   -seed FILE        SQL de datos, después de las migraciones
#   -seed-mb N        en vez de -seed: datos sintéticos de ~N MiB
#   -role U           usuario de la aplicación (app)  -database B   base (appdb)
#   -image I          imagen (mariadb)                -mem M        RAM de la VM (1G)
#   -from T           plantilla con MariaDB instalado en vez de -image (macOS)
#   -cpus N           vCPUs (2)                       -state DIR    dónde va la contraseña
#   -keep             no borrar la máquina de preparación tras guardar
#
# Entorno: KLING = comando kling (con sus banderas, p.ej. "kling -H ssh://lab").
#
# LA CONTRASEÑA del usuario se genera aquí y se guarda SOLO en el host
# ($STATE/<nombre>/password, 0600). Al invitado va su hash de
# mysql_native_password ("*" + HEX(SHA1(SHA1(clave)))), calculado aquí con
# openssl (o python3), nunca la clave. Sin ninguno de los dos, no se construye.
#
# LAS MIGRACIONES corren como root (en MySQL no hay SET ROLE a un usuario):
# las vistas, rutinas, disparadores y eventos que creen tendrán DEFINER root, y
# kling db doctor lo avisa (MY020). Créalos con DEFINER = `app`@`%` o SQL
# SECURITY INVOKER.
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

die() { echo "db-golden-mysql: $*" >&2; exit 1; }
say() { echo "db-golden-mysql: $*"; }

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

# native_hash <fichero con la clave>: el hash de mysql_native_password.
native_hash() {
  local h
  if command -v openssl >/dev/null 2>&1; then
    h="$(openssl dgst -sha1 -binary < "$1" | openssl dgst -sha1 -binary | od -An -tx1 | tr -d ' \n' | tr 'a-f' 'A-F')"
  elif command -v python3 >/dev/null 2>&1; then
    h="$(python3 -c 'import hashlib,sys; print(hashlib.sha1(hashlib.sha1(open(sys.argv[1],"rb").read()).digest()).hexdigest().upper())' "$1")"
  else
    die "hace falta openssl o python3 en el host para calcular el hash de la contraseña (la contraseña no entra en el invitado)"
  fi
  [[ "$h" =~ ^[0-9A-F]{40}$ ]] || die "no se pudo calcular el hash de la contraseña"
  printf '*%s' "$h"
}

# El cliente del superusuario dentro de la máquina: root por el socket local
# (unix_socket), el binario que haya; vale también a los dos lados de una
# tubería. Lo variable va por stdin.
# shellcheck disable=SC2016 # se expande en el invitado, no aquí
MYC='"$(command -v mariadb || command -v mysql)" --protocol=socket -uroot'

cmd_image() {
  [ -f "$HERE/recipes/mariadb.recipe.json" ] || die "falta scripts/recipes/mariadb.recipe.json"
  say "construyendo la imagen mariadb (Alpine + mariadb); tarda unos minutos"
  "${K[@]}" image build mariadb -builder base -base min -grow 512 -spec "$HERE/recipes/mariadb.recipe.json"
  "${K[@]}" image recipe mariadb
}

cmd_build() {
  local migrations="" seed="" seed_mb=0 role=app db=appdb image=mariadb from=""
  local mem=1G cpus=2 state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}" name=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -migrations) migrations="${2:?falta valor}"; shift 2 ;;
      -seed)       seed="${2:?falta valor}"; shift 2 ;;
      -seed-mb)    seed_mb="${2:?falta valor}"; shift 2 ;;
      -as-super)   shift ;; # en MySQL las migraciones ya corren como root
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
  [ -n "$name" ] || die "uso: db-golden-mysql.sh build [opciones] <nombre>"

  # Todo lo que acaba en SQL o en una línea de shell se valida aquí.
  [[ "$name" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "nombre no válido: $name"
  [[ "$role" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "usuario no válido: $role"
  [[ "$db" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "base no válida: $db"
  case "$role" in root|mysql|mariadb_sys|mysql_sys) die "el usuario de la aplicación no puede ser $role" ;; esac
  case "$db" in mysql|sys|information_schema|performance_schema|test) die "la base de la aplicación no puede ser $db" ;; esac
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

  local m="$name-build" sdir="$state/$name"
  mkdir -p "$sdir"
  chmod 700 "$state" "$sdir"
  BUILD_TMP="$(mktemp -d)"
  BUILD_M="$m"
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  "${K[@]}" rm -f "$m" >/dev/null 2>&1 || true

  # Contraseña: 192 bits en hex, a un fichero 0600; al invitado, su hash.
  od -An -tx1 -N24 /dev/urandom | tr -d ' \n' > "$BUILD_TMP/password"
  [ "$(wc -c < "$BUILD_TMP/password" | tr -d ' ')" -eq 48 ] || die "no se pudo generar la contraseña"
  local hash
  hash="$(native_hash "$BUILD_TMP/password")"

  # La etiqueta del motor viaja con la plantilla (kling save copia las
  # etiquetas) y la heredan las copias: así kling db sabe que es MySQL.
  local labels=(-label "kling.db.engine=mysql" -label "kling.db.role=$role" -label "kling.db.database=$db")
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

  local cmdline ip
  cmdline="$("${K[@]}" exec "$m" -- cat /proc/cmdline)"
  ip="$(printf '%s' "$cmdline" | tr ' ' '\n' | sed -n 's/^ip=\([0-9.]*\)::.*/\1/p' | head -n1)"
  [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "no encuentro la IP del invitado en /proc/cmdline"

  say "datadir en el overlay y arranque de MariaDB en $ip"
  # El datadir va en la raíz de la máquina (overlay), NO en un volumen: Freeze
  # desmonta los volúmenes y Fork rechaza los de escritura.
  "${K[@]}" exec -i -timeout 5m "$m" -- sh -s <<SETUP
set -eu
D=/var/lib/mysql
BIN=\$(command -v mariadbd || command -v mysqld) || { echo "no hay mariadbd en la imagen"; exit 1; }
INIT=\$(command -v mariadb-install-db || command -v mysql_install_db) || { echo "no hay mariadb-install-db en la imagen"; exit 1; }
mkdir -p /run/mysqld /var/log/mysql /var/lib/mysql-files /etc/my.cnf.d
chown mysql:mysql /run/mysqld /var/log/mysql /var/lib/mysql-files
chmod 750 /var/lib/mysql-files
install -d -o mysql -g mysql -m 0700 "\$D"
# zz-: después de la configuración del paquete (la de Alpine trae
# skip-networking), y gana.
cat > /etc/my.cnf.d/zz-kling-db.cnf <<'CONF'
[client-server]
socket = /run/mysqld/mysqld.sock

[mysqld]
user = mysql
datadir = /var/lib/mysql
bind-address = $ip
port = 3306
skip-networking = 0
skip-name-resolve
# Ni LOAD DATA LOCAL ni ficheros fuera de un directorio vacío.
local-infile = 0
secure-file-priv = /var/lib/mysql-files
symbolic-links = 0
max_connections = 50
innodb_buffer_pool_size = 128M
innodb_log_file_size = 64M
default_time_zone = '+00:00'
character-set-server = utf8mb4
collation-server = utf8mb4_general_ci
# Sin log general ni de consultas lentas: ni SQL ni datos en disco.
general_log = 0
slow_query_log = 0
log_warnings = 2
CONF
# kling db audit: server_audit, SOLO conexiones (quién, desde dónde, a qué
# base), sin consultas. FORCE_PLUS_PERMANENT: nadie lo desinstala en marcha.
if P=\$(find /usr/lib -name server_audit.so 2>/dev/null | head -n1) && [ -n "\$P" ]; then
  cat >> /etc/my.cnf.d/zz-kling-db.cnf <<'CONF'
plugin_load_add = server_audit
server_audit = FORCE_PLUS_PERMANENT
server_audit_events = CONNECT
server_audit_logging = ON
server_audit_output_type = file
server_audit_file_path = /var/log/mysql/audit.log
server_audit_file_rotate_size = 1000000
server_audit_file_rotations = 3
CONF
fi
"\$INIT" --user=mysql --datadir="\$D" --auth-root-authentication-method=socket --skip-test-db >/var/log/mysql/install.out 2>&1 || { cat /var/log/mysql/install.out; exit 1; }
# El servidor hereda las tuberías del exec: a fichero, sin stdin y en su
# propia sesión, o el exec no termina nunca.
setsid "\$BIN" --user=mysql </dev/null >/var/log/mysql/mariadbd.out 2>&1 &
ADMIN=\$(command -v mariadb-admin || command -v mysqladmin)
i=0
until "\$ADMIN" --protocol=socket -uroot ping --silent >/dev/null 2>&1; do
  i=\$((i + 1))
  [ "\$i" -lt 90 ] || { tail -n 30 /var/log/mysql/mariadbd.out; exit 1; }
  sleep 1
done
SETUP

  say "cuentas: fuera las anónimas, root solo local; usuario $role y base $db (al invitado va el hash)"
  # Las cuentas que trae la instalación: solo root@localhost (unix_socket) y
  # las de sistema; ni anónimas ni root desde otro host.
  quiet "${K[@]}" exec -i "$m" -- sh -c "$MYC -N -B | $MYC" <<'SQL'
SELECT CONCAT('DROP USER ''', REPLACE(User, '''', ''''''), '''@''', REPLACE(Host, '''', ''''''), ''';')
  FROM mysql.user WHERE User = '' OR (User IN ('root', 'mysql') AND Host <> 'localhost');
SQL
  if ! {
    printf "DROP DATABASE IF EXISTS test;\n"
    printf "CREATE USER '%s'@'%%' IDENTIFIED WITH mysql_native_password AS '%s';\n" "$role" "$hash"
    printf "CREATE DATABASE %s CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci;\n" "$db"
    printf "GRANT ALL PRIVILEGES ON %s.* TO '%s'@'%%';\n" "$db" "$role"
  } | "${K[@]}" exec -i "$m" -- sh -c "$MYC" >/dev/null 2>&1; then
    die "no se pudo crear el usuario o la base (salida omitida; mira /var/log/mysql/mariadbd.out con -keep)"
  fi
  local left
  left="$("${K[@]}" exec -i "$m" -- sh -c "$MYC -N -B" <<<"SELECT COUNT(*) FROM mysql.user WHERE User = '' OR (User IN ('root', 'mysql') AND Host <> 'localhost') OR (User = '$role' AND Host = '%' AND authentication_string <> '$hash');")"
  [ "$left" = 0 ] || die "quedan cuentas anónimas, root remoto o el usuario sin su hash"

  if [ "$seed_mb" -gt 0 ]; then
    # ~330 bytes por fila con su índice (seq_1_to_N: el motor SEQUENCE de
    # MariaDB, que viene de serie).
    local rows=$(( seed_mb * 1048576 / 330 ))
    cat > "$BUILD_TMP/seed-synth.sql" <<SQL
CREATE TABLE seed_events (
  id BIGINT PRIMARY KEY,
  account INT NOT NULL,
  payload TEXT NOT NULL,
  created_at DATETIME NOT NULL,
  KEY seed_events_account_idx (account)
) ENGINE=InnoDB;
INSERT INTO seed_events
  SELECT seq, seq % 1000, REPEAT(MD5(seq), 6), TIMESTAMP '2026-01-01 00:00:00' - INTERVAL (seq % 86400) SECOND
  FROM seq_1_to_$rows;
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
    quiet "${K[@]}" exec -timeout 30m "$m" -- sh -c "$MYC $db < $dest"
  done

  say "ANALYZE, FLUSH y comprobaciones antes de congelar"
  # Los nombres de tabla van entre acentos graves (doblados si los llevan).
  local q_analyze
  q_analyze="SELECT CONCAT('ANALYZE TABLE \`', REPLACE(TABLE_NAME, '\`', '\`\`'), '\`;') FROM information_schema.TABLES WHERE TABLE_SCHEMA = '$db' AND TABLE_TYPE = 'BASE TABLE';"
  quiet "${K[@]}" exec -i -timeout 30m "$m" -- sh -c "$MYC -N -B | $MYC $db" <<<"$q_analyze"
  quiet "${K[@]}" exec "$m" -- sh -c "$MYC -e 'FLUSH TABLES'"

  # Un dorado no puede llevar clientes conectados: se repartirían a todas las
  # copias.
  local clients
  clients="$("${K[@]}" exec -i "$m" -- sh -c "$MYC -N -B" <<<"SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID <> CONNECTION_ID() AND COMMAND <> 'Daemon' AND USER NOT IN ('system user', 'event_scheduler');")"
  [ "$clients" = 0 ] || die "quedan $clients conexiones de cliente abiertas; no se congela"

  local audit
  audit="$("${K[@]}" exec -i "$m" -- sh -c "$MYC -N -B" <<<"SELECT COALESCE((SELECT PLUGIN_STATUS FROM information_schema.PLUGINS WHERE PLUGIN_NAME = 'SERVER_AUDIT'), 'NONE');")"
  if [ "$audit" != ACTIVE ]; then
    say "AVISO: server_audit no está activo ($audit): kling db audit solo verá los eventos del daemon (docs/mysql.md)"
  fi

  local usage
  usage="$("${K[@]}" exec "$m" -- sh -c "df -P / | awk 'NR==2 {gsub(\"%\",\"\",\$5); print \$5}'")"
  [[ "$usage" =~ ^[0-9]+$ ]] || die "no se pudo leer el uso del disco"
  [ "$usage" -lt 85 ] || die "el overlay está al ${usage}%: no queda margen para las copias"
  local size
  size="$("${K[@]}" exec -i "$m" -- sh -c "$MYC -N -B" <<<"SELECT CONCAT(ROUND(COALESCE(SUM(DATA_LENGTH + INDEX_LENGTH), 0) / 1048576, 1), ' MiB') FROM information_schema.TABLES WHERE TABLE_SCHEMA = '$db';")"
  say "base $db: $size · disco del overlay al ${usage}%"

  quiet "${K[@]}" exec "$m" -- sh -c 'rm -rf /var/lib/dbgolden; sync'

  say "guardando la máquina viva como plantilla $name"
  "${K[@]}" save -replace -warm=false "$m" "$name"

  install -m 600 "$BUILD_TMP/password" "$sdir/password.new"
  mv -f "$sdir/password.new" "$sdir/password"
  {
    echo "ENGINE=mysql"
    echo "DBUSER=$role"
    echo "DBNAME=$db"
    echo "GUEST_IP=$ip"
    echo "TEMPLATE=$name"
  } > "$sdir/conn.env.new"
  mv -f "$sdir/conn.env.new" "$sdir/conn.env"

  BUILD_OK=1
  say "listo: plantilla $name (mysql)"
  say "  contraseña: $sdir/password (0600, solo en este host)"
  say "  una copia:  kling db up $name -name <copia>"
}

case "${1:-}" in
  image) shift; cmd_image "$@" ;;
  build) shift; cmd_build "$@" ;;
  *) sed -n '2,37p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
