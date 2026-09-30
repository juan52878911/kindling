#!/usr/bin/env bash
# spec.sh — escribe el spec del constructor "android" del núcleo con los
# ficheros de ESTE checkout: el lanzador, la sonda de listo, el gancho de
# identidad, android-sh, el comprobador del kernel y, si se da el .dex,
# uidump. Es lo mismo que mete build-image.sh, pero la imagen la arma el
# daemon en Go (internal/android): sin Lima, debootstrap ni e2fsprogs.
#
#   prototypes/android/image/spec.sh > spec.json
#   kling image build android13 -builder android -spec spec.json
#
#   PHONED=0 ...                    el lanzador de bash (android-launch.sh)
#   KLING_PHONED=/ruta/kling-phoned binario linux/$ARCH de prototypes/android/phoned
#   UIDUMP_DEX=/ruta/kindling-uidump.dex   (uidump/build.sh; sin él, sin uidump)
#   ARCH=arm64|amd64  DATA_MODE=overlay|tmpfs  ANDROID_NET=veth|isolated|shared
#   SLIM=1  VERITY=0  FEC_ROOTS=2  BASE_NAME=android13-base
#   WIDTH HEIGHT DPI FPS EXTRA_ARGS ADB_SECURE PREP (como build-image.sh)
#
# Las rutas van como "src": el constructor lee los ficheros en el host del
# daemon. Un daemon de Linux corre los constructores como root y solo lee de
# /usr/local/lib/kindling o de los directorios de KLING_ANDROID_INPUTS (en su
# entorno); el de macOS, lo que pueda leer su usuario.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PROTO="$(cd "$HERE/.." && pwd)"
REPO="$(cd "$PROTO/../.." && pwd)"

ARCH="${ARCH:-$(uname -m)}"
case "$ARCH" in aarch64|arm64) ARCH=arm64 ;; x86_64|amd64) ARCH=amd64 ;; *) echo "spec.sh: ARCH must be arm64 or amd64" >&2; exit 1 ;; esac
PHONED="${PHONED:-1}"
ADB_SECURE="${ADB_SECURE:-$PHONED}"
PREP="${PREP:-$PHONED}"
DATA_MODE="${DATA_MODE:-overlay}"
DATA_SIZE="${DATA_SIZE:-2G}"
EXTRA_ARGS="${EXTRA_ARGS-androidboot.use_redroid_stream=1}"

die() { echo "spec.sh: $*" >&2; exit 1; }
# jstr: una cadena JSON (con \n, comillas y barras escapadas).
jstr() { local s="$1"; s="${s//\\/\\\\}"; s="${s//\"/\\\"}"; s="${s//$'\t'/\\t}"; s="${s//$'\n'/\\n}"; printf '"%s"' "$s"; }
jfile() { jstr "$(cat "$1")"$'\n'; }
lista() { sed -e 's/#.*//' -e 's/[[:space:]]*$//' -e '/^$/d' "$1"; }
files=()
add() { # add RUTA_EN_IMAGEN FICHERO [MODO]
  [ -f "$2" ] || die "missing $2"
  local m=""; [ -n "${3:-}" ] && m=", \"mode\": \"$3\""
  files+=("{\"path\": $(jstr "$1"), \"src\": $(jstr "$(cd "$(dirname "$2")" && pwd)/$(basename "$2")")$m}")
}

LIB=/usr/local/lib/kindling-android
add "$LIB/check-android-config.sh" "$PROTO/kernel/check-android-config.sh" 0755
add /usr/local/bin/android-sh "$HERE/android-sh" 0755
if [ "$PHONED" = 1 ]; then
  KLING_PHONED="${KLING_PHONED:-$REPO/kling-phoned}"
  [ -f "$KLING_PHONED" ] || die "no kling-phoned: build it (CGO_ENABLED=0 GOOS=linux GOARCH=$ARCH go build -o kling-phoned ./prototypes/android/phoned) or pass KLING_PHONED="
  [ -d "$HERE/kindling-phoned" ] || die "this checkout has no image/kindling-phoned (the kling-phoned hooks); use PHONED=0"
  add /usr/local/bin/kling-phoned "$KLING_PHONED" 0755
  add /etc/kindling/ready "$HERE/kindling-phoned/ready" 0755
  add /etc/kindling/post-restore.d/10-identity "$HERE/kindling-phoned/post-restore.d/10-identity" 0755
  SERVICE=/usr/local/bin/kling-phoned
else
  [ "$ADB_SECURE" = 0 ] || die "ADB_SECURE=1 needs PHONED=1"
  add "$LIB/android-launch.sh" "$HERE/android-launch.sh" 0755
  add /etc/kindling/ready "$HERE/kindling/ready" 0755
  add /etc/kindling/post-restore.d/10-identity "$HERE/kindling/post-restore.d/10-identity" 0755
  SERVICE="$LIB/android-launch.sh"
fi
if [ -n "${UIDUMP_DEX:-}" ]; then
  add /android/system/framework/kindling-uidump.dex "$UIDUMP_DEX" 0644
  add /android/system/etc/init/kindling-uidump.rc "$PROTO/uidump/kindling-uidump.rc" 0644
  add /usr/local/bin/uidump "$PROTO/uidump/uidump" 0755
fi

conf="\"ANDROID_WIDTH\": \"${WIDTH:-720}\", \"ANDROID_HEIGHT\": \"${HEIGHT:-1280}\", \"ANDROID_DPI\": \"${DPI:-320}\", \"ANDROID_FPS\": \"${FPS:-15}\""
conf+=", \"ANDROID_DATA_MODE\": \"$DATA_MODE\", \"ANDROID_DATA_SIZE\": \"$DATA_SIZE\", \"ANDROID_NET\": \"${ANDROID_NET:-veth}\""
conf+=", \"ANDROID_EXTRA_ARGS\": $(jstr "$EXTRA_ARGS")"
[ "$PHONED" = 1 ] && conf+=", \"ANDROID_ADB_SECURE\": \"$ADB_SECURE\", \"ANDROID_PREP\": \"$PREP\""

data=""
if [ "$DATA_MODE" = tmpfs ]; then
  case "$DATA_SIZE" in *G) mib=$(( ${DATA_SIZE%G} * 1024 )) ;; *M) mib=${DATA_SIZE%M} ;; *) die "DATA_SIZE must end in G or M" ;; esac
  data=", \"data_ext4_mib\": $mib"
fi
slim=""
if [ "${SLIM:-0}" = 1 ]; then
  S="$HERE/slim"
  svcs="$(lista "$S/services.txt" | awk '{printf "%s\"%s\"", (NR>1?", ":""), $0}')"
  apps="$(lista "$S/apps.txt" | awk '{printf "%s\"%s\"", (NR>1?", ":""), $0}')"
  slim=", \"slim\": {\"prop\": $(jfile "$S/slim.prop"), \"prop_name\": \"prototypes/android/image/slim/slim.prop\", \"services\": [$svcs], \"apps\": [$apps], \"features_xml\": $(jfile "$S/features.xml")}"
fi
verity=""
[ "${VERITY:-1}" = 0 ] && verity=", \"verity\": false"
[ -n "${FEC_ROOTS:-}" ] && verity+=", \"fec_roots\": $FEC_ROOTS"
base=""
[ -n "${BASE_NAME:-}" ] && base=", \"base_name\": $(jstr "$BASE_NAME")"

printf '{\n  "arch": "%s",\n  "service": %s,\n  "conf": {%s}%s%s%s%s,\n  "files": [\n    %s\n  ]\n}\n' \
  "$ARCH" "$(jstr "$SERVICE")" "$conf" "$data" "$slim" "$verity" "$base" \
  "$(IFS=$'\n'; printf '%s' "${files[*]}" | sed '$!s/$/,/' | sed '2,$s/^/    /')"
