#!/bin/bash
# stress-restore.sh — estrés de restaurar/pausar/congelar/bifurcar un teléfono
# Android (Redroid 13 sobre kindling) buscando procesos nuevos que mueran con
# SIGILL (ver docs/sigill.md).
#
#   KLING_HOST=unix://$HOME/.kindling-android-X/kling.sock \
#     ./stress-restore.sh -from android13-gold -cycles 100 -modes run,pause,freeze,fork
#
# Cada ciclo hace UNA operación real del núcleo y después comprueba que Android
# sigue sano: que arrancan procesos nuevos (`toybox true` ×5 por android-sh),
# que system_server no se ha reiniciado y que no hay ningún `signal 4 (SIGILL)`
# nuevo en `logcat -b crash` ni en dmesg. Cada -heavy ciclos añade carga de
# verdad: `am start` de Ajustes, `uiautomator dump` y vuelta a HOME.
#
# Modos (se alternan en el orden dado):
#   run     `kling run -from` de un clon nuevo, comprobar, borrarlo
#   pause   `kling pause` + `kling thaw` del clon vivo (RAM, sin volcar)
#   freeze  `kling freeze` + `kling thaw` del clon vivo (a disco)
#   fork    `kling sandbox create -from` + `sandbox fork -n 1`, comprobar los dos
#   save    `kling save` del clon vivo como dorado encadenado, `run -from` de él
#   reads   en el clon vivo, -read-passes pasadas de: vaciar la caché de páginas
#           del invitado y leer con md5sum en paralelo todo /android/system y
#           /usr, contra una referencia de dos pasadas iguales. Es la prueba que
#           caza la causa real (docs/sigill.md: bloques de 4 KiB que llegan a
#           ceros por virtio-blk); los demás modos solo ven sus consecuencias.
# Con la capa detrás de dm-verity (image/verity.sh) cada ciclo apunta en la
# columna note cuántas líneas nuevas de "device-mapper: verity" hay en dmesg
# (todas en OUT/verity.txt), y reads avisa de los ficheros que dan EIO
# (READ_EIO). -keep-going hace que una lectura mala en reads se apunte en
# ciclos.csv y lecturas-malas.txt y se siga, para contar en N pasadas.
# El clon vivo (pause/freeze/save/reads) se CONGELA mientras corre un
# run/fork/save para no tener nunca más de una VM en RAM salvo en el fork.
#
# Al primer fallo se para, deja la máquina viva (salvo -rm-on-fail) y guarda en
# OUT/fallo-N/: tombstones, logcat (crash y main), dmesg, /proc/cpuinfo y los
# binarios del backtrace. Todo lo demás va a OUT/ciclos.csv.
#
# Memoria del Mac: antes de cada VM nueva espera a kern.memorystatus_level ≥
# -memlevel (35 por defecto). -hostload N lanza N procesos `yes` con nice 10
# mientras dura (contención y migración entre núcleos P/E). -hostspike GIB
# reserva de golpe GIB GiB aleatorios en el Mac durante 15 s al principio de
# cada ciclo (se corta si memorystatus_level baja de 18 o al disco le quedan
# menos de 3 GiB): es lo que dispara el
# fallo de lecturas; úsalo con cuidado si hay más VMs en el Mac.
set -uo pipefail

KLING="${KLING:-kling}"
FROM=""
CYCLES=100
MODES="run,pause,freeze,fork"
HEAVY=5
MEMLEVEL=35
HOSTLOAD=0
OUT=""
P="st$$"
RM_ON_FAIL=0
READY_TIMEOUT=90
HOSTSPIKE=0
READ_PASSES=3
KEEP_GOING=0

usage() { sed -n '2,44p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }
while [ $# -gt 0 ]; do
  case "$1" in
    -from) FROM="$2"; shift 2 ;;
    -cycles) CYCLES="$2"; shift 2 ;;
    -modes) MODES="$2"; shift 2 ;;
    -heavy) HEAVY="$2"; shift 2 ;;
    -memlevel) MEMLEVEL="$2"; shift 2 ;;
    -hostload) HOSTLOAD="$2"; shift 2 ;;
    -out) OUT="$2"; shift 2 ;;
    -prefix) P="$2"; shift 2 ;;
    -rm-on-fail) RM_ON_FAIL=1; shift ;;
    -hostspike) HOSTSPIKE="$2"; shift 2 ;;
    -read-passes) READ_PASSES="$2"; shift 2 ;;
    -keep-going) KEEP_GOING=1; shift ;;
    -h|--help) usage 0 ;;
    *) echo "unknown flag: $1" >&2; usage 2 ;;
  esac
done
[ -n "$FROM" ] || { echo "stress-restore: -from GOLDEN is required" >&2; exit 2; }
OUT="${OUT:-$(dirname "$0")/results/stress-$(date +%Y%m%d-%H%M%S)}"
mkdir -p "$OUT"

log() { echo "[$(date +%H:%M:%S)] $*" | tee -a "$OUT/stress.log" >&2; }
k() { "$KLING" "$@"; }
ax() { local m="$1" t="$2"; shift 2; "$KLING" exec -timeout "${t}s" "$m" -- android-sh "$@"; }
now() { perl -MTime::HiRes=time -e 'printf "%.3f", time'; }
secs() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.2f", b - a }'; }

espera_memoria() {
  local l
  while :; do
    l="$(sysctl -n kern.memorystatus_level 2>/dev/null || echo 100)"
    [ "$l" -ge "$MEMLEVEL" ] && return 0
    sleep 5
  done
}

# lista: procesos nuevos de Android arrancan (reintenta hasta READY_TIMEOUT s:
# tras restaurar, el agente tarda unos cientos de ms en contestar).
lista() {
  local m="$1" t0 t
  t0="$(now)"
  while :; do
    if ax "$m" 15 'toybox true' >/dev/null 2>&1; then return 0; fi
    t="$(now)"
    awk -v a="$(secs "$t0" "$t")" -v b="$READY_TIMEOUT" 'BEGIN { exit !(a > b) }' && return 1
    sleep 0.3
  done
}

# Estado por máquina: system_server y cuántos SIGILL había (para ver lo nuevo).
# Ficheros en $OUT/.estado-<máquina> (bash 3.2: nada de arrays asociativos).
estado_de() { cat "$OUT/.estado-$1" 2>/dev/null || echo "- 0 0"; }

# sano MÁQUINA CICLO: 0 si todo bien; 1 y deja el motivo en $MOTIVO si no.
sano() {
  local m="$1" c="$2" out ss ill illk prev_ss prev_ill prev_illk
  MOTIVO=""
  out="$(ax "$m" 30 'for i in 1 2 3 4 5; do toybox true || { echo NEWPROC_FAIL; exit 0; }; done
    echo "ss=$(pidof system_server)"
    echo "boot=$(getprop sys.boot_completed)"
    echo "ill=$(logcat -d -b crash 2>/dev/null | grep -c "signal 4 (SIGILL)")"' 2>&1)" || {
    MOTIVO="android-sh failed: $(echo "$out" | tail -n 1)"; return 1; }
  case "$out" in *NEWPROC_FAIL*) MOTIVO="toybox true failed"; return 1 ;; esac
  ss="$(echo "$out" | sed -n 's/^ss=//p')"
  ill="$(echo "$out" | sed -n 's/^ill=//p')"
  illk="$("$KLING" exec -timeout 10s "$m" -- sh -c 'dmesg | grep -ciE "sigill|undefined instruction|signal 4"' 2>/dev/null | tr -d '\r\n ')"
  read -r prev_ss prev_ill prev_illk <<<"$(estado_de "$m")"
  echo "$ss ${ill:-0} ${illk:-0}" >"$OUT/.estado-$m"
  [ "$(echo "$out" | sed -n 's/^boot=//p')" = 1 ] || { MOTIVO="sys.boot_completed != 1"; return 1; }
  [ -n "$ss" ] || { MOTIVO="no system_server"; return 1; }
  [ "${ill:-0}" -gt "${prev_ill:-0}" ] && { MOTIVO="new SIGILL in logcat -b crash ($ill)"; return 1; }
  [ "${illk:-0}" -gt "${prev_illk:-0}" ] && { MOTIVO="SIGILL/undefined instruction in dmesg ($illk)"; return 1; }
  if [ "$prev_ss" != "-" ] && [ "$prev_ss" != "$ss" ]; then
    MOTIVO="system_server restarted ($prev_ss -> $ss)"; return 1
  fi
  if [ "$HEAVY" -gt 0 ] && [ $((c % HEAVY)) -eq 0 ]; then
    ax "$m" 60 'am start -W -n com.android.settings/.Settings >/dev/null 2>&1 || exit 3
      uiautomator dump /data/local/tmp/ui.xml >/dev/null 2>&1 || exit 4
      grep -q "<hierarchy" /data/local/tmp/ui.xml || exit 5
      am force-stop com.android.settings; input keyevent KEYCODE_HOME' >/dev/null 2>&1 || {
      MOTIVO="heavy load failed (am start/uiautomator dump, rc=$?)"; return 1; }
    # la carga arranca procesos: vuelve a mirar si alguno murió con SIGILL
    ill="$(ax "$m" 20 'logcat -d -b crash 2>/dev/null | grep -c "signal 4 (SIGILL)"' 2>/dev/null | tr -d '\r\n ')"
    echo "$ss ${ill:-0} ${illk:-0}" >"$OUT/.estado-$m"
    [ "${ill:-0}" -gt "${prev_ill:-0}" ] && { MOTIVO="new SIGILL after heavy load ($ill)"; return 1; }
  fi
  return 0
}

# LECTURAS corre en la VM (Debian, no Android): `sh -c "$LECTURAS" x PASADAS`.
# La primera vez guarda la referencia (dos pasadas que deben coincidir); luego
# cada pasada vacía la caché y relee en paralelo. En un desajuste relee el
# fichero con O_DIRECT (sin caché) y dice qué páginas de 4 KiB difieren y si
# la versión mala es todo ceros.
LECTURAS='
R=/run/stress-lecturas; mkdir -p $R
pasada() { sync; echo 3 > /proc/sys/vm/drop_caches
  tr "\n" "\0" < $R/files | xargs -0 -P 16 -n 16 md5sum 2>$R/err | sort -k2 > $1
  [ -s $R/err ] && sed "s/^/READ_EIO pass=$i /" $R/err | head -5; return 0; }
if [ ! -f $R/ref ]; then
  find /android/system /usr -xdev -type f -size +16k 2>/dev/null | sort > $R/files
  pasada $R/ref; pasada $R/ref2
  cmp -s $R/ref $R/ref2 || { echo "READ_BAD reference passes differ"; diff $R/ref $R/ref2 | head -4; }
fi
i=0; malo=0
while [ $i -lt $1 ]; do
  i=$((i+1)); pasada $R/cur
  cmp -s $R/ref $R/cur && continue
  malo=1
  diff $R/ref $R/cur | sed -n "s/^> [0-9a-f]*  //p" | while read -r f; do
    dd if="$f" iflag=direct bs=1M status=none > $R/good 2>/dev/null
    cat "$f" > $R/bad
    echo "READ_BAD pass=$i $f pages4k=[$(cmp -l $R/good $R/bad | awk "{print int((\$1-1)/4096)}" | uniq | tr "\n" " " | cut -c1-60)] bytes=$(cmp -l $R/good $R/bad | wc -l) nonzero_in_bad=$(cmp -l $R/good $R/bad | awk "\$3 != 0" | wc -l)"
  done
done
echo "reads: $1 passes of $(wc -l < $R/files) files"
[ -e /dev/mapper/android-layer ] && echo "verity: $(dmsetup status android-layer 2>/dev/null)"
exit $malo'

lecturas() { # lecturas MÁQUINA: 0 si las lecturas coinciden con la referencia
  local m="$1" out
  out="$("$KLING" exec -timeout 900s "$m" -- sh -c "$LECTURAS" lecturas "$READ_PASSES" 2>&1)" && return 0
  MOTIVO="$(echo "$out" | grep -m 3 -E 'READ_BAD|READ_EIO' | tr '\n' ' ')"
  [ -n "$MOTIVO" ] || MOTIVO="read check failed: $(echo "$out" | tail -n 1)"
  echo "$out" >>"$OUT/lecturas-malas.txt"
  return 1
}

# verity_nuevos MÁQUINA: cuántas líneas de dm-verity nuevas hay en su dmesg
# (bloques que no cuadraron: releídos bien, corregidos por FEC o EIO). Se
# acumulan en $OUT/verity.txt, sin repetir (llevan la marca de tiempo).
verity_nuevos() {
  local antes
  touch "$OUT/verity.txt"; antes="$(wc -l <"$OUT/verity.txt")"
  { cat "$OUT/verity.txt"
    "$KLING" exec -timeout 10s "$1" -- sh -c 'dmesg | grep "device-mapper: verity" | grep -v "using implementation"' 2>/dev/null | sed "s/^/$1 /"
  } | sort -u >"$OUT/.verity" && mv "$OUT/.verity" "$OUT/verity.txt"
  echo $(( $(wc -l <"$OUT/verity.txt") - antes ))
}

# El pico también se corta si al disco del Mac le quedan menos de 3 GiB: con
# la swap llena, macOS la crece en ese disco, y un disco lleno tumba el Mac
# entero (medido: con la swap llena el disco llegó a bajar a 2 GiB libres).
pico() { # reserva de golpe HOSTSPIKE GiB aleatorios 15 s, en segundo plano
  [ "$HOSTSPIKE" -gt 0 ] || return 0
  perl -e '
    my ($g, $home) = @ARGV; open(my $r, "<", "/dev/urandom") or exit; my @t;
    while (@t < $g * 8) { my $l = `sysctl -n kern.memorystatus_level`; last if $l < 18;
      my ($kb) = (`df -k "$home"` =~ /\n\S+\s+\d+\s+\d+\s+(\d+)/); last if defined $kb && $kb < 3 * 1048576;
      my $b; read($r, $b, 128 << 20); push @t, $b; }
    sleep 15;' "$HOSTSPIKE" "$HOME" &
  HOSTPIDS="$HOSTPIDS $!"
}

# recoger MÁQUINA N: todo lo necesario para el diagnóstico, antes de que se pierda.
recoger() {
  local m="$1" d="$OUT/fallo-$2"
  mkdir -p "$d"
  log "collecting evidence from $m into $d"
  "$KLING" exec -timeout 20s "$m" -- dmesg >"$d/dmesg.txt" 2>&1
  "$KLING" exec -timeout 20s "$m" -- cat /proc/cpuinfo >"$d/cpuinfo-vm.txt" 2>&1
  ax "$m" 30 'logcat -d -b crash' >"$d/logcat-crash.txt" 2>&1
  ax "$m" 30 'logcat -d -b main,system,kernel -t 3000' >"$d/logcat.txt" 2>&1
  ax "$m" 30 'cat /proc/cpuinfo' >"$d/cpuinfo-android.txt" 2>&1
  ax "$m" 60 'cd /data/tombstones && tar cf - .' >"$d/tombstones.tar" 2>/dev/null
  # binarios que salen en los backtraces de SIGILL (para desensamblar)
  local b
  for b in $(grep -A40 "signal 4 (SIGILL)" "$d/logcat-crash.txt" | sed -n 's#.* pc [0-9a-f]*  \(/[^ ]*\).*#\1#p' | sort -u | head -8); do
    ax "$m" 30 "cat $b" >"$d/$(echo "$b" | tr / _)" 2>/dev/null
  done
  k ps >"$d/ps.txt" 2>&1
}

HOSTPIDS=""
limpiar() {
  local p
  for p in $HOSTPIDS; do kill "$p" 2>/dev/null; done
}
trap limpiar EXIT
if [ "$HOSTLOAD" -gt 0 ]; then
  for _ in $(seq 1 "$HOSTLOAD"); do nice -n 10 yes >/dev/null & HOSTPIDS="$HOSTPIDS $!"; done
  log "host load: $HOSTLOAD yes processes"
fi

IFS=, read -r -a MODOS <<<"$MODES"
LIVE=""
LIVE_FROZEN=0
necesita_vivo=0
for x in "${MODOS[@]}"; do case "$x" in pause|freeze|save|reads) necesita_vivo=1 ;; esac; done

falla() { # falla MÁQUINA CICLO MODO
  log "FAIL cycle $2 ($3) on $1: $MOTIVO"
  echo "$2,$3,$1,FAIL,,$(sysctl -n kern.memorystatus_level),\"$MOTIVO\"" >>"$OUT/ciclos.csv"
  recoger "$1" "$2"
  if [ "$RM_ON_FAIL" = 1 ]; then k rm -f "$1" >/dev/null 2>&1; fi
  log "stopped after $2 cycles; machine $1 kept for diagnosis"
  exit 1
}

vivo_despierto() {
  if [ "$LIVE_FROZEN" = 1 ]; then
    k thaw "$LIVE" >/dev/null 2>&1 || { MOTIVO="thaw of $LIVE failed"; falla "$LIVE" "$1" thaw-live; }
    LIVE_FROZEN=0
    lista "$LIVE" || { MOTIVO="not ready after thaw"; falla "$LIVE" "$1" thaw-live; }
  fi
}
vivo_dormido() {
  if [ -n "$LIVE" ] && [ "$LIVE_FROZEN" = 0 ]; then
    k freeze "$LIVE" >/dev/null 2>&1 && LIVE_FROZEN=1
  fi
}

echo "cycle,mode,machine,result,op_s,memlevel,note" >"$OUT/ciclos.csv"
log "stress: from=$FROM cycles=$CYCLES modes=$MODES heavy=$HEAVY out=$OUT"

if [ "$necesita_vivo" = 1 ]; then
  LIVE="$P-live"
  espera_memoria
  k run -from "$FROM" -name "$LIVE" >/dev/null || { echo "run -from $FROM failed" >&2; exit 1; }
  lista "$LIVE" || { MOTIVO="live clone not ready"; falla "$LIVE" 0 start; }
  sano "$LIVE" 1 || falla "$LIVE" 0 start
fi

for c in $(seq 1 "$CYCLES"); do
  modo="${MODOS[$(((c - 1) % ${#MODOS[@]}))]}"
  pico
  m="" op=""
  case "$modo" in
    run)
      vivo_dormido
      m="$P-r$c"; espera_memoria
      t0="$(now)"
      k run -from "$FROM" -name "$m" >/dev/null 2>"$OUT/.err" || { MOTIVO="run -from failed: $(tail -n1 "$OUT/.err")"; falla "$m" "$c" "$modo"; }
      lista "$m" || { MOTIVO="not ready ${READY_TIMEOUT}s after run -from"; falla "$m" "$c" "$modo"; }
      op="$(secs "$t0" "$(now)")"
      sano "$m" "$c" || falla "$m" "$c" "$modo"
      k rm -f "$m" >/dev/null 2>&1; rm -f "$OUT/.estado-$m"
      ;;
    pause)
      vivo_despierto "$c"; m="$LIVE"
      k pause "$m" >/dev/null 2>&1 || { MOTIVO="pause failed"; falla "$m" "$c" "$modo"; }
      sleep "$(awk -v r="$RANDOM" 'BEGIN { printf "%.1f", (r % 30) / 10 }')"
      t0="$(now)"
      k thaw "$m" >/dev/null 2>&1 || { MOTIVO="thaw failed"; falla "$m" "$c" "$modo"; }
      lista "$m" || { MOTIVO="not ready after thaw"; falla "$m" "$c" "$modo"; }
      op="$(secs "$t0" "$(now)")"
      sano "$m" "$c" || falla "$m" "$c" "$modo"
      ;;
    freeze)
      vivo_despierto "$c"; m="$LIVE"
      k freeze "$m" >/dev/null 2>&1 || { MOTIVO="freeze failed"; falla "$m" "$c" "$modo"; }
      espera_memoria
      t0="$(now)"
      k thaw "$m" >/dev/null 2>&1 || { MOTIVO="thaw from disk failed"; falla "$m" "$c" "$modo"; }
      lista "$m" || { MOTIVO="not ready after thaw from disk"; falla "$m" "$c" "$modo"; }
      op="$(secs "$t0" "$(now)")"
      sano "$m" "$c" || falla "$m" "$c" "$modo"
      ;;
    fork)
      vivo_dormido
      m="$P-sb$c"; espera_memoria
      t0="$(now)"
      k sandbox create -from "$FROM" -name "$m" -ttl 30m -q >/dev/null 2>"$OUT/.err" || { MOTIVO="sandbox create failed: $(tail -n1 "$OUT/.err")"; falla "$m" "$c" "$modo"; }
      lista "$m" || { MOTIVO="sandbox not ready"; falla "$m" "$c" "$modo"; }
      sano "$m" "$c" || falla "$m" "$c" "$modo"
      espera_memoria
      f="$(k sandbox fork "$m" -n 1 -q 2>"$OUT/.err" | head -n 1)"
      [ -n "$f" ] || { MOTIVO="fork failed: $(tail -n1 "$OUT/.err")"; falla "$m" "$c" "$modo"; }
      lista "$f" || { MOTIVO="fork copy not ready"; falla "$f" "$c" "$modo"; }
      op="$(secs "$t0" "$(now)")"
      cp "$OUT/.estado-$m" "$OUT/.estado-$f" 2>/dev/null
      sano "$f" "$c" || falla "$f" "$c" "$modo"
      sano "$m" "$c" || falla "$m" "$c" "$modo"
      k sandbox rm -f "$m" "$f" >/dev/null 2>&1 || k rm -f "$m" "$f" >/dev/null 2>&1
      rm -f "$OUT/.estado-$m" "$OUT/.estado-$f"
      ;;
    save)
      vivo_despierto "$c"
      g="$P-chain"
      k save -replace "$LIVE" "$g" >/dev/null 2>"$OUT/.err" || { MOTIVO="save failed: $(tail -n1 "$OUT/.err")"; falla "$LIVE" "$c" "$modo"; }
      # el clon vivo sigue en marcha tras save; se sustituye por uno
      # restaurado del dorado encadenado (restaurar lo que ya fue restaurado)
      k rm -f "$LIVE" >/dev/null 2>&1
      espera_memoria
      t0="$(now)"
      k run -from "$g" -name "$LIVE" >/dev/null 2>"$OUT/.err" || { MOTIVO="run -from $g failed: $(tail -n1 "$OUT/.err")"; falla "$LIVE" "$c" "$modo"; }
      lista "$LIVE" || { MOTIVO="chained clone not ready"; falla "$LIVE" "$c" "$modo"; }
      op="$(secs "$t0" "$(now)")"; m="$LIVE"; LIVE_FROZEN=0
      sano "$m" "$c" || falla "$m" "$c" "$modo"
      ;;
    reads)
      vivo_despierto "$c"; m="$LIVE"
      t0="$(now)"
      if ! lecturas "$m"; then
        # -keep-going: una lectura mala se apunta y se sigue (para contar
        # cuántas salen en N pasadas, docs/verity.md); lo demás sigue parando.
        [ "$KEEP_GOING" = 1 ] || falla "$m" "$c" "$modo"
        log "READ_BAD cycle $c on $m (keep going): $MOTIVO"
        echo "$c,$modo,$m,READ_BAD,,$(sysctl -n kern.memorystatus_level),\"$MOTIVO\"" >>"$OUT/ciclos.csv"
      fi
      op="$(secs "$t0" "$(now)")"
      sano "$m" "$c" || falla "$m" "$c" "$modo"
      ;;
    *) echo "unknown mode $modo" >&2; exit 2 ;;
  esac
  echo "$c,$modo,$m,ok,$op,$(sysctl -n kern.memorystatus_level),verity=$(verity_nuevos "$m")" >>"$OUT/ciclos.csv"
  [ $((c % 10)) -eq 0 ] && log "cycle $c/$CYCLES ok ($modo, ${op}s)"
done

log "done: $CYCLES cycles, no failure"
if [ -n "$LIVE" ]; then k rm -f "$LIVE" >/dev/null 2>&1; fi
exit 0
