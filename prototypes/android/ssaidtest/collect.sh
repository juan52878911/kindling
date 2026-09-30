#!/bin/bash
# collect.sh TEL... — instala ssaid-a.apk y ssaid-b.apk en cada teléfono por la
# API de kling-phoned (phone.sh api), los abre y escribe una línea por app:
#
#   <tel> <paquete> <android_id>
#
# El valor sale de logcat (etiqueta SSAIDTEST, buffer main): lo escribe la propia
# app con Settings.Secure.ANDROID_ID. Si ya están instalados los reinstala con -r
# (el SSAID no cambia: depende del paquete y de la firma). Variables: APKS (dir
# con los dos APK, por defecto este), PHONE (phone.sh). bash 3.2.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
APKS="${APKS:-$HERE}"
PHONE="${PHONE:-$HERE/../phone.sh}"
W="$(mktemp -d "${TMPDIR:-/tmp}/ssaid.XXXXXX")"
trap 'rm -rf "$W"' EXIT
install_only=0
[ "${1:-}" = --no-install ] && { install_only=1; shift; }
for v in a b; do
  base64 <"$APKS/ssaid-$v.apk" | tr -d '\n' >"$W/$v.b64"
  printf '{"package":"kindling.ssaidtest.%s"}' "$v" >"$W/$v.json"
done
for t in "$@"; do
  for v in a b; do
    if [ "$install_only" = 0 ]; then
      "$PHONE" api "$t" POST '/v1/install?encoding=base64' "$W/$v.b64" >/dev/null || { echo "$t install-$v FAIL" >&2; continue; }
    fi
    "$PHONE" api "$t" POST /v1/launch "$W/$v.json" >/dev/null || { echo "$t launch-$v FAIL" >&2; continue; }
  done
  sleep 3
  "$PHONE" api "$t" GET '/v1/logs?buffer=main&lines=2000' 2>/dev/null |
    awk -v t="$t" '/SSAIDTEST/ { for (i = 1; i < NF; i++) if ($i ~ /^kindling\.ssaidtest\.[ab]$/) last[$i] = $(i + 1) }
      END { for (p in last) print t, p, last[p] }' | sort
done
