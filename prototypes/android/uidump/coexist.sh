#!/bin/bash
# Comprueba, DENTRO de la VM, que uidump no estorba a `uiautomator` ni a
# `am instrument` (docs/uidump.md, "Convivencia"). Con la APK de itest/build.sh:
#
#   kling cp itest.apk <m>:/tmp/itest.apk
#   kling cp coexist.sh <m>:/tmp/coexist.sh
#   kling exec <m> -- bash /tmp/coexist.sh [N=30]
#
# Lanza N veces al rival, primero justo tras un uidump y luego mientras un bucle
# de `uidump` corre sin pausa, y cuenta cuántas acaba bien. Sale 1 si alguna
# cuenta no es N/N.
N=${1:-30}
APK=${ITEST_APK:-/tmp/itest.apk}
fallos=0

if ! android-sh 'pm list instrumentation' | grep -q kindling.itest; then
  android-sh --push "$APK" /data/local/tmp/itest.apk &&
    android-sh 'pm install -r /data/local/tmp/itest.apk' >/dev/null ||
    { echo "coexist: cannot install the test APK ($APK)" >&2; exit 2; }
fi

bucle() {
  rm -f /tmp/coexist.stop
  ( while [ ! -e /tmp/coexist.stop ]; do uidump >/dev/null 2>&1; done ) &
  BG=$!
  sleep 1
}
para() { touch /tmp/coexist.stop; wait "$BG" 2>/dev/null; rm -f /tmp/coexist.stop; }

# cuenta ETIQUETA PATRON COMANDO [PREVIO]: N veces COMANDO en Android; bien si
# su salida trae PATRON. PREVIO (un comando de la VM) va antes de cada una.
cuenta() {
  local ok=0 i out
  for ((i = 1; i <= N; i++)); do
    [ -z "${4:-}" ] || $4 >/dev/null 2>&1
    out="$(android-sh "$3" 2>&1 | tr -d '\r')"
    case "$out" in *"$2"*) ok=$((ok + 1)) ;; esac
  done
  echo "$1: $ok/$N"
  [ "$ok" = "$N" ] || fallos=$((fallos + 1))
}

uidump start >/dev/null 2>&1
INSTR='am instrument -w kindling.itest/kindling.itest.T'
cuenta "am instrument tras un uidump" "ITEST OK" "$INSTR" uidump
cuenta "uiautomator dump tras un uidump" "dumped to" 'uiautomator dump /sdcard/coexist.xml' uidump
bucle
cuenta "am instrument con uidump en bucle" "ITEST OK" "$INSTR"
cuenta "uiautomator dump con uidump en bucle" "dumped to" 'uiautomator dump /sdcard/coexist.xml'
para
exit $((fallos > 0))
