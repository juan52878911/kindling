#!/usr/bin/env bash
# kling db, nodo N1: un Postgres CALIENTE congelado como plantilla dorada.
#
#   scripts/db-golden.sh image                     construye la imagen pg16
#   scripts/db-golden.sh image -ext [-pg 17] [-only timescaledb,vector]
#                                                  plantilla pg16-ext con extensiones compiladas
#                                                  (db-image-ext.sh; golden build -from pg16-ext)
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
#   -as-super         migraciones y seed como superusuario (todo o nada; mejor -extension)
#   -extension A,B    CREATE EXTENSION como superusuario ANTES de las migraciones,
#                     que siguen como el rol de la app (repetible)
#   -preload A,B      shared_preload_libraries (se suma a lo que ya traiga la plantilla)
#   -conf CLAVE=VALOR una línea más en postgresql.conf (repetible)
#   -init DIR         un directorio al estilo docker-entrypoint-initdb.d: *.sql,
#                     *.sql.gz y *.sh en orden, con \i a ficheros hermanos;
#                     como el rol de la app (con CREATEROLE solo mientras dura)
#   -env-file F       KEY=VALUE para los .sh de -init (0600; claves fuera de argv)
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
  # -ext: la plantilla con extensiones compiladas (TimescaleDB TSL, pgvector,
  # PostGIS, pg_cron, pg_partman), con su propio script.
  if [ "${1:-}" = -ext ]; then
    shift
    exec bash "$HERE/db-image-ext.sh" "$@"
  fi
  [ -f "$HERE/recipes/pg16.recipe.json" ] || die "falta scripts/recipes/pg16.recipe.json"
  say "construyendo la imagen pg16 (Alpine + postgresql16); tarda unos minutos"
  "${K[@]}" image build pg16 -builder base -base min -grow 512 -spec "$HERE/recipes/pg16.recipe.json"
  "${K[@]}" image recipe pg16
}

# run_init ejecuta el directorio de -init (lo llama cmd_build, con sus
# variables: m, role, db, init, init_files, env_file).
#
# Como docker-entrypoint-initdb.d: *.sql y *.sql.gz con psql, *.sh con bash
# (o sh), en orden, desde el propio directorio (un \i de un .psql hermano
# funciona) y con POSTGRES_USER, POSTGRES_DB y lo de -env-file en el entorno.
# Lo demás (.psql, .md...) solo se usa si un script lo incluye.
#
# Diferencia a propósito con Docker: allí corre como el superusuario (que es
# POSTGRES_USER). Aquí corre como el rol de la aplicación, que NO es
# superusuario: los objetos son suyos y la RLS le aplica. Para lo que un init
# suele hacer además (crear roles: app_user, uno por servicio), el rol tiene
# CREATEROLE solo mientras dura -init, y el socket local le deja entrar sin
# clave solo mientras dura -init (la máquina de preparación no tiene red).
# Las extensiones van antes, con -extension.
run_init() {
  say "-init $(basename "$init"): ${#init_files[@]} script(s) como $role"
  tar -C "$init" -czf "$BUILD_TMP/init.tgz" .
  quiet "${K[@]}" cp "$BUILD_TMP/init.tgz" "$m:/var/lib/dbgolden/init.tgz"
  if [ -n "$env_file" ]; then
    quiet "${K[@]}" cp "$env_file" "$m:/var/lib/dbgolden/init.env"
  fi
  "${K[@]}" exec -i -timeout 5m "$m" -- sh -s <<PREP >/dev/null || die "no se pudo preparar -init"
set -eu
D=/var/lib/postgresql/data
mkdir -p /var/lib/dbgolden/init
tar xzf /var/lib/dbgolden/init.tgz -C /var/lib/dbgolden/init
rm /var/lib/dbgolden/init.tgz
chown -R postgres:postgres /var/lib/dbgolden/init
[ ! -f /var/lib/dbgolden/init.env ] || { chown postgres:postgres /var/lib/dbgolden/init.env; chmod 600 /var/lib/dbgolden/init.env; }
# El rol entra por el socket sin clave mientras dura -init (primera línea).
sed -i '1i local all $role trust # kling-db-init' "\$D/pg_hba.conf"
su -s /bin/sh postgres -c "pg_ctl -D \$D reload" >/dev/null
su -s /bin/sh postgres -c "psql -X -q -v ON_ERROR_STOP=1 -d postgres -c 'ALTER ROLE $role CREATEROLE'"
PREP
  local f base out shell
  for f in "${init_files[@]}"; do
    base="$(basename "$f")"
    say "  $base"
    case "$base" in
      *.sql)    out="$("${K[@]}" exec -timeout 30m "$m" -- su -s /bin/sh postgres -c \
                  "cd /var/lib/dbgolden/init && psql -X -q -v ON_ERROR_STOP=1 -h /run/postgresql -U $role -d $db -f ./$base" 2>&1)" ;;
      *.sql.gz) out="$("${K[@]}" exec -timeout 30m "$m" -- su -s /bin/sh postgres -c \
                  "cd /var/lib/dbgolden/init && gunzip -c ./$base | psql -X -q -v ON_ERROR_STOP=1 -h /run/postgresql -U $role -d $db" 2>&1)" ;;
      *.sh)
        shell="sh"
        if head -n1 "$f" | grep -q bash; then
          "${K[@]}" exec "$m" -- sh -c 'command -v bash' >/dev/null 2>&1 \
            || die "$base needs bash and this image has none: build the golden from pg16-ext (kling db golden image -ext), which has it"
          shell=bash
        fi
        out="$("${K[@]}" exec -timeout 30m "$m" -- su -s /bin/sh postgres -c \
          "cd /var/lib/dbgolden/init && if [ -f ../init.env ]; then while IFS= read -r l || [ -n \"\$l\" ]; do case \"\$l\" in ''|'#'*) continue ;; esac; export \"\${l%%=*}=\${l#*=}\"; done < ../init.env; fi; POSTGRES_USER=$role POSTGRES_DB=$db PGHOST=/run/postgresql PGUSER=$role PGDATABASE=$db $shell ./$base" 2>&1)" ;;
    esac || {
      out="${out//psql:.\//psql:$init/}"
      out="${out//\/var\/lib\/dbgolden\/init\//$init/}"
      printf '%s\n' "$out" >&2
      die "-init $base failed (file and line above); nothing was saved"
    }
    [ -z "$out" ] || printf '%s\n' "$out" | sed 's/^/    /'
  done
  "${K[@]}" exec -i -timeout 5m "$m" -- sh -s <<POST >/dev/null || die "no se pudo cerrar -init"
set -eu
D=/var/lib/postgresql/data
su -s /bin/sh postgres -c "psql -X -q -v ON_ERROR_STOP=1 -d postgres -c 'ALTER ROLE $role NOCREATEROLE'"
sed -i '/ # kling-db-init\$/d' "\$D/pg_hba.conf"
su -s /bin/sh postgres -c "pg_ctl -D \$D reload" >/dev/null
rm -rf /var/lib/dbgolden/init /var/lib/dbgolden/init.env
POST
}

cmd_build() {
  local migrations="" seed="" seed_mb=0 as_super=0 role=app db=appdb image=pg16 from=""
  local mem=1G cpus=2 mem_cpus_set=0 state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}" name=""
  local exts="" preload="" confs=() init="" env_file=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -migrations) migrations="${2:?falta valor}"; shift 2 ;;
      -seed)       seed="${2:?falta valor}"; shift 2 ;;
      -seed-mb)    seed_mb="${2:?falta valor}"; shift 2 ;;
      -as-super)   as_super=1; shift ;;
      -extension)  exts="${exts:+$exts,}${2:?falta valor}"; shift 2 ;;
      -preload)    preload="${preload:+$preload,}${2:?falta valor}"; shift 2 ;;
      -conf)       confs+=("${2:?falta valor}"); shift 2 ;;
      -init)       init="${2:?falta valor}"; shift 2 ;;
      -env-file)   env_file="${2:?falta valor}"; shift 2 ;;
      -role)       role="${2:?falta valor}"; shift 2 ;;
      -database)   db="${2:?falta valor}"; shift 2 ;;
      -image)      image="${2:?falta valor}"; shift 2 ;;
      -from)       from="${2:?falta valor}"; shift 2 ;;
      -mem)        mem="${2:?falta valor}"; mem_cpus_set=1; shift 2 ;;
      -cpus)       cpus="${2:?falta valor}"; mem_cpus_set=1; shift 2 ;;
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
  local e
  local -a ext_list=() pre_list=()
  [ -z "$exts" ] || IFS=, read -r -a ext_list <<<"$exts"
  [ -z "$preload" ] || IFS=, read -r -a pre_list <<<"$preload"
  for e in ${ext_list[@]+"${ext_list[@]}"}; do
    [[ "$e" =~ ^[a-z0-9_][a-z0-9_-]{0,62}$ ]] || die "-extension no válida: $e"
  done
  for e in ${pre_list[@]+"${pre_list[@]}"}; do
    [[ "$e" =~ ^[a-z0-9_]{1,63}$ ]] || die "-preload no válido: $e"
  done
  # -conf: CLAVE=VALOR, con la clave de un GUC y el valor en una línea. Las
  # que sostienen la seguridad y la auditoría del golden no se tocan.
  local c k v q="'" conf_text=""
  for c in ${confs[@]+"${confs[@]}"}; do
    [[ "$c" =~ ^([a-z_][a-z0-9_.]{0,62})=(.*)$ ]] || die "-conf no válido (CLAVE=VALOR): $c"
    k="${BASH_REMATCH[1]}" v="${BASH_REMATCH[2]}"
    [[ "$v" != *[[:cntrl:]]* && "$v" != *\\* ]] || die "-conf $k: el valor no puede llevar caracteres de control ni barras invertidas"
    case "$k" in
      shared_preload_libraries) die "-conf $k: usa -preload (se suma a lo que traiga la plantilla)" ;;
      listen_addresses|port|unix_socket_*|password_encryption|ssl*|hba_file|ident_file|data_directory|config_file|include*|log_*|logging_collector)
        die "-conf $k: lo fija el golden (seguridad, conexión o auditoría)" ;;
    esac
    conf_text+="$k = $q${v//$q/$q$q}$q"$'\n'
  done
  [ "$seed_mb" -le "$SEED_MB_MAX" ] || die "-seed-mb máximo $SEED_MB_MAX: el overlay de la máquina es de 512 MiB"
  [ -z "$seed" ] || [ "$seed_mb" -eq 0 ] || die "-seed y -seed-mb son excluyentes"
  [ -z "$seed" ] || [ -f "$seed" ] || die "no existe el seed: $seed"
  [ -z "$init" ] || [ -d "$init" ] || die "no existe el directorio de -init: $init"
  if [ -n "$env_file" ]; then
    [ -f "$env_file" ] && [ ! -L "$env_file" ] || die "-env-file no es un fichero: $env_file"
    case "$(stat -c %a "$env_file" 2>/dev/null || stat -f %Lp "$env_file")" in
      600|400) ;;
      *) die "-env-file $env_file: puede llevar claves; chmod 600" ;;
    esac
  fi
  local -a init_files=()
  if [ -n "$init" ]; then
    while IFS= read -r f; do init_files+=("$f"); done < <(find "$init" -maxdepth 1 -type f \( -name '*.sql' -o -name '*.sql.gz' -o -name '*.sh' \) | LC_ALL=C sort)
    [ "${#init_files[@]}" -gt 0 ] || die "-init $init: no hay *.sql, *.sql.gz ni *.sh"
  fi
  local files=() f
  if [ -n "$migrations" ]; then
    [ -d "$migrations" ] || die "no existe el directorio de migraciones: $migrations"
    # LC_ALL=C: el orden no puede depender del locale de quien lo lanza.
    while IFS= read -r f; do files+=("$f"); done < <(find "$migrations" -maxdepth 1 -type f -name '*.sql' | LC_ALL=C sort)
    [ "${#files[@]}" -gt 0 ] || die "no hay ficheros .sql en $migrations"
  fi
  [ -z "$seed" ] || files+=("$seed")

  local m="$name-build" sdir="$state/$name"
  # El directorio de la plantilla se crea al final, con la plantilla ya
  # guardada: un build que falla no deja un "golden" a medias en el estado.
  mkdir -p "$state"
  chmod 700 "$state"
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
    # La memoria y las CPU de una máquina que nace de una plantilla son las de
    # su snapshot: no se pueden cambiar al restaurar. Se dice, en vez de
    # ignorar -mem/-cpus en silencio.
    if [ "$mem_cpus_set" -eq 1 ]; then
      echo "db-golden: warning: -mem and -cpus don't apply with -from: the golden gets the memory and CPUs of the template $from (kling template ls); build the template with the size you want" >&2
    fi
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

  # Lo de -conf y -preload, a un fichero que SETUP añade a postgresql.conf
  # (por kling cp: ningún valor pasa por una línea de shell del invitado).
  if [ -n "$conf_text" ] || [ "${#pre_list[@]}" -gt 0 ]; then
    printf '%s' "$conf_text" > "$BUILD_TMP/extra.conf"
    printf '%s\n' ${pre_list[@]+"${pre_list[@]}"} > "$BUILD_TMP/preload"
    quiet "${K[@]}" cp "$BUILD_TMP/extra.conf" "$m:/tmp/kdb-extra.conf"
    quiet "${K[@]}" cp "$BUILD_TMP/preload" "$m:/tmp/kdb-preload"
  fi

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
# -preload: lo pedido se SUMA a lo que ya traiga la plantilla (pg16-ext precarga
# timescaledb), y cada librería tiene que existir: si no, Postgres no arranca y
# el error llega tarde y críptico.
if [ -f /tmp/kdb-preload ]; then
  LIBDIR=\$(dirname "\$(ls /usr/lib/postgresql*/plpgsql.so | head -n1)")
  FALTAN=""
  for L in \$(cat /tmp/kdb-preload); do [ -f "\$LIBDIR/\$L.so" ] || FALTAN="\$FALTAN \$L"; done
  [ -z "\$FALTAN" ] || { echo "db-golden: -preload: not installed in this image:\$FALTAN (kling db golden image -ext builds pg16-ext)" >&2; exit 1; }
  CUR=\$(su -s /bin/sh postgres -c "postgres -D \$D -C shared_preload_libraries" | tr -d "' ")
  ALL=\$( { echo "\$CUR" | tr ',' '\n'; cat /tmp/kdb-preload; } | grep -v '^\$' | awk '!v[\$0]++' | paste -sd, -)
  echo "shared_preload_libraries = '\$ALL'" >> "\$D/postgresql.conf"
  rm -f /tmp/kdb-preload
fi
if [ -f /tmp/kdb-extra.conf ]; then
  cat /tmp/kdb-extra.conf >> "\$D/postgresql.conf"
  rm -f /tmp/kdb-extra.conf
fi
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

  # Extensiones: TODAS las que hacen falta (las de -extension y las que crean
  # las migraciones) se comprueban de una vez, antes de ejecutar nada, y el
  # error las lista todas con dónde se piden. Antes, psql paraba en la primera
  # y la segunda no se veía hasta arreglar aquella.
  local avail mencion falta="" super="" nombre donde
  # pedida_n[i] es una extensión y pedida_d[i] quién la pide (-extension o
  # fichero:línea). Dos arrays y no uno asociativo: la bash de macOS (3.2) no
  # los tiene.
  local -a pedida_n=() pedida_d=()
  pedir() {
    local i
    for i in ${pedida_n[@]+"${!pedida_n[@]}"}; do [ "${pedida_n[$i]}" = "$1" ] && return 0; done
    pedida_n+=("$1"); pedida_d+=("$2")
  }
  for e in ${ext_list[@]+"${ext_list[@]}"}; do pedir "$e" "-extension"; done
  local -a scan=(${files[@]+"${files[@]}"})
  if [ -n "$init" ]; then
    while IFS= read -r f; do scan+=("$f"); done < <(find "$init" -maxdepth 1 -type f \( -name '*.sql' -o -name '*.psql' -o -name '*.sh' \) | LC_ALL=C sort)
  fi
  if [ "${#scan[@]}" -gt 0 ]; then
    while IFS= read -r mencion; do
      donde="$(printf '%s' "$mencion" | cut -d: -f1-2)"
      # Comentarios fuera: "-- CREATE EXTENSION x" no pide nada.
      [[ "$(printf '%s' "${mencion#"$donde":}" | sed 's/^[[:space:]]*//')" != --* ]] || continue
      nombre="$(printf '%s' "$mencion" | sed -E 's/.*[Ee][Xx][Tt][Ee][Nn][Ss][Ii][Oo][Nn][[:space:]]+([Ii][Ff][[:space:]]+[Nn][Oo][Tt][[:space:]]+[Ee][Xx][Ii][Ss][Tt][Ss][[:space:]]+)?"?([A-Za-z0-9_-]+).*/\2/' | tr 'A-Z' 'a-z')"
      [[ "$nombre" =~ ^[a-z0-9_][a-z0-9_-]{0,62}$ ]] || continue
      pedir "$nombre" "$donde"
    done < <(grep -HniE 'create[[:space:]]+extension[[:space:]]' "${scan[@]}" 2>/dev/null || true)
  fi
  if [ "${#pedida_n[@]}" -gt 0 ]; then
    avail="$("${K[@]}" exec "$m" -- su -s /bin/sh postgres -c \
      "psql -X -At -d postgres -c \"SELECT name || ' ' || bool_or(trusted) FROM pg_available_extension_versions GROUP BY name\"")" \
      || die "no se pudo leer pg_available_extension_versions"
    local i
    for i in "${!pedida_n[@]}"; do
      e="${pedida_n[$i]}"
      case $'\n'"$avail"$'\n' in
        *$'\n'"$e "*) ;;
        *) falta+=$'\n'"  $e  (${pedida_d[$i]})"; continue ;;
      esac
      # Una no confiable (timescaledb, postgis...) solo la crea un superusuario:
      # sin -extension ni -as-super, la migración fallaría como el rol de la app.
      if [ "${pedida_d[$i]}" != "-extension" ] && [ "$as_super" -eq 0 ] && ! printf '%s\n' "$avail" | grep -qx "$e true"; then
        super+=",$e"
      fi
    done
    [ -z "$falta" ] || die "extensions not available in this image:$falta
  build a template that has them:  kling db golden image -ext   (then: golden build -from pg16-ext)"
    [ -z "$super" ] || die "the migrations create extensions that only a superuser can create: ${super#,}
  add:  -extension ${super#,}   (created as superuser before the migrations, which still run as $role)"
  fi
  if [ "${#ext_list[@]}" -gt 0 ]; then
    say "creando extensiones como superusuario: ${ext_list[*]}"
    local esql=""
    for e in "${ext_list[@]}"; do esql+="CREATE EXTENSION IF NOT EXISTS \"$e\" CASCADE;"$'\n'; done
    printf '%s' "$esql" > "$BUILD_TMP/extensions.sql"
    quiet "${K[@]}" cp "$BUILD_TMP/extensions.sql" "$m:/var/lib/dbgolden/in/000-extensions.sql"
    quiet "${K[@]}" exec "$m" -- chmod 644 /var/lib/dbgolden/in/000-extensions.sql
    quiet "${K[@]}" exec -timeout 10m "$m" -- su -s /bin/sh postgres -c \
      "psql -X -q -v ON_ERROR_STOP=1 -d $db -f /var/lib/dbgolden/in/000-extensions.sql"
  fi

  if [ -n "$init" ]; then
    run_init
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
    # El error de psql cita la ruta de la copia (/var/lib/dbgolden/in/007.sql)
    # y la línea: se cambia por la del fichero de verdad.
    local out
    if ! out="$("${K[@]}" exec -timeout 30m "$m" -- su -s /bin/sh postgres -c \
      "$who psql -X -q -v ON_ERROR_STOP=1 -d $db -f $dest" 2>&1)"; then
      out="${out//psql:$dest:/$f:}"
      printf '%s\n' "${out//$dest/$f}" >&2
      die "$(basename "$f") failed (file and line above); nothing was saved"
    fi
  done

  say "VACUUM, CHECKPOINT y comprobaciones antes de congelar"
  quiet "${K[@]}" exec -timeout 30m "$m" -- su -s /bin/sh postgres -c \
    "psql -X -q -v ON_ERROR_STOP=1 -d $db -c 'VACUUM (ANALYZE)'"
  # También postgres y template1: sus catálogos de initdb no se han analizado
  # nunca, y sin esto el autovacuum lo hace en CADA copia al minuto o dos de
  # arrancar (11 transacciones medidas en template1). Eso cambia la huella con
  # la que kling db branch decide si una copia de reserva sigue valiendo.
  for d in postgres template1; do
    [ "$d" = "$db" ] && continue
    quiet "${K[@]}" exec -timeout 10m "$m" -- su -s /bin/sh postgres -c \
      "psql -X -q -v ON_ERROR_STOP=1 -d $d -c 'VACUUM (ANALYZE)'"
  done
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

  # Fuera los ficheros de entrada (pueden llevar datos), el log de Postgres de
  # la construcción (si no, `kling db audit` de cada copia lo mezclaría con lo
  # suyo) y a disco lo pendiente.
  quiet "${K[@]}" exec "$m" -- sh -c 'rm -rf /var/lib/dbgolden; : > /var/log/postgresql/pg.log; sync'

  say "guardando la máquina viva como plantilla $name"
  "${K[@]}" save -replace -warm=false "$m" "$name"

  # La contraseña se escribe al final y en un solo paso: un intento fallido no
  # deja el fichero a medias ni pisa el de la plantilla anterior.
  mkdir -p "$sdir"
  chmod 700 "$sdir"
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
