#!/usr/bin/env bash
# kling db, compatible con MongoDB (#117): un FerretDB CALIENTE congelado como
# plantilla.
#
#   scripts/db-golden-ferretdb.sh image                     importa la imagen ferretdb
#   scripts/db-golden-ferretdb.sh build [opciones] <nombre> arranca, carga y congela
#
# (o db-golden.sh image|build -engine ferretdb ..., que llega aquí.)
#
# FerretDB (Apache-2.0) habla el protocolo de MongoDB sobre un Postgres con la
# extensión DocumentDB. La imagen es la oficial de evaluación
# (ghcr.io/ferretdb/ferretdb-eval:2: Postgres 17 + DocumentDB + FerretDB),
# importada sin Docker con kling image import; Alpine no empaqueta DocumentDB.
#
# `build` arranca una microVM desde esa imagen (sin red, con exec), arranca
# Postgres y FerretDB, crea el rol de la aplicación, carga datos de prueba si se
# piden y guarda la máquina VIVA como plantilla <nombre> con la etiqueta
# kling.db.engine=ferretdb: `kling db up <nombre>` da copias con el servidor ya
# caliente, y por debajo cada copia es un Postgres: kling db rota la
# contraseña del rol con un verificador SCRAM, como en Postgres, y FerretDB
# comprueba el login de Mongo (SCRAM-SHA-256) contra ese rol.
#
# Opciones de build:
#   -seed-docs N      N documentos sintéticos en <base>.seed
#   -role U           rol de la aplicación (app)      -database B   base de Mongo (appdb)
#   -image I          imagen (ferretdb)               -mem M        RAM de la VM (1G)
#   -cpus N           vCPUs (2)                       -state DIR    dónde va la contraseña
#   -keep             no borrar la máquina de preparación tras guardar
#
# LA CONTRASEÑA del rol de la aplicación se genera aquí, se guarda en el host
# ($STATE/<nombre>/password, 0600) y entra al invitado solo por stdin, para el
# ALTER ROLE del dorado (como db-golden.sh). Las copias no la usan: kling db up
# les pone otra con un verificador SCRAM calculado en el host.
#
# EL SUPERUSUARIO postgres tiene una clave generada DENTRO del invitado, que
# solo usa FerretDB para su propia conexión por el loopback; no sale nunca. Por
# red solo entra el rol de la aplicación (pg_hba).
#
# EL ROL de la aplicación lleva documentdb_admin_role: FerretDB 2 no tiene aún
# permisos por base, así que ese rol ve todas las bases de Mongo de la copia
# (que son suyas: la copia es de un solo inquilino).
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

die() { echo "db-golden-ferretdb: $*" >&2; exit 1; }
say() { echo "db-golden-ferretdb: $*"; }

SEED_DOCS_MAX=500000
IMAGE_REF="${FERRETDB_IMAGE:-ghcr.io/ferretdb/ferretdb-eval:2}"

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

# psql como el superusuario, por el socket local del invitado; el SQL por stdin.
# shellcheck disable=SC2016 # se expande en el invitado, no aquí
PSQL='su postgres -c "psql -X -q -At -v ON_ERROR_STOP=1 -d postgres"'

cmd_image() {
  say "importando $IMAGE_REF como ferretdb (Postgres 17 + DocumentDB + FerretDB; el servidor lo arranca build)"
  "${K[@]}" image import "$IMAGE_REF" -name ferretdb -replace -entrypoint sleep -- infinity
}

cmd_build() {
  local seed_docs=0 role=app db=appdb image=ferretdb
  local mem=1G cpus=2 state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}" name=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -seed-docs)  seed_docs="${2:?falta valor}"; shift 2 ;;
      -role)       role="${2:?falta valor}"; shift 2 ;;
      -database)   db="${2:?falta valor}"; shift 2 ;;
      -image)      image="${2:?falta valor}"; shift 2 ;;
      -mem)        mem="${2:?falta valor}"; shift 2 ;;
      -cpus)       cpus="${2:?falta valor}"; shift 2 ;;
      -state)      state="${2:?falta valor}"; shift 2 ;;
      -keep)       BUILD_KEEP=1; shift ;;
      -migrations|-seed|-seed-mb|-as-super|-template|-from)
                   die "$1 no existe para FerretDB: usa -seed-docs, o carga los datos con tu driver en una copia y guárdala" ;;
      -*)          die "opción desconocida: $1" ;;
      *)           [ -z "$name" ] || die "sobra un argumento: $1"; name="$1"; shift ;;
    esac
  done
  [ -n "$name" ] || die "uso: db-golden-ferretdb.sh build [opciones] <nombre>"

  # Todo lo que acaba en SQL o en una línea de shell se valida aquí.
  [[ "$name" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "nombre no válido: $name"
  [[ "$role" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "rol no válido: $role"
  [[ "$db" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "base no válida: $db"
  case "$role" in postgres|documentdb*) die "el rol de la aplicación no puede ser $role" ;; esac
  case "$db" in admin|local|config) die "la base de la aplicación no puede ser $db" ;; esac
  [[ "$image" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "imagen no válida: $image"
  [[ "$mem" =~ ^[0-9]+[MG]?$ ]] || die "-mem no válido: $mem"
  [[ "$cpus" =~ ^[0-9]+$ ]] || die "-cpus no válido: $cpus"
  [[ "$seed_docs" =~ ^[0-9]+$ ]] || die "-seed-docs no válido: $seed_docs"
  [ "$seed_docs" -le "$SEED_DOCS_MAX" ] || die "-seed-docs máximo $SEED_DOCS_MAX"

  local m="$name-build" sdir="$state/$name"
  mkdir -p "$sdir"
  chmod 700 "$state" "$sdir"
  BUILD_TMP="$(mktemp -d)"
  BUILD_M="$m"
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  "${K[@]}" rm -f "$m" >/dev/null 2>&1 || true

  # Contraseña: 192 bits en hex, a un fichero 0600.
  od -An -tx1 -N24 /dev/urandom | tr -d ' \n' > "$BUILD_TMP/password"
  [ "$(wc -c < "$BUILD_TMP/password" | tr -d ' ')" -eq 48 ] || die "no se pudo generar la contraseña"

  local labels=(-label "kling.db.engine=ferretdb" -label "kling.db.role=$role" -label "kling.db.database=$db")
  say "arrancando $m desde la imagen $image (egress none)"
  "${K[@]}" run -name "$m" -image "$image" -egress none -allow-exec \
    -cpus "$cpus" -mem "$mem" -cpu-pct 100 "${labels[@]}" >/dev/null

  local i
  for i in $(seq 1 60); do
    if "${K[@]}" exec -timeout 5s "$m" -- true >/dev/null 2>&1; then break; fi
    [ "$i" -lt 60 ] || die "el agente del invitado no contesta"
    sleep 1
  done

  say "arranque de Postgres + DocumentDB y FerretDB"
  "${K[@]}" exec -i -timeout 5m "$m" -- bash -s <<SETUP
set -eu
umask 077
mkdir -p /etc/kling-db
# La clave del superusuario: generada aquí dentro; solo la usa FerretDB.
N=\$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
[ \${#N} -eq 48 ] || { echo "sin bytes aleatorios"; exit 1; }
printf %s "\$N" > /etc/kling-db/pg-super
chmod 600 /etc/kling-db/pg-super
# El entrypoint de la imagen hace initdb y lanza supervisord (Postgres y
# FerretDB). En su propia sesión y sin las tuberías del exec, o no termina.
POSTGRES_PASSWORD="\$N" setsid /usr/local/bin/entrypoint.sh </dev/null >/var/log/ferretdb-eval.out 2>&1 &
i=0
until /usr/local/bin/ferretdb ping >/dev/null 2>&1; do
  i=\$((i + 1))
  [ "\$i" -lt 120 ] || { tail -n 30 /var/log/ferretdb-eval.out; exit 1; }
  sleep 1
done
# Por red, solo el rol de la aplicación y con SCRAM. El loopback y el socket
# quedan como los deja la imagen (los usa FerretDB, dentro del invitado).
F=\$(su postgres -c "psql -X -At -d postgres -c 'SHOW hba_file'")
sed -i 's/^host all all all scram-sha-256\$/host all $role all scram-sha-256/' "\$F"
grep -q '^host all $role all scram-sha-256\$' "\$F" || { echo "no encuentro la línea de red de pg_hba.conf"; exit 1; }
su postgres -c "psql -X -q -At -d postgres -c 'SELECT pg_reload_conf()'" >/dev/null
SETUP

  say "rol $role (la clave entra por stdin) y base $db"
  if ! {
    printf "CREATE ROLE %s LOGIN;\n" "$role"
    printf "ALTER ROLE %s PASSWORD '%s';\n" "$role" "$(cat "$BUILD_TMP/password")"
    printf "GRANT documentdb_admin_role TO %s;\n" "$role"
  } | "${K[@]}" exec -i "$m" -- sh -c "$PSQL" >/dev/null 2>&1; then
    die "no se pudo crear el rol (salida omitida: puede citar la clave)"
  fi

  if [ "$seed_docs" -gt 0 ]; then
    say "cargando $seed_docs documentos sintéticos en $db.seed"
    quiet "${K[@]}" exec -i -timeout 30m "$m" -- sh -c "$PSQL" <<SQL
SELECT count(documentdb_api.insert_one('$db', 'seed',
  format('{"n": %s, "account": %s, "payload": "%s"}', g, g % 1000, repeat(md5(g::text), 4))::documentdb_core.bson))
  FROM generate_series(1, $seed_docs) g;
SQL
  fi

  say "VACUUM, CHECKPOINT y comprobaciones antes de congelar"
  quiet "${K[@]}" exec -i -timeout 30m "$m" -- sh -c "$PSQL" <<<"VACUUM (ANALYZE);"
  # Un dorado no puede llevar clientes conectados: se repartirían a todas las
  # copias. FerretDB tiene su pool abierto: esos no cuentan.
  local clients
  clients="$("${K[@]}" exec -i "$m" -- sh -c "$PSQL" <<<"SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend' AND pid <> pg_backend_pid() AND usename = '$role';")"
  [ "$clients" = 0 ] || die "quedan $clients conexiones del rol $role abiertas; no se congela"
  quiet "${K[@]}" exec -i "$m" -- sh -c "$PSQL" <<<"CHECKPOINT;"

  local ip cmdline
  cmdline="$("${K[@]}" exec "$m" -- cat /proc/cmdline)"
  ip="$(printf '%s' "$cmdline" | tr ' ' '\n' | sed -n 's/^ip=\([0-9.]*\)::.*/\1/p' | head -n1)"

  say "guardando la máquina viva como plantilla $name"
  "${K[@]}" save -replace -warm=false "$m" "$name"

  install -m 600 "$BUILD_TMP/password" "$sdir/password.new"
  mv -f "$sdir/password.new" "$sdir/password"
  {
    echo "ENGINE=ferretdb"
    echo "DBUSER=$role"
    echo "DBNAME=$db"
    echo "GUEST_IP=$ip"
    echo "TEMPLATE=$name"
  } > "$sdir/conn.env.new"
  mv -f "$sdir/conn.env.new" "$sdir/conn.env"

  BUILD_OK=1
  say "listo: plantilla $name (ferretdb)"
  say "  contraseña: $sdir/password (0600, solo en este host)"
  say "  una copia:  kling db up $name -name <copia>   ·   kling db connect <copia> -dsn"
}

case "${1:-}" in
  image) shift; cmd_image "$@" ;;
  build) shift; cmd_build "$@" ;;
  *) sed -n '2,42p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
