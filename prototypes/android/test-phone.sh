#!/bin/bash
# test-phone.sh — prueba de extremo a extremo de phone.sh en el Mac.
#
#   prototypes/android/test-phone.sh
#
# Hace: up de 2 teléfonos (crea el dorado si falta), adb a cada uno,
# screencap por adb, identidades distintas (y una tercera tras borrar uno),
# pause/resume con los puertos nuevos, que el secreto no aparezca en `kling
# ps`, en los logs ni en el dorado, que un teléfono con identidad no se deje
# guardar, y rm. Imprime los tiempos y sale con 1 si algo falla.
#
# Usa el daemon y la raíz de phone.sh (PHONE_ROOT); no toca otros teléfonos
# salvo que se llamen como los que crea (borra todos los teléfonos al empezar
# y al acabar: úsalo con una raíz de pruebas). Como mucho 2 VMs a la vez.
# bash 3.2.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
PHONE="$HERE/phone.sh"
export PHONE_ROOT="${PHONE_ROOT:-$HOME/.kindling-android-telefono}"
export KLING_HOST="unix://$PHONE_ROOT/kling.sock"
KLING="${KLING:-kling}"
OUT="${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/test-phone.XXXXXX")}"
mkdir -p "$OUT"
mkdir -p "$OUT"
fallos=0

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time * 1000'; }
secs() { perl -e 'printf "%.2f\n", ($ARGV[1] - $ARGV[0]) / 1000' "$1" "$2"; }
ok() { printf 'ok    %s\n' "$*"; }
mal() { printf 'FAIL  %s\n' "$*"; fallos=$((fallos + 1)); }
es_png() { [ "$(head -c 8 "$1" 2>/dev/null | od -An -tx1 | tr -d ' \n')" = 89504e470d0a1a0a ]; }
puerto() { "$PHONE" ls | awk -v n="$1" '$1 == n { sub(/^127\.0\.0\.1:/, "", $3); print $3 }'; }
id_de() { adb -s "$1" shell settings get secure android_id 2>/dev/null | tr -d '\r'; }
# Los serials usados, para comprobar al final que rm los olvida en adb (el
# servidor de adb es de todo el Mac: otros teléfonos pueden estar en él).
serial_de() { local s; s="$("$PHONE" adb "$1" 2>/dev/null)" || return 1; echo "$s" >>"$OUT/serials.txt"; echo "$s"; }

command -v adb >/dev/null || { echo "test-phone.sh needs adb (brew install android-platform-tools)"; exit 2; }
echo "raíz $PHONE_ROOT · salida $OUT · $(date '+%F %T') · kern.memorystatus_level=$(sysctl -n kern.memorystatus_level)"

"$PHONE" rm -a >/dev/null 2>&1 || true

# ── 1. up de 2 ───────────────────────────────────────────────────────────────
t0="$(now_ms)"
if "$PHONE" up -n 2 >"$OUT/up.txt" 2>"$OUT/up.err"; then
  ok "up -n 2 en $(secs "$t0" "$(now_ms)") s"
  sed 's/^/        /' "$OUT/up.txt"
else
  mal "up -n 2"; cat "$OUT/up.err"; exit 1
fi
"$PHONE" ls | sed 's/^/        /'

# ── 2. adb, screencap e identidad de cada uno ────────────────────────────────
ids=""
for t in phone-1 phone-2; do
  s="$(serial_de "$t")" || { mal "$t: adb serial"; continue; }
  v="$(adb -s "$s" shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')"
  [ "$v" = 1 ] && ok "$t: adb $s, boot_completed=1" || mal "$t: adb shell getprop ($v)"
  t0="$(now_ms)"
  adb -s "$s" exec-out screencap -p >"$OUT/$t.png" 2>/dev/null
  if es_png "$OUT/$t.png"; then ok "$t: screencap por adb $(secs "$t0" "$(now_ms)") s ($(wc -c <"$OUT/$t.png" | tr -d ' ') bytes)"
  else mal "$t: screencap por adb"; fi
  id="$(id_de "$s")"
  echo "$id" >>"$OUT/ids.txt"
  ok "$t: android_id $id, device_name $(adb -s "$s" shell settings get global device_name 2>/dev/null | tr -d '\r')"
  ids="$ids $id"
done

# ── 3. pause / resume (los puertos cambian) ─────────────────────────────────
p_antes="$(puerto phone-1)"
"$PHONE" pause phone-1 >/dev/null && ok "pause phone-1" || mal "pause phone-1"
st="$("$PHONE" ls | awk '$1 == "phone-1" { print $2 }')"
[ "$st" = paused ] && ok "phone-1 está $st" || mal "phone-1 está '$st' tras pause"
if r="$("$PHONE" resume phone-1 2>&1)"; then ok "resume: $r"; else mal "resume: $r"; fi
p_despues="$(puerto phone-1)"
echo "        puerto adb antes $p_antes, después $p_despues"
t0="$(now_ms)"
s="$(serial_de phone-1)"
adb -s "$s" exec-out screencap -p >"$OUT/phone-1-resume.png" 2>/dev/null
if es_png "$OUT/phone-1-resume.png"; then ok "screencap por adb tras resume $(secs "$t0" "$(now_ms)") s"
else mal "screencap tras resume"; fi
# Tres ciclos más, solo tiempos: thaw y thaw→screencap por adb.
for i in 1 2 3; do
  "$PHONE" pause phone-1 >/dev/null
  sleep 1
  t0="$(now_ms)"
  "$KLING" thaw phone-1 >/dev/null
  t1="$(now_ms)"
  s="$(serial_de phone-1)"
  adb -s "$s" exec-out screencap -p >"$OUT/thaw-$i.png" 2>/dev/null
  t2="$(now_ms)"
  if es_png "$OUT/thaw-$i.png"; then ok "ciclo $i: thaw $(secs "$t0" "$t1") s, thaw→screencap por adb $(secs "$t0" "$t2") s"
  else mal "ciclo $i: screencap tras thaw"; fi
done

# ── 4. tras aplicar la identidad ─────────────────────────────────────────────
# Con la capacidad `ready` (núcleo nuevo) el gancho de la imagen consumió el
# secreto y el daemon levantó la marca: freeze y thaw se aceptan y la identidad
# sigue ahí. Con kling v0.16 el teléfono queda marcado y save se niega.
if "$KLING" info 2>/dev/null | grep '^capabilities:' | tr ', ' '\n\n' | grep -qx ready; then
  hs="$("$KLING" ps -json 2>/dev/null | perl -MJSON::PP -e 'local $/; for (@{decode_json(<STDIN>)}) { print $_->{has_secrets} ? "si" : "no" if $_->{name} eq "phone-2" }')"
  [ "$hs" = no ] && ok "phone-2 sin marca de secretos tras aplicar la identidad" || mal "phone-2 sigue marcado con secretos ($hs)"
  "$KLING" ps | sed 's/^/        /'
  "$KLING" ps | head -n 1 | grep -qw READY && ok "kling ps enseña la columna READY" || mal "kling ps sin columna READY"
  s="$("$PHONE" adb phone-2)"
  id2_antes="$(id_de "$s")"
  # El puerto de adb cambia al descongelar: que el servidor de adb olvide el viejo.
  adb disconnect "$s" >/dev/null 2>&1
  if "$KLING" freeze phone-2 >"$OUT/freeze.txt" 2>&1; then ok "freeze phone-2 aceptado"
  else mal "freeze phone-2: $(head -c 120 "$OUT/freeze.txt")"; fi
  t0="$(now_ms)"
  if "$KLING" thaw phone-2 >"$OUT/thaw2.txt" 2>&1 && "$KLING" machine ready phone-2 -wait 60s >/dev/null 2>&1; then
    ok "thaw phone-2 y listo en $(secs "$t0" "$(now_ms)") s"
  else mal "thaw phone-2: $(head -c 120 "$OUT/thaw2.txt")"; fi
  s="$(serial_de phone-2)"
  id2="$(id_de "$s")"
  [ -n "$id2" ] && [ "$id2" = "$id2_antes" ] && ok "phone-2 conserva su android_id tras freeze/thaw ($id2)" ||
    mal "android_id de phone-2 antes '$id2_antes', después '$id2'"
else
  if "$KLING" save phone-2 test-phone-leak >"$OUT/save.txt" 2>&1; then
    mal "kling save de un teléfono con secreto NO se negó"; "$KLING" template rm -f test-phone-leak >/dev/null 2>&1
  else ok "kling save phone-2 se niega: $(head -c 90 "$OUT/save.txt")…"; fi
fi

# ── 5. tercera identidad: borrar uno y crear otro (máx. 2 VMs a la vez) ─────
"$PHONE" rm phone-2 >/dev/null && ok "rm phone-2"
if "$PHONE" up >"$OUT/up3.txt" 2>"$OUT/up3.err"; then
  sed 's/^/        /' "$OUT/up3.txt"
  t3="$(awk '{ print $1; exit }' "$OUT/up3.txt")"
  s="$(serial_de "$t3")"
  id="$(id_de "$s")"; echo "$id" >>"$OUT/ids.txt"; ids="$ids $id"
  ok "$t3: android_id $id"
else mal "tercer up"; cat "$OUT/up3.err"; fi
n="$(sort -u "$OUT/ids.txt" | grep -c '^[0-9a-f]\{16\}$')"
[ "$n" -eq 3 ] && ok "3 clones, 3 android_id distintos:$ids" || mal "android_id distintos: $n de 3 ($ids)"

# ── 6. el secreto no se filtra ───────────────────────────────────────────────
fuga=0
for id in $ids; do
  "$KLING" ps -json 2>/dev/null | grep -q "$id" && { mal "android_id $id en kling ps -json"; fuga=1; }
  grep -rqs "$id" "$PHONE_ROOT/daemon.log" "$PHONE_ROOT"/machines/*/*.log && { mal "android_id $id en logs de $PHONE_ROOT"; fuga=1; }
  for f in "$PHONE_ROOT"/snapshots/phone-golden/*; do
    [ -f "$f" ] || continue
    LC_ALL=C grep -q -a -F "$id" "$f" && { mal "android_id $id en el dorado ($f)"; fuga=1; }
  done
done
[ "$fuga" = 0 ] && ok "ningún android_id en kling ps, en los logs del daemon/VMs ni en los ficheros del dorado ($(du -sh "$PHONE_ROOT/snapshots/phone-golden" 2>/dev/null | cut -f1))"

# ── 7. rm ────────────────────────────────────────────────────────────────────
"$PHONE" rm -a >/dev/null
left="$("$PHONE" ls | awk 'NR > 1' | wc -l | tr -d ' ')"
[ "$left" = 0 ] && ok "rm -a: no quedan teléfonos" || mal "rm -a: quedan $left"
viejas=0
for s in $(sort -u "$OUT/serials.txt" 2>/dev/null); do
  adb devices | awk '{ print $1 }' | grep -qx "$s" && { mal "adb devices aún lista $s"; viejas=1; }
done
[ "$viejas" = 0 ] && ok "adb devices sin entradas de los teléfonos borrados"

echo
if [ "$fallos" -eq 0 ]; then echo "PASS (salida en $OUT)"; exit 0; fi
echo "FAIL: $fallos fallo(s) (salida en $OUT)"; exit 1
