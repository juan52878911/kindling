#!/bin/bash
# test-phoned.sh — prueba de extremo a extremo de kling-phoned (docs/phoned.md)
# con phone.sh, en el Mac (vz) o en Linux (Firecracker).
#
#   PHONE_ROOT=~/.kindling-phoned-test prototypes/android/test-phoned.sh
#   PHONE_SOCK=/run/kling-x.sock KLING=/ruta/kling TERMUX_APK=termux.apk prototypes/android/test-phoned.sh
#
# Necesita una imagen construida con PHONED=1 (el valor por defecto de
# build-image.sh). Hace, SIN allow_exec en ningún momento (el dorado se crea sin
# él: PHONE_EXEC=0):
#   1. dorado nuevo (golden rebuild) y 2 clones con una clave de adb propia cada uno;
#   2. la API por POST /machines/{ref}/guest: health, screen, tree, tap, swipe,
#      text, key, logs e install de un APK (TERMUX_APK) + launch;
#   3. identidad: serie, android_id y clave de SSAID distintas; adb con la clave
#      del clon entra, con la del otro clon o sin clave, no;
#   4. un tercer clon (borrando uno: máximo 2 VMs a la vez);
#   5. pause/thaw y freeze/thaw con la identidad intacta;
#   6. que los valores no estén en kling ps, los logs ni el dorado.
# Borra todos los teléfonos de esa raíz al empezar y al acabar. bash 3.2.
# shellcheck disable=SC2015,SC2013,SC2329  # ok || mal, como test-phone.sh
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PHONE="$HERE/phone.sh"
export PHONE_ROOT="${PHONE_ROOT:-$HOME/.kindling-phoned-test}"
SOCK="${PHONE_SOCK:-$PHONE_ROOT/kling.sock}"
export KLING_HOST="unix://$SOCK"
KLING="${KLING:-kling}"
export KLING
export PHONE_EXEC=0
OUT="${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/test-phoned.XXXXXX")}"
mkdir -p "$OUT/keys"
export PHONE_ADB_KEYS_DIR="$OUT/keys"
# Sin la clave del usuario: cada clon solo con la suya (abajo).
export PHONE_ADB_PUBKEY="$OUT/keys/none.pub"
fallos=0

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time * 1000'; }
secs() { perl -e 'printf "%.2f\n", ($ARGV[1] - $ARGV[0]) / 1000' "$1" "$2"; }
ok() { printf 'ok    %s\n' "$*"; }
mal() { printf 'FAIL  %s\n' "$*"; fallos=$((fallos + 1)); }
es_png() { [ "$(head -c 8 "$1" 2>/dev/null | od -An -tx1 | tr -d ' \n')" = 89504e470d0a1a0a ]; }
api() { "$PHONE" api "$@"; }
# jget CLAVE: un campo del JSON de stdin.
jget() { perl -MJSON::PP -e 'local $/; my $d = eval { decode_json(<STDIN>) } || {}; my $v = $d->{$ARGV[0]};
  print JSON::PP::is_bool($v) ? ($v ? 1 : 0) : ref $v ? encode_json($v) : ($v // "")' "$1"; }
adb_de() { "$PHONE" ls | awk -v n="$1" '$1 == n { print $3 }'; }

# adbk NOMBRE SERIAL ARGS...: adb con un servidor propio y SOLO la clave de NOMBRE
# (HOME aparte: el servidor de adb carga ~/.android/adbkey). "nokey": una clave
# nueva que ningún clon conoce.
adb_puerto() { case "$1" in phone-1) echo 5141 ;; phone-2) echo 5142 ;; phone-3) echo 5143 ;; *) echo 5149 ;; esac; }
adbk() {
  local who="$1" s="$2"; shift 2
  local h="$OUT/home-$who" p
  p="$(adb_puerto "$who")"
  if [ ! -f "$h/.android/adbkey" ]; then
    mkdir -p "$h/.android"
    if [ -f "$OUT/keys/$who" ]; then cp "$OUT/keys/$who" "$h/.android/adbkey"; cp "$OUT/keys/$who.pub" "$h/.android/adbkey.pub"; fi
  fi
  HOME="$h" ANDROID_USER_HOME="$h/.android" adb -P "$p" connect "$s" >/dev/null 2>&1
  sleep 0.5
  HOME="$h" ANDROID_USER_HOME="$h/.android" perl -e 'alarm shift; exec @ARGV' 20 adb -P "$p" -s "$s" "$@" 2>&1
}
adb_fin() { local w; for w in phone-1 phone-2 phone-3 nokey; do HOME="$OUT/home-$w" adb -P "$(adb_puerto "$w")" kill-server >/dev/null 2>&1; done; }
trap adb_fin EXIT

command -v adb >/dev/null || { echo "test-phoned.sh needs adb"; exit 2; }
for t in phone-1 phone-2 phone-3; do
  adb keygen "$OUT/keys/$t" >/dev/null 2>&1 || { echo "adb keygen failed"; exit 2; }
done
echo "raíz $PHONE_ROOT · socket $SOCK · salida $OUT · $(date '+%F %T')"
"$PHONE" rm -a >/dev/null 2>&1 || true

# ── 1. dorado y 2 clones ─────────────────────────────────────────────────────
t0="$(now_ms)"
if "$PHONE" golden rebuild >"$OUT/golden.txt" 2>&1; then ok "golden rebuild (sin allow_exec) en $(secs "$t0" "$(now_ms)") s"
else mal "golden rebuild"; tail -20 "$OUT/golden.txt"; exit 1; fi
grep -E 'boot_completed en|guardado en' "$OUT/golden.txt" | sed 's/^/        /'
"$KLING" template inspect -json phone-golden 2>/dev/null | grep -q '"allow_exec": *true' &&
  mal "el dorado tiene allow_exec" || ok "el dorado no tiene allow_exec"
t0="$(now_ms)"
if "$PHONE" up -n 2 >"$OUT/up.txt" 2>"$OUT/up.err"; then ok "up -n 2 en $(secs "$t0" "$(now_ms)") s"; sed 's/^/        /' "$OUT/up.txt"
else mal "up -n 2"; cat "$OUT/up.err"; exit 1; fi

# ── 2. la API ────────────────────────────────────────────────────────────────
for t in phone-1 phone-2; do
  h="$(api "$t" GET /v1/health)" && [ "$(printf '%s' "$h" | jget ok)" = 1 ] &&
    ok "$t: health $(printf '%s' "$h" | tr -d '\n' | cut -c1-200)" || mal "$t: health $h"
  t0="$(now_ms)"
  api "$t" GET '/v1/screen?encoding=base64' >"$OUT/$t.png"
  es_png "$OUT/$t.png" && ok "$t: screen $(secs "$t0" "$(now_ms)") s ($(wc -c <"$OUT/$t.png" | tr -d ' ') bytes)" || mal "$t: screen"
  t0="$(now_ms)"
  api "$t" GET /v1/tree >"$OUT/$t.xml"
  grep -q '<hierarchy' "$OUT/$t.xml" && ok "$t: tree $(secs "$t0" "$(now_ms)") s ($(wc -c <"$OUT/$t.xml" | tr -d ' ') bytes)" || mal "$t: tree"
done
printf '{"x":360,"y":640}' >"$OUT/tap.json"
printf '{"x1":360,"y1":1000,"x2":360,"y2":400,"ms":200}' >"$OUT/swipe.json"
printf '{"text":"hola kindling"}' >"$OUT/text.json"
printf '{"key":"HOME"}' >"$OUT/key.json"
for g in tap swipe text key; do
  t0="$(now_ms)"
  r="$(api phone-1 POST "/v1/$g" "$OUT/$g.json")" && ok "phone-1: $g $(secs "$t0" "$(now_ms)") s $r" || mal "phone-1: $g $r"
done
api phone-1 GET '/v1/logs?buffer=main&lines=20' >"$OUT/logs.txt" && [ -s "$OUT/logs.txt" ] && ok "phone-1: logs ($(wc -l <"$OUT/logs.txt" | tr -d ' ') líneas)" || mal "phone-1: logs"
if [ -f "${TERMUX_APK:-}" ]; then
  base64 <"$TERMUX_APK" | tr -d '\n' >"$OUT/apk.b64"
  t0="$(now_ms)"
  r="$(api phone-1 POST '/v1/install?encoding=base64' "$OUT/apk.b64")" &&
    ok "phone-1: install $(basename "$TERMUX_APK") ($(wc -c <"$TERMUX_APK" | tr -d ' ') bytes) en $(secs "$t0" "$(now_ms)") s: $r" || mal "phone-1: install $r"
  printf '{"package":"com.termux"}' >"$OUT/launch.json"
  r="$(api phone-1 POST /v1/launch "$OUT/launch.json")" && ok "phone-1: launch com.termux" || mal "phone-1: launch $r"
  sleep 2
  api phone-1 GET /v1/tree | grep -q 'com.termux' && ok "phone-1: Termux en pantalla (tree)" || mal "phone-1: Termux no aparece en el árbol"
else
  echo "        (sin TERMUX_APK: no se prueba install)"
fi

# ── 3. identidad y adb ───────────────────────────────────────────────────────
ident() {  # ident TEL → "serie android_id clave_ssaid"
  api "$1" GET /v1/identity | perl -MJSON::PP -e 'local $/; my $d = decode_json(<STDIN>);
    print join(" ", $d->{serial}, $d->{android_id}, $d->{ssaid_userkey_sha256} || "-", $d->{adb_keys}), "\n"'
}
: >"$OUT/ids.txt"
for t in phone-1 phone-2; do
  i="$(ident "$t")"; echo "$t $i" >>"$OUT/ids.txt"; ok "$t: identidad (serie, android_id, clave SSAID, claves adb) = $i"
done
s1="$(adb_de phone-1)"; s2="$(adb_de phone-2)"
v="$(adbk phone-1 "$s1" shell getprop ro.serialno | tr -d '\r')"
[ "$v" = "$(awk '$1 == "phone-1" { print $2 }' "$OUT/ids.txt")" ] && ok "adb a phone-1 con SU clave: ro.serialno=$v" || mal "adb phone-1 con su clave: $v"
v="$(adbk phone-2 "$s1" shell true)"
case "$v" in *unauthorized*|*"failed to authenticate"*) ok "adb a phone-1 con la clave de phone-2: rechazado";; *) mal "adb phone-1 con la clave de phone-2: '$v'";; esac
v="$(adbk nokey "$s2" shell true)"
case "$v" in *unauthorized*|*"failed to authenticate"*) ok "adb a phone-2 sin clave: rechazado";; *) mal "adb phone-2 sin clave: '$v'";; esac
v="$(adbk phone-2 "$s2" shell getprop ro.adb.secure | tr -d '\r')"
[ "$v" = 1 ] && ok "adb a phone-2 con SU clave: ro.adb.secure=1" || mal "adb phone-2 con su clave: $v"

# ── 4. tercer clon ───────────────────────────────────────────────────────────
"$PHONE" rm phone-2 >/dev/null && ok "rm phone-2"
if "$PHONE" up >"$OUT/up3.txt" 2>"$OUT/up3.err"; then
  sed 's/^/        /' "$OUT/up3.txt"
  t3="$(awk '{ print $1; exit }' "$OUT/up3.txt")"
  i="$(ident "$t3")"; echo "$t3 $i" >>"$OUT/ids.txt"; ok "$t3: identidad = $i"
else mal "tercer up"; cat "$OUT/up3.err"; fi
for c in 2 3 4; do
  n="$(awk -v c="$c" '{ print $c }' "$OUT/ids.txt" | sort -u | grep -vc '^-$')"
  [ "$n" -eq 3 ] && ok "3 clones, 3 valores distintos en la columna $c (serie/android_id/SSAID)" || mal "columna $c: $n distintos de 3"
done

# ── 5. pause/thaw y freeze/thaw ──────────────────────────────────────────────
antes="$(ident phone-1)"
"$KLING" pause phone-1 >/dev/null && ok "pause phone-1" || mal "pause phone-1"
sleep 1
t0="$(now_ms)"
"$KLING" thaw phone-1 >/dev/null
t1="$(now_ms)"
api phone-1 GET '/v1/screen?encoding=base64' >"$OUT/thaw.png"
es_png "$OUT/thaw.png" && ok "pause→thaw $(secs "$t0" "$t1") s, thaw→screen por la API $(secs "$t0" "$(now_ms)") s" || mal "screen tras thaw"
hs="$("$KLING" ps -json | perl -MJSON::PP -e 'local $/; for (@{decode_json(<STDIN>)}) { print $_->{has_secrets} ? "si" : "no" if $_->{name} eq "phone-1" }')"
[ "$hs" = no ] && ok "phone-1 sin marca de secretos" || mal "phone-1 con marca de secretos ($hs)"
if "$KLING" freeze phone-1 >"$OUT/freeze.txt" 2>&1; then ok "freeze phone-1"; else mal "freeze phone-1: $(head -c 200 "$OUT/freeze.txt")"; fi
t0="$(now_ms)"
"$KLING" thaw phone-1 >/dev/null 2>&1 && "$KLING" machine ready phone-1 -wait 60s >/dev/null 2>&1
t1="$(now_ms)"
api phone-1 GET /v1/health >/dev/null && ok "freeze→thaw y listo en $(secs "$t0" "$t1") s; health ok" || mal "health tras freeze/thaw"
despues="$(ident phone-1)"
[ "$antes" = "$despues" ] && ok "phone-1 conserva su identidad tras pause y freeze ($despues)" || mal "identidad antes '$antes' después '$despues'"
v="$(adbk phone-1 "$(adb_de phone-1)" shell getprop ro.serialno | tr -d '\r')"
[ "$v" = "$(echo "$despues" | cut -d' ' -f1)" ] && ok "adb con su clave tras freeze/thaw" || mal "adb tras freeze/thaw: $v"

# ── 6. los valores no se filtran ─────────────────────────────────────────────
fuga=0
for v in $(awk '{ print $2; print $3 }' "$OUT/ids.txt"); do
  "$KLING" ps -json 2>/dev/null | grep -q "$v" && { mal "$v en kling ps -json"; fuga=1; }
  for t in phone-1 "$t3"; do "$KLING" logs "$t" 2>/dev/null | grep -q "$v" && { mal "$v en kling logs $t"; fuga=1; }; done
  grep -rqs "$v" "$PHONE_ROOT"/daemon.log "$PHONE_ROOT"/machines/*/*.log 2>/dev/null && { mal "$v en logs de $PHONE_ROOT"; fuga=1; }
  for f in "$PHONE_ROOT"/snapshots/phone-golden/*; do
    [ -f "$f" ] || continue
    LC_ALL=C grep -q -a -F "$v" "$f" && { mal "$v en el dorado ($f)"; fuga=1; }
  done
done
[ "$fuga" = 0 ] && ok "ni series ni android_id en kling ps, kling logs, los logs del daemon ni el dorado"
"$KLING" logs phone-1 2>/dev/null | grep -a 'identity applied' | tail -n 1 | sed 's/^/        /'

"$PHONE" rm -a >/dev/null
echo
if [ "$fallos" -eq 0 ]; then echo "PASS (salida en $OUT)"; exit 0; fi
echo "FAIL: $fallos fallo(s) (salida en $OUT)"; exit 1
