#!/usr/bin/env bash
# kling db, Redis (#65): un Redis CALIENTE congelado como plantilla.
#
#   scripts/db-golden-redis.sh image                     construye la imagen redis
#   scripts/db-golden-redis.sh build [opciones] <nombre> arranca, carga y congela
#
# (o db-golden.sh image|build -engine redis ..., que llega aquí.)
#
# `build` arranca una microVM desde la imagen redis (Alpine; sin red, con
# exec), arranca redis-server escuchando en la IP del invitado y en un socket
# local, crea el usuario ACL de la aplicación, carga el seed, lo guarda en
# dump.rdb y guarda la máquina VIVA como plantilla <nombre> con la etiqueta
# kling.db.engine=redis: `kling db up <nombre>` da copias con el servidor ya
# caliente. Método y límites: docs/db-engines.md.
#
# Opciones de build:
#   -seed FILE        comandos de Redis, uno por línea (los ejecuta redis-cli
#                     como administrador), p.ej. SET clave valor
#   -seed-keys N      en vez de -seed: N claves sintéticas de ~100 bytes
#   -role U           usuario ACL de la aplicación (app)
#   -image I          imagen (redis)                  -mem M        RAM de la VM (512M)
#   -from T           plantilla con Redis instalado en vez de -image (macOS)
#   -cpus N           vCPUs (1)                       -state DIR    dónde va la contraseña
#   -keep             no borrar la máquina de preparación tras guardar
#
# Entorno: KLING = comando kling (con sus banderas, p.ej. "kling -H ssh://lab").
#
# LA CONTRASEÑA del usuario de la aplicación se genera aquí y se guarda SOLO
# en el host ($STATE/<nombre>/password, 0600). Al invitado va su SHA-256 (lo
# que Redis guarda: ACL SETUSER ... #<hash>), por stdin, nunca la clave.
#
# EL ADMINISTRADOR es el usuario default, que solo usan kling db y quien entre
# en la máquina: su clave se genera DENTRO del invitado y vive en
# /etc/kling-db/redis-admin (0600, root); no sale nunca. Cada copia la estrena
# al prepararse (kling db up/fork/reset). El usuario de la aplicación tiene
# +@all -@admin: ni CONFIG, ni ACL, ni SHUTDOWN, ni MODULE.
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

die() { echo "db-golden-redis: $*" >&2; exit 1; }
say() { echo "db-golden-redis: $*"; }

SEED_KEYS_MAX=5000000

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

# El cliente del administrador dentro de la máquina (el mismo que usa kling
# db): el usuario default por el socket local, con la clave en el entorno.
# Lo variable va por stdin.
# shellcheck disable=SC2016 # se expande en el invitado, no aquí
RC='c=$(command -v redis-cli || command -v valkey-cli) && REDISCLI_AUTH=$(cat /etc/kling-db/redis-admin) && export REDISCLI_AUTH && exec "$c" -s /run/redis/redis.sock'

cmd_image() {
  [ -f "$HERE/recipes/redis.recipe.json" ] || die "falta scripts/recipes/redis.recipe.json"
  say "construyendo la imagen redis (Alpine + redis); tarda unos minutos"
  "${K[@]}" image build redis -builder base -base min -grow 256 -spec "$HERE/recipes/redis.recipe.json"
  "${K[@]}" image recipe redis
}

cmd_build() {
  local seed="" seed_keys=0 role=app image=redis from=""
  local mem=512M cpus=1 state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}" name=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -seed)       seed="${2:?falta valor}"; shift 2 ;;
      -seed-keys)  seed_keys="${2:?falta valor}"; shift 2 ;;
      -role)       role="${2:?falta valor}"; shift 2 ;;
      -image)      image="${2:?falta valor}"; shift 2 ;;
      -from)       from="${2:?falta valor}"; shift 2 ;;
      -mem)        mem="${2:?falta valor}"; shift 2 ;;
      -cpus)       cpus="${2:?falta valor}"; shift 2 ;;
      -state)      state="${2:?falta valor}"; shift 2 ;;
      -keep)       BUILD_KEEP=1; shift ;;
      -migrations|-seed-mb|-database|-as-super|-template)
                   die "$1 no existe para Redis (no hay SQL): usa -seed con comandos de Redis" ;;
      -*)          die "opción desconocida: $1" ;;
      *)           [ -z "$name" ] || die "sobra un argumento: $1"; name="$1"; shift ;;
    esac
  done
  [ -n "$name" ] || die "uso: db-golden-redis.sh build [opciones] <nombre>"

  # Todo lo que acaba en un comando o en una línea de shell se valida aquí.
  [[ "$name" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "nombre no válido: $name"
  [[ "$role" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "usuario no válido: $role"
  [ "$role" != default ] || die "el usuario de la aplicación no puede ser default (es el administrador)"
  [[ "$image" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "imagen no válida: $image"
  [ -z "$from" ] || [[ "$from" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "plantilla no válida: $from"
  [[ "$mem" =~ ^[0-9]+[MG]?$ ]] || die "-mem no válido: $mem"
  [[ "$cpus" =~ ^[0-9]+$ ]] || die "-cpus no válido: $cpus"
  [[ "$seed_keys" =~ ^[0-9]+$ ]] || die "-seed-keys no válido: $seed_keys"
  [ "$seed_keys" -le "$SEED_KEYS_MAX" ] || die "-seed-keys máximo $SEED_KEYS_MAX: todo vive en la RAM de la copia"
  [ -z "$seed" ] || [ "$seed_keys" -eq 0 ] || die "-seed y -seed-keys son excluyentes"
  [ -z "$seed" ] || [ -f "$seed" ] || die "no existe el seed: $seed"

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

  # La etiqueta del motor viaja con la plantilla y la heredan las copias.
  local labels=(-label "kling.db.engine=redis" -label "kling.db.role=$role")
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

  say "configuración, usuarios ACL y arranque de redis-server en $ip (al invitado va el hash)"
  # Todo en la raíz de la máquina (overlay), NO en un volumen: Freeze desmonta
  # los volúmenes y Fork rechaza los de escritura. $hash son 64 hex y $role
  # pasó la validación: nada que escapar.
  "${K[@]}" exec -i -timeout 5m "$m" -- sh -s <<SETUP
set -eu
umask 077
BIN=\$(command -v redis-server || command -v valkey-server) || { echo "no hay redis-server en la imagen"; exit 1; }
CLI=\$(command -v redis-cli || command -v valkey-cli) || { echo "no hay redis-cli en la imagen"; exit 1; }
id redis >/dev/null 2>&1 || { echo "no hay usuario redis en la imagen"; exit 1; }
mkdir -p /etc/kling-db /run/redis /var/lib/redis /var/log/redis
chmod 755 /etc/kling-db
chown redis:redis /run/redis /var/lib/redis /var/log/redis
chmod 700 /run/redis /var/lib/redis /var/log/redis
# La clave del administrador: generada aquí dentro, solo la lee root.
N=\$(od -An -tx1 -N24 /dev/urandom | tr -d ' \n')
[ \${#N} -eq 48 ] || { echo "sin bytes aleatorios"; exit 1; }
printf %s "\$N" > /etc/kling-db/redis-admin
chmod 600 /etc/kling-db/redis-admin
H=\$(printf %s "\$N" | sha256sum | cut -d' ' -f1)
# El aclfile lo reescribe ACL SAVE (fichero temporal + rename en su
# directorio): va en el de datos, que es de redis.
printf 'user default on #%s ~* &* +@all\nuser %s on #%s ~* &* +@all -@admin\n' "\$H" "$role" "$hash" > /var/lib/redis/users.acl
chown redis:redis /var/lib/redis/users.acl
cat > /etc/kling-db/redis.conf <<'CONF'
bind $ip
port 6379
protected-mode yes
unixsocket /run/redis/redis.sock
unixsocketperm 700
daemonize no
dir /var/lib/redis
dbfilename dump.rdb
# Sin instantáneas automáticas ni AOF: la copia vive en la RAM de su microVM
# congelada; el dump.rdb del dorado solo sirve si el servidor se reinicia.
save ""
appendonly no
aclfile /var/lib/redis/users.acl
logfile /var/log/redis/redis.log
maxclients 100
enable-debug-command no
enable-module-command no
enable-protected-configs no
CONF
chmod 644 /etc/kling-db/redis.conf
# El servidor hereda las tuberías del exec: a fichero, sin stdin y en su
# propia sesión, o el exec no termina nunca.
setsid su -s /bin/sh redis -c "exec \$BIN /etc/kling-db/redis.conf" </dev/null >/var/log/redis/server.out 2>&1 &
export REDISCLI_AUTH="\$N"
i=0
until [ "\$("\$CLI" -s /run/redis/redis.sock PING 2>/dev/null)" = PONG ]; do
  i=\$((i + 1))
  [ "\$i" -lt 60 ] || { tail -n 30 /var/log/redis/server.out /var/log/redis/redis.log 2>/dev/null; exit 1; }
  sleep 1
done
SETUP

  # Ningún usuario sin contraseña, y el de la aplicación con exactamente su hash.
  local acl
  acl="$("${K[@]}" exec -i "$m" -- sh -c "$RC" <<<"ACL LIST")"
  if printf '%s\n' "$acl" | grep -q ' nopass'; then die "hay un usuario ACL sin contraseña"; fi
  # Redis 7 escribe "user app on sanitize-payload #<hash> ...": el orden de
  # los atributos no se da por sabido.
  local line
  line="$(printf '%s\n' "$acl" | grep "^user $role " || true)"
  case " $line " in
    *" on "*" #$hash "*) ;;
    *) die "el usuario $role no está activo con su hash" ;;
  esac
  [ "$(printf '%s\n' "$line" | grep -o ' #[0-9a-f]*' | wc -l | tr -d ' ')" -eq 1 ] || die "el usuario $role tiene más de una contraseña"
  [ "$(printf '%s\n' "$acl" | grep -c '^user ')" -eq 2 ] || die "sobran usuarios ACL"

  if [ -n "$seed" ]; then
    say "aplicando $(basename "$seed")"
    quiet "${K[@]}" exec "$m" -- mkdir -p /var/lib/dbgolden
    quiet "${K[@]}" cp "$seed" "$m:/var/lib/dbgolden/seed.redis"
    # Una línea que falla no para redis-cli: se cuentan los errores.
    local errs
    errs="$("${K[@]}" exec -timeout 30m "$m" -- sh -c "$RC < /var/lib/dbgolden/seed.redis 2>&1" | grep -Ec '^(\(error\) )?(ERR|WRONGTYPE|NOPERM|NOSCRIPT|BUSYKEY|EXECABORT) ' || true)"
    [ "$errs" = 0 ] || die "el seed dio $errs errores"
  elif [ "$seed_keys" -gt 0 ]; then
    say "$seed_keys claves sintéticas"
    local r
    r="$("${K[@]}" exec -i -timeout 30m "$m" -- sh -c "$RC" <<<"EVAL \"for i=1,tonumber(ARGV[1]) do redis.call('SET','seed:'..i,string.rep('x',100)) end return 1\" 0 $seed_keys")"
    [ "$r" = 1 ] || die "no se pudieron crear las claves sintéticas"
  fi

  say "SAVE y comprobaciones antes de congelar"
  [ "$("${K[@]}" exec -i "$m" -- sh -c "$RC" <<<"SAVE")" = OK ] || die "SAVE falló"

  # Un dorado no puede llevar clientes conectados: se repartirían a todas las
  # copias. CLIENT LIST enseña también la conexión que pregunta.
  local clients
  clients="$("${K[@]}" exec -i "$m" -- sh -c "$RC" <<<"CLIENT LIST" | grep -c '^id=' || true)"
  [ "$clients" -le 1 ] || die "quedan $((clients - 1)) conexiones de cliente abiertas; no se congela"

  local keys usage
  keys="$("${K[@]}" exec -i "$m" -- sh -c "$RC" <<<"DBSIZE")"
  usage="$("${K[@]}" exec "$m" -- sh -c "df -P / | awk 'NR==2 {gsub(\"%\",\"\",\$5); print \$5}'")"
  [[ "$usage" =~ ^[0-9]+$ ]] || die "no se pudo leer el uso del disco"
  [ "$usage" -lt 85 ] || die "el overlay está al ${usage}%: no queda margen para las copias"
  say "redis: $keys claves · disco del overlay al ${usage}%"

  quiet "${K[@]}" exec "$m" -- sh -c 'rm -rf /var/lib/dbgolden; sync'

  say "guardando la máquina viva como plantilla $name"
  "${K[@]}" save -replace -warm=false "$m" "$name"

  install -m 600 "$BUILD_TMP/password" "$sdir/password.new"
  mv -f "$sdir/password.new" "$sdir/password"
  {
    echo "ENGINE=redis"
    echo "DBUSER=$role"
    echo "GUEST_IP=$ip"
    echo "TEMPLATE=$name"
  } > "$sdir/conn.env.new"
  mv -f "$sdir/conn.env.new" "$sdir/conn.env"

  BUILD_OK=1
  say "listo: plantilla $name (redis)"
  say "  contraseña: $sdir/password (0600, solo en este host)"
  say "  una copia:  kling db up $name -name <copia>"
}

case "${1:-}" in
  image) shift; cmd_image "$@" ;;
  build) shift; cmd_build "$@" ;;
  *) sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
