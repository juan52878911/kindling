#!/usr/bin/env bash
# kling db, almacén de objetos S3 con Garage (#115): un Garage CALIENTE
# congelado como plantilla.
#
#   scripts/db-golden-garage.sh image [-tag v1.1.0]       importa la imagen dxflrs/garage
#   scripts/db-golden-garage.sh build [opciones] <nombre> arranca, carga y congela
#
# (o db-golden.sh image|build -engine garage ..., que llega aquí.)
#
# La imagen es la oficial de Docker (dxflrs/garage, scratch: un binario estático
# y nada más), importada sin Docker con kling image import. No tiene shell: el
# servicio es `/garage -c /etc/garage.toml server`, que reintenta hasta que
# build le copia la configuración (kling cp), y toda orden de administración
# va por argv directo (kling exec ... -- /garage ...).
# `build` arranca una microVM desde ella (sin red, con exec), le pone la
# configuración, asigna el layout de un nodo, crea el bucket de semilla con
# N objetos, y guarda la máquina VIVA como plantilla <nombre> con la etiqueta
# kling.db.engine=garage: `kling db up <nombre>` da copias con el servidor ya
# caliente. Método y límites: docs/db-engines.md.
#
# Opciones de build:
#   -seed-objects N   objetos en el bucket de semilla (50)   -object-kb K  tamaño de cada uno (4)
#   -bucket B         el bucket (appdb)  -role U  nombre de la clave (app)
#   -image I          imagen (garage)    -mem M   RAM de la VM (512M)
#   -cpus N           vCPUs (2)          -state DIR  dónde va el secreto   -disk D  disco (2G)
#   -keep             no borrar la máquina de preparación tras guardar
#
# EL SECRETO: SigV4 es un HMAC y Garage necesita el secreto, no un hash. Se
# genera aquí y se guarda en el host ($STATE/<nombre>/password, 0600); el
# identificador de acceso sale de él (GK + sha256, ver S3AccessKey en
# ext/db/internal/dbstate). Cada copia estrena su propio par y borra el del
# dorado: la clave del dorado no vale en ninguna copia.
# El cliente de la carga es curl --aws-sigv4 desde el host (la máquina de
# preparación no tiene red hacia fuera, pero el host llega a su IP); la clave
# va a curl por -K (stdin), no por argv. Hace falta Linux (la IP del invitado
# es alcanzable) y curl >= 7.75.
set -euo pipefail
umask 077

KLING="${KLING:-kling}"
# shellcheck disable=SC2206 # KLING puede llevar banderas: se parte a propósito
K=($KLING)

die() { echo "db-golden-garage: $*" >&2; exit 1; }
say() { echo "db-golden-garage: $*"; }

OBJECTS_MAX=100000

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

# sha256_hex: el SHA-256 en hexadecimal (minúsculas) de stdin.
sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 | cut -d' ' -f1
  elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 -r | cut -d' ' -f1
  else die "hace falta sha256sum, shasum u openssl en el host"
  fi
}

# access_key <secreto>: lo mismo que dbstate.S3AccessKey.
access_key() {
  local h
  h="$(printf 'kling-db-s3-access\n%s' "$1" | sha256_hex)"
  [[ "$h" =~ ^[0-9a-f]{64}$ ]] || die "no se pudo calcular la clave de acceso"
  printf 'GK%s' "${h:0:24}"
}

# La orden de administración dentro de la máquina (sin shell: argv directo).
G=(/garage -c /etc/garage.toml)

cmd_image() {
  local tag=v1.1.0
  if [ "${1:-}" = -tag ]; then tag="${2:?falta valor}"; fi
  [[ "$tag" =~ ^v?[0-9]+(\.[0-9]+){0,3}$ ]] || die "-tag no válido: $tag (v1.1.0, v2.0.0...)"
  say "importando dxflrs/garage:$tag como garage (el servicio reintenta hasta que build le dé la configuración)"
  "${K[@]}" image import "dxflrs/garage:$tag" -name garage -replace -entrypoint /garage -- -c /etc/garage.toml server
}

cmd_build() {
  local seed_objects=50 object_kb=4 role=app bucket=appdb image=garage
  local mem=512M cpus=2 disk=2G state="${KLING_DB_STATE:-$HOME/.local/state/kling-db}" name=""
  while [ $# -gt 0 ]; do
    case "$1" in
      -seed-objects) seed_objects="${2:?falta valor}"; shift 2 ;;
      -object-kb)    object_kb="${2:?falta valor}"; shift 2 ;;
      -role)         role="${2:?falta valor}"; shift 2 ;;
      -bucket|-database) bucket="${2:?falta valor}"; shift 2 ;;
      -image)        image="${2:?falta valor}"; shift 2 ;;
      -mem)          mem="${2:?falta valor}"; shift 2 ;;
      -cpus)         cpus="${2:?falta valor}"; shift 2 ;;
      -disk)         disk="${2:?falta valor}"; shift 2 ;;
      -state)        state="${2:?falta valor}"; shift 2 ;;
      -keep)         BUILD_KEEP=1; shift ;;
      -migrations|-seed|-seed-mb|-seed-rows|-as-super|-template|-from)
                     die "$1 no existe para Garage: usa -seed-objects, o carga los objetos en una copia y guárdala" ;;
      -*)            die "opción desconocida: $1" ;;
      *)             [ -z "$name" ] || die "sobra un argumento: $1"; name="$1"; shift ;;
    esac
  done
  [ -n "$name" ] || die "uso: db-golden-garage.sh build [opciones] <nombre>"

  # Todo lo que acaba en una orden se valida aquí.
  [[ "$name" =~ ^[a-z0-9][a-z0-9_-]{0,40}$ ]] || die "nombre no válido: $name"
  [[ "$role" =~ ^[a-z_][a-z0-9_]{0,30}$ ]] || die "nombre de clave no válido: $role"
  [[ "$bucket" =~ ^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$ ]] || die "bucket no válido: $bucket (3-63: minúsculas, dígitos y '-')"
  [[ "$image" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]] || die "imagen no válida: $image"
  [[ "$mem" =~ ^[0-9]+[MG]?$ ]] || die "-mem no válido: $mem"
  [[ "$disk" =~ ^[0-9]+[MG]?$ ]] || die "-disk no válido: $disk"
  [[ "$cpus" =~ ^[0-9]+$ ]] || die "-cpus no válido: $cpus"
  [[ "$seed_objects" =~ ^[0-9]+$ ]] || die "-seed-objects no válido: $seed_objects"
  [ "$seed_objects" -le "$OBJECTS_MAX" ] || die "-seed-objects máximo $OBJECTS_MAX"
  [[ "$object_kb" =~ ^[0-9]+$ ]] && [ "$object_kb" -ge 1 ] && [ "$object_kb" -le 1024 ] || die "-object-kb entre 1 y 1024"
  command -v curl >/dev/null 2>&1 || die "hace falta curl en el host (carga de los objetos)"

  local m="$name-build" sdir="$state/$name"
  mkdir -p "$sdir"
  chmod 700 "$state" "$sdir"
  BUILD_TMP="$(mktemp -d)"
  BUILD_M="$m"
  trap cleanup EXIT
  trap 'exit 130' INT
  trap 'exit 143' TERM

  "${K[@]}" rm -f "$m" >/dev/null 2>&1 || true

  # El secreto: 256 bits en hexadecimal (64), a un fichero 0600.
  od -An -tx1 -N32 /dev/urandom | tr -d ' \n' > "$BUILD_TMP/password"
  [ "$(wc -c < "$BUILD_TMP/password" | tr -d ' ')" -eq 64 ] || die "no se pudo generar el secreto"
  local secret ak rpc
  secret="$(cat "$BUILD_TMP/password")"
  ak="$(access_key "$secret")"
  rpc="$(od -An -tx1 -N32 /dev/urandom | tr -d ' \n')"

  local labels=(-label "kling.db.engine=garage" -label "kling.db.role=$role" -label "kling.db.database=$bucket")
  say "arrancando $m desde la imagen $image (egress none)"
  "${K[@]}" run -name "$m" -image "$image" -egress none -allow-exec \
    -cpus "$cpus" -mem "$mem" -disk "$disk" -cpu-pct 100 "${labels[@]}" >/dev/null

  local i
  for i in $(seq 1 60); do
    if "${K[@]}" exec -timeout 5s "$m" -- "${G[0]}" --version >/dev/null 2>&1; then break; fi
    [ "$i" -lt 60 ] || die "el agente del invitado no contesta"
    sleep 1
  done

  # La IP del invitado, del listado JSON del núcleo (la imagen scratch no trae
  # cat para leer /proc/cmdline).
  local ip
  ip="$("${K[@]}" ps -json 2>/dev/null | grep -o "\"name\":\"$m\"[^}]*" | sed -n 's/.*"ip":"\([0-9.]*\)".*/\1/p' | head -n1)"
  [[ "$ip" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "no encuentro la IP del invitado (kling ps -json)"

  say "configuración de Garage y layout de un nodo ($ip)"
  cat > "$BUILD_TMP/garage.toml" <<TOML
metadata_dir = "/var/lib/garage/meta"
data_dir = "/var/lib/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "127.0.0.1:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "$rpc"

[s3_api]
s3_region = "garage"
api_bind_addr = "0.0.0.0:3900"
root_domain = ".s3.garage"

[admin]
api_bind_addr = "127.0.0.1:3903"
TOML
  "${K[@]}" cp "$BUILD_TMP/garage.toml" "$m:/etc/garage.toml" >/dev/null

  for i in $(seq 1 90); do
    if "${K[@]}" exec -timeout 10s "$m" -- "${G[@]}" status >/dev/null 2>&1; then break; fi
    [ "$i" -lt 90 ] || { "${K[@]}" logs "$m" 2>&1 | tail -n 20 >&2; die "Garage no arranca"; }
    sleep 1
  done
  local node
  node="$("${K[@]}" exec -timeout 10s "$m" -- "${G[@]}" status 2>/dev/null | awk '/127\.0\.0\.1:3901/{print $1; exit}')"
  [[ "$node" =~ ^[0-9a-f]{16}$ ]] || die "no encuentro el id del nodo"
  quiet "${K[@]}" exec -timeout 30s "$m" -- "${G[@]}" layout assign -z dc1 -c 1G "$node"
  quiet "${K[@]}" exec -timeout 30s "$m" -- "${G[@]}" layout apply --version 1

  say "bucket $bucket y clave $role (el secreto se queda en el host)"
  for i in $(seq 1 30); do
    if "${K[@]}" exec -timeout 20s "$m" -- "${G[@]}" bucket create "$bucket" >/dev/null 2>&1; then break; fi
    [ "$i" -lt 30 ] || die "no se pudo crear el bucket"
    sleep 1
  done
  quiet "${K[@]}" exec -timeout 30s "$m" -- "${G[@]}" key import --yes -n "$role" "$ak" "$secret" ||
    die "no se pudo importar la clave (salida omitida)"
  quiet "${K[@]}" exec -timeout 30s "$m" -- "${G[@]}" bucket allow --read --write --owner "$bucket" --key "$ak"

  if [ "$seed_objects" -gt 0 ]; then
    say "cargando $seed_objects objetos de ${object_kb} KiB en $bucket"
    printf 'user = "%s:%s"\n' "$ak" "$secret" > "$BUILD_TMP/curl.cfg"
    head -c "$((object_kb * 1024))" /dev/zero | tr '\0' 'x' > "$BUILD_TMP/obj"
    local empty=e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855 body h n
    for i in $(seq 1 "$seed_objects"); do
      # El contenido de cada objeto lleva su número: no hay dos iguales.
      { printf 'seed object %05d\n' "$i"; cat "$BUILD_TMP/obj"; } > "$BUILD_TMP/obj.$i"
      h="$(sha256_hex < "$BUILD_TMP/obj.$i")"
      # Garage exige x-amz-content-sha256; curl lo firma si va como cabecera.
      curl -sS --max-time 30 -K "$BUILD_TMP/curl.cfg" --aws-sigv4 "aws:amz:garage:s3" -H "x-amz-content-sha256: $h" \
        -o "$BUILD_TMP/resp" -w '%{http_code}' -X PUT --data-binary "@$BUILD_TMP/obj.$i" \
        "http://$ip:3900/$bucket/seed/obj-$(printf '%05d' "$i").txt" > "$BUILD_TMP/code" || true
      [ "$(cat "$BUILD_TMP/code")" = 200 ] || { head -c 400 "$BUILD_TMP/resp" >&2; echo >&2; die "falló la carga del objeto $i (HTTP $(cat "$BUILD_TMP/code"))"; }
      rm -f "$BUILD_TMP/obj.$i"
    done
    body="$(curl -sS --fail --max-time 30 -K "$BUILD_TMP/curl.cfg" --aws-sigv4 "aws:amz:garage:s3" -H "x-amz-content-sha256: $empty" \
      "http://$ip:3900/$bucket?list-type=2&max-keys=1000")" || die "falló el listado del bucket"
    n="$(printf '%s' "$body" | grep -o '<Key>' | wc -l | tr -d ' ')"
    [ "$n" -eq "$seed_objects" ] || die "el bucket tiene $n objetos, no $seed_objects"
  fi

  quiet "${K[@]}" exec -timeout 30s "$m" -- "${G[@]}" key list
  say "guardando la máquina viva como plantilla $name"
  "${K[@]}" save -replace -warm=false "$m" "$name"

  install -m 600 "$BUILD_TMP/password" "$sdir/password.new"
  mv -f "$sdir/password.new" "$sdir/password"
  {
    echo "ENGINE=garage"
    echo "DBUSER=$role"
    echo "DBNAME=$bucket"
    echo "GUEST_IP=$ip"
    echo "ACCESS_KEY=$ak"
    echo "TEMPLATE=$name"
  } > "$sdir/conn.env.new"
  mv -f "$sdir/conn.env.new" "$sdir/conn.env"

  BUILD_OK=1
  say "listo: plantilla $name (garage, $seed_objects objetos en $bucket)"
  say "  secreto: $sdir/password (0600, solo en este host)"
  say "  una copia:  kling db up $name -name <copia>   ·   kling db connect <copia> -dsn"
}

case "${1:-}" in
  image) shift; cmd_image "$@" ;;
  build) shift; cmd_build "$@" ;;
  *) sed -n '2,36p' "$0" | sed 's/^# \{0,1\}//'; exit 2 ;;
esac
