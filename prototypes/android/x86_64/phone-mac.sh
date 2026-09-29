#!/bin/bash
# phone-mac.sh — los teléfonos del CT Linux (Firecracker) usados desde el Mac.
# docs/proxmox.md.
#
#   x86_64/phone-mac.sh up -n 5          phone.sh en el CT (cualquier orden: ls, pause,
#   x86_64/phone-mac.sh ls               resume, pool, rm, golden rebuild)
#   x86_64/phone-mac.sh adb <tel>        túnel ssh -L a su adb e imprime el serial del Mac
#   x86_64/phone-mac.sh view <tel>       el túnel y scrcpy
#   x86_64/phone-mac.sh unforward [<tel>]  cierra los túneles (todos, sin argumento)
#
# Nada se publica en la red: adb (sin autenticación) solo escucha en la IP
# privada de cada microVM dentro del CT, y el Mac llega por un túnel ssh que
# abre 127.0.0.1:<15000+N> en el Mac. El CT no reenvía nada en su IP de la LAN.
#
# Variables: PHONE_SSH (phones: el alias de ~/.ssh/config), PHONE_REMOTE
# (/usr/local/bin/phone, el envoltorio de phone.sh que deja docs/proxmox.md en
# el CT), PHONE_PORT_BASE (15000).
set -euo pipefail

SSH_HOST="${PHONE_SSH:-phones}"
REMOTE="${PHONE_REMOTE:-/usr/local/bin/phone}"
BASE="${PHONE_PORT_BASE:-15000}"
PREFIX="${PHONE_PREFIX:-phone}"

die() { printf 'phone-mac.sh: %s\n' "$*" >&2; exit 1; }
num() { case "$1" in ''|*[!0-9]*) echo "${1##*-}" ;; *) echo "$1" ;; esac; }
remote() { ssh "$SSH_HOST" "$REMOTE" "$@"; }

# forward TEL: abre (o reutiliza) el túnel a su adb e imprime 127.0.0.1:<puerto>.
forward() {
  local n p ep
  n="$(num "$1")"; p=$((BASE + n))
  case "$n" in ''|*[!0-9]*) die "phone name must end in a number: $1" ;; esac
  # host:puerto de adb dentro del CT (la IP de la microVM); vacío si no corre.
  ep="$(remote ls | awk -v m="$PREFIX-$n" '$1 == m && $2 == "running" { print $3 }')"
  [ -n "$ep" ] && [ "$ep" != - ] || die "$PREFIX-$n is not running (phone-mac.sh ls)"
  if ! lsof -nP -iTCP:"$p" -sTCP:LISTEN >/dev/null 2>&1; then
    ssh "$SSH_HOST" -o ExitOnForwardFailure=yes -f -N -L "127.0.0.1:$p:$ep"
  fi
  adb connect "127.0.0.1:$p" >/dev/null 2>&1 || true
  adb -s "127.0.0.1:$p" shell true >/dev/null 2>&1 || die "adb does not answer through 127.0.0.1:$p -> $ep"
  echo "127.0.0.1:$p"
}

unforward() {
  local lo=$((BASE + 1)) hi=$((BASE + 999)) p
  if [ $# -ge 1 ]; then lo=$((BASE + $(num "$1"))); hi=$lo; fi
  for p in $(lsof -nP -a -c ssh -iTCP -sTCP:LISTEN 2>/dev/null |
             awk -v lo="$lo" -v hi="$hi" '{ n = split($9, a, ":"); p = a[n] + 0; if (p >= lo && p <= hi) print p }' | sort -u); do
    adb disconnect "127.0.0.1:$p" >/dev/null 2>&1 || true
    pkill -f "ssh $SSH_HOST -o ExitOnForwardFailure=yes -f -N -L 127.0.0.1:$p:" || true
    echo "closed 127.0.0.1:$p"
  done
}

[ $# -ge 1 ] || { sed -n '2,11p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
c="$1"; shift
case "$c" in
  adb)
    [ $# -ge 1 ] || die "usage: phone-mac.sh adb <tel> [adb args...]"
    s="$(forward "$1")"; shift
    if [ $# -eq 0 ]; then echo "$s"; else exec adb -s "$s" "$@"; fi ;;
  view)
    [ $# -eq 1 ] || die "usage: phone-mac.sh view <tel>"
    command -v scrcpy >/dev/null || die "scrcpy is not installed (brew install scrcpy)"
    s="$(forward "$1")"
    exec scrcpy -s "$s" --window-title "$PREFIX-$(num "$1")" ;;
  unforward) unforward "$@" ;;
  *) remote "$c" "$@" ;;
esac
