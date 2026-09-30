#!/bin/bash
# phone.sh — teléfonos Android (Redroid 13) en microVMs de kindling, en el Mac.
#
#   ./phone.sh up [-n N]          N teléfonos nuevos desde el dorado (lo crea si falta)
#   ./phone.sh ls                 nombre, estado y puerto adb en 127.0.0.1
#   ./phone.sh view <tel>         scrcpy si está; si no, una captura (adb) en Vista Previa
#   ./phone.sh adb <tel> [args]   adb contra ese teléfono (sin args: imprime el serial)
#   ./phone.sh shell <tel> [cmd]  una shell de Android (adb shell; sin adb, kling exec)
#   ./phone.sh api <tel> <MÉTODO> <ruta> [fichero]   la API de kling-phoned (docs/phoned.md)
#                                 por POST /machines/{ref}/guest, sin allow_exec, con el
#                                 token del teléfono ($PHONE_ROOT/tokens/<tel>)
#   ./phone.sh pause <tel>...     pausado en RAM: no gasta CPU; resume en ~25 ms
#   ./phone.sh resume <tel>...    lo reanuda (los puertos se leen siempre de kling ps)
#   ./phone.sh pool <N>           deja N teléfonos pausados listos para resume
#   ./phone.sh rm <tel>...|-a     borra teléfonos (-a: todos)
#   ./phone.sh golden rebuild     rehace el dorado (arranque en frío + kling save)
#   ./phone.sh daemon [stop]      estado del daemon privado, o pararlo
#
# <tel> es el nombre (phone-3) o solo el número (3).
#
# Cómo funciona (docs/telefono.md):
#   - Un daemon de kindling PRIVADO (raíz $PHONE_ROOT, socket propio), porque el
#     kernel Android es uno por daemon; el de siempre y sus dorados no se tocan.
#     Si no está corriendo, este script lo arranca.
#   - Un dorado "$GOLDEN": Android arrancado en frío hasta sys.boot_completed=1,
#     pantalla encendida, sin bloqueo ni animaciones, y `kling save`. Cada
#     teléfono es `kling run -from` del dorado (~0,6 s de restaurar).
#   - adb llega al Mac por los reenvíos de kindling (etiqueta
#     kling.ports=5555,8091; el 8091 es la API de kling-phoned), SOLO en
#     127.0.0.1. El VNC de Redroid ya no va por defecto (pantalla en negro, #96). Con una imagen de kling-phoned adb pide
#     clave: la de cada clon viaja con su identidad (PHONE_ADB_PUBKEY, por
#     defecto ~/.android/adbkey.pub; PHONE_ADB_KEYS_DIR/<tel>.pub si existe). El puerto del Mac cambia en cada restore/thaw: siempre se lee de
#     `kling ps -json` (campo forwards), nunca se guarda.
#   - "Listo" lo decide la imagen (/etc/kindling/ready: sys.boot_completed=1) y
#     lo espera el núcleo: `kling run -wait-ready` al arrancar el dorado y cada
#     clon, y `kling save` no congela hasta que la sonda dice que sí.
#   - Identidad propia por clon: tras restaurar, un android_id, un nombre, una
#     serie y las claves de adb nuevos viajan como secreto de sesión por MMDS (`kling machine secret -hooks`,
#     stdin) y el gancho de la imagen (/etc/kindling/post-restore.d/10-identity)
#     los aplica dentro. No quedan en la línea de órdenes, ni en `kling ps`, ni
#     en el dorado. Al vaciar después el almacén, el daemon levanta la marca de
#     secretos: el teléfono se congela (freeze/thaw) como cualquier otro.
#   - Con un kling sin la capacidad `ready` (v0.16) hace lo mismo a mano: sondea
#     getprop, aplica la identidad con `android-sh --identity`, pasa -cpu-pct y
#     el teléfono queda marcado con secretos (ni freeze ni save; pause sí).
#
# Variables: PHONE_ROOT (~/.kindling-android-telefono), PHONE_IMAGE (android13),
# PHONE_GOLDEN (phone-golden), PHONE_PREFIX (phone), PHONE_CPUS (2),
# PHONE_MEM (1536 MiB), PHONE_CPU_PCT (vacío: el de la receta de la imagen,
# 100 por vCPU; con kling v0.16, CPUS×100), PHONE_EGRESS (none | internet),
# PHONE_MIN_MEMLEVEL (35: no arranca otro teléfono mientras
# kern.memorystatus_level esté por debajo).
# PHONE_EXEC (1): 0 = dorado SIN allow_exec; lo que phone.sh hacía por kling
# exec va por la API de kling-phoned (necesita una imagen PHONED=1).
# El egress y los recursos se deciden al hacer el dorado: `golden rebuild`.
#
# bash 3.2 (el de macOS): nada de mapfile, arrays asociativos ni ${x,,}.
set -euo pipefail

PHONE_ROOT="${PHONE_ROOT:-$HOME/.kindling-android-telefono}"
# ── linux ── En Linux (Firecracker, docs/proxmox.md) no hay reenvíos a
# 127.0.0.1: adb se alcanza en la IP de la VM (tap), que solo se ve desde
# el anfitrión. PHONE_SOCK=/run/kling.sock usa un daemon ya en marcha (el del
# sistema, con el kernel Android) en vez de arrancar uno privado.
OS="$(uname -s)"
SOCK="${PHONE_SOCK:-$PHONE_ROOT/kling.sock}"
# ── fin linux ──
export KLING_HOST="unix://$SOCK"
KLING="${KLING:-kling}"
IMAGE="${PHONE_IMAGE:-android13}"
GOLDEN="${PHONE_GOLDEN:-phone-golden}"
PREFIX="${PHONE_PREFIX:-phone}"
CPUS="${PHONE_CPUS:-2}"
MEM="${PHONE_MEM:-1536}"
# Con el 50 % por defecto, vz pausa la VM entera para cumplir el techo y Android
# va a un cuarto de velocidad (README, "Resultado en el Mac"). La receta de la
# imagen trae cpu_pct_per_vcpu=100; kling v0.16 no la lee y se le pasa CPUS×100.
CPU_PCT="${PHONE_CPU_PCT:-}"
EGRESS="${PHONE_EGRESS:-none}"      # la del dorado al crearlo; los clones heredan la del dorado
MIN_MEMLEVEL="${PHONE_MIN_MEMLEVEL:-35}"
# `-ttl 0` significa "el de la configuración" (10 min): un teléfono se congelaría
# solo. Un TTL largo equivale a no tenerlo.
TTL="${PHONE_TTL:-8760h}"
BOOT_TIMEOUT="${PHONE_BOOT_TIMEOUT:-180}"
LABEL_PHONE="kindling.phone=1"
PHONE_EXEC="${PHONE_EXEC:-1}"
PHONED_PORT=8091

log() { printf '==> %s\n' "$*" >&2; }
die() { printf 'phone.sh: %s\n' "$*" >&2; exit 1; }
k() { "$KLING" "$@"; }
# kq: kling sin su salida (las pistas "next: ..." van a stderr); si falla, la enseña.
kq() {
  local out
  out="$("$KLING" "$@" 2>&1)" && return 0
  printf '%s\n' "$out" >&2
  return 1
}
now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time * 1000'; }
secs() { perl -e 'printf "%.2f\n", ($ARGV[1] - $ARGV[0]) / 1000' "$1" "$2"; }

uso() { sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'; exit "${1:-0}"; }

# nucleo_listo: el CLI y el daemon tienen la capacidad `ready` (sonda, ganchos,
# -wait-ready, cpu_pct de la receta y marca de secretos que se levanta). Se
# pregunta una vez.
NUCLEO_LISTO=""
nucleo_listo() {
  if [ -z "$NUCLEO_LISTO" ]; then
    NUCLEO_LISTO=no
    if "$KLING" run -h 2>&1 | grep -q -- '-wait-ready' &&
       k info 2>/dev/null | awk '/^capabilities:/ { gsub(",", " "); for (i = 2; i <= NF; i++) if ($i == "ready") f = 1 } END { exit !f }'; then
      NUCLEO_LISTO=si
    fi
  fi
  [ "$NUCLEO_LISTO" = si ]
}

# ── daemon privado ───────────────────────────────────────────────────────────
# (`kling status` además pregunta al gateway: más lento y no hace falta.)
daemon_up() { [ -S "$SOCK" ] && k ps -json >/dev/null 2>&1; }

ensure_daemon() {
  daemon_up && return 0
  [ -z "${PHONE_SOCK:-}" ] || die "no daemon answering on PHONE_SOCK=$PHONE_SOCK"
  [ -f "$PHONE_ROOT/images/vmlinux" ] || die "no Android kernel at $PHONE_ROOT/images/vmlinux.
  Install the kernel and image first (prototypes/android/README.md: fase0.sh -bundle ... -kernel ...
  with -root $PHONE_ROOT, or copy vmlinux, android-base.ext4, $IMAGE.layer.ext4 and $IMAGE.recipe.json there)"
  [ -f "$PHONE_ROOT/images/$IMAGE.layer.ext4" ] || die "no image $IMAGE in $PHONE_ROOT/images (see above)"
  log "arrancando el daemon privado (raíz $PHONE_ROOT)"
  # trap '' INT: un Ctrl-C en este script no debe matar al daemon.
  ( trap '' INT; exec nohup "$KLING" daemon -root "$PHONE_ROOT" -socket "$SOCK" ) \
    >>"$PHONE_ROOT/daemon.log" 2>&1 &
  echo $! >"$PHONE_ROOT/daemon.pid"
  local i
  for i in $(seq 1 50); do daemon_up && return 0; sleep 0.2; done
  die "the private daemon did not come up (see $PHONE_ROOT/daemon.log)"
}

daemon_stop() {
  daemon_up || { echo "daemon: not running"; return 0; }
  local pid n
  pid="$(cat "$PHONE_ROOT/daemon.pid" 2>/dev/null || true)"
  n="$(phones_tsv | awk 'END { print NR }')"
  [ "$n" -eq 0 ] || die "there are $n phone(s); remove them first (phone.sh rm -a)"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null || die "daemon not started by phone.sh (no $PHONE_ROOT/daemon.pid); stop it yourself"
  kill "$pid"
  echo "daemon: stopped (PID $pid)"
}

# ── lectura de `kling ps -json` ──────────────────────────────────────────────
# phones_tsv: nombre, estado, host:puerto para 5555 ("-" si no
# hay), de las máquinas con la etiqueta de teléfono, ordenadas por nombre. En
# el Mac es 127.0.0.1:<reenvío>; en Linux, <ip de la VM>:<puerto> si corre.
phones_tsv() {
  k ps -json 2>/dev/null | perl -MJSON::PP -e '
    local $/; my $t = <STDIN>; $t = "[]" if !defined $t || $t !~ /\S/;
    my @m = @{ decode_json($t) || [] };
    for my $m (sort { $a->{name} cmp $b->{name} } @m) {
      next unless ($m->{labels} || {})->{"kindling.phone"};
      my $f = $m->{forwards} || {};
      my $a = $f->{5555} // (($m->{ip} && $m->{state} eq "running") ? "$m->{ip}:5555" : "-");
      print join("\t", $m->{name}, $m->{state}, $a), "\n";
    }'
}

# nombre: acepta "phone-3" o "3".
nombre() {
  case "$1" in
    ''|*[!0-9]*) echo "$1" ;;
    *) echo "$PREFIX-$1" ;;
  esac
}

# puerto TEL GUESTPORT: host:puerto por el que se llega a ese puerto del
# teléfono. Vacío si no está (pausado, parado o sin reenvío).
puerto() {
  phones_tsv | awk -F'\t' -v n="$1" '$1 == n && $3 != "-" { print $3 }'
}

estado() { phones_tsv | awk -F'\t' -v n="$1" '$1 == n { print $2 }'; }

existe() { [ -n "$(estado "$1")" ]; }

# ── adb ──────────────────────────────────────────────────────────────────────
tiene_adb() { command -v adb >/dev/null 2>&1; }

# adb_usable TEL: este phone.sh puede entrar por adb en TEL: hay adb y la clave
# que abre TEL es la del usuario (~/.android/adbkey, la que usa el servidor de
# adb), no una propia del clon (PHONE_ADB_KEYS_DIR/<tel>.pub: esa es para
# quien tenga su privada). Con PHONE_EXEC=0 y sin clave, tampoco: la imagen de
# kling-phoned pide clave.
adb_usable() {
  tiene_adb || return 1
  if [ -n "${PHONE_ADB_KEYS_DIR:-}" ] && [ -f "$PHONE_ADB_KEYS_DIR/$1.pub" ]; then return 1; fi
  [ "$PHONE_EXEC" = 1 ] || [ -n "$(adb_pubkey "$1")" ]
}

# adb_listo SERIAL TIMEOUT_S: adb connect hasta que `adb shell true` conteste.
adb_listo() {
  local s="$1" t="$2" fin
  fin=$(( $(date +%s) + t ))
  while [ "$(date +%s)" -lt "$fin" ]; do
    adb connect "$s" >/dev/null 2>&1 || true
    if adb -s "$s" shell true >/dev/null 2>&1; then return 0; fi
    # Una entrada "offline" vieja no se recupera sola.
    adb disconnect "$s" >/dev/null 2>&1 || true
    sleep 0.2
  done
  return 1
}

# serial TEL: host:puerto de adb, conectado.
serial() {
  local p
  p="$(puerto "$1" 5555)"
  [ -n "$p" ] || die "$1 has no adb port (state: $(estado "$1"); paused? phone.sh resume $1)"
  tiene_adb || die "adb is not installed (brew install android-platform-tools)"
  adb_listo "$p" 20 || die "adb does not answer on $p"
  echo "$p"
}

# Olvida en el servidor de adb las entradas de un teléfono cuyo puerto va a
# dejar de valer (pause, rm): si no, `adb devices` las enseña "offline".
adb_olvida() {
  local p
  tiene_adb || return 0
  p="$(puerto "$1" 5555)"
  [ -n "$p" ] && adb disconnect "$p" >/dev/null 2>&1 || true
}

# ── dentro de Android ────────────────────────────────────────────────────────
ax() { local m="$1" t="$2"; shift 2; k exec -timeout "${t}s" "$m" -- android-sh "$@"; }

# api TEL MÉTODO RUTA [FICHERO]: una llamada a kling-phoned por el proxy del
# daemon (POST /machines/{ref}/guest), sin allow_exec. Imprime el cuerpo de la
# respuesta (decodificado si la ruta pide encoding=base64 y es un GET) y sale
# con 0 si el invitado contestó 2xx. El cuerpo de ida sale de FICHERO tal cual.
api() {
  local m="$1" meth="$2" path="$3" body="${4:-}"
  # El token del teléfono (docs/phoned.md, #110): sin él solo contesta
  # /v1/health. Va por la entrada estándar de perl, no en argv.
  # (|| true: sin fichero, con pipefail, la tubería entera fallaría.)
  { cat "$PHONE_ROOT/tokens/$m" 2>/dev/null || true; } | perl -MJSON::PP -e '
    my ($meth, $path, $port, $bf) = @ARGV; my $b = ""; my $tok = <STDIN> // ""; chomp $tok;
    if (defined $bf && $bf ne "") { local $/; open(my $f, "<", $bf) or die "$bf: $!\n"; binmode $f; $b = <$f>; }
    my %h = ("Content-Type" => "application/json"); $h{Authorization} = "Bearer $tok" if $tok ne "";
    print encode_json({port => $port + 0, path => $path, method => $meth, body => $b, wait_ms => 5000,
      max_body_bytes => 33554432, headers => \%h});' \
    "$meth" "$path" "$PHONED_PORT" "$body" |
  curl -sS --max-time 300 --unix-socket "$SOCK" -X POST -H 'Content-Type: application/json' \
    --data-binary @- "http://kling/machines/$m/guest" |
  perl -MJSON::PP -MMIME::Base64 -e '
    local $/; my $t = <STDIN>; my $d = eval { decode_json($t) } or do { print STDERR "phone.sh api: $t\n"; exit 1 };
    if (!defined $d->{status}) { print STDERR "phone.sh api: ", ($d->{message} // $t), "\n"; exit 1 }
    my $b = $d->{body} // "";
    if ($ARGV[0] eq "GET" && $ARGV[1] =~ /encoding=base64/ && $d->{status} == 200) { $b = decode_base64($b) }
    else { utf8::encode($b) }
    binmode STDOUT; print $b; exit($d->{status} >= 200 && $d->{status} < 300 ? 0 : 2)' "$meth" "$path"
}

# espera_listo TEL: con el núcleo nuevo, la sonda de la imagen (run
# -wait-ready ya esperó; esto lo confirma); con v0.16, getprop a mano.
espera_listo() {
  if nucleo_listo; then
    k machine ready "$1" -wait "${BOOT_TIMEOUT}s" >/dev/null 2>&1
    return
  fi
  espera_boot "$1"
}

espera_boot() {
  local m="$1" fin v
  fin=$(( $(date +%s) + BOOT_TIMEOUT ))
  while [ "$(date +%s)" -lt "$fin" ]; do
    v="$(ax "$m" 5 getprop sys.boot_completed 2>/dev/null | tr -d '\r\n' || true)"
    [ "$v" = 1 ] && return 0
    sleep 0.5
  done
  return 1
}

prepara_pantalla() {
  # Pantalla encendida para siempre, sin bloqueo y sin animaciones (lo mismo
  # que fase0.sh): lo que se ve es la pantalla de inicio, y los
  # clones restauran ya así.
  ax "$1" 30 'svc power stayon true; settings put system screen_off_timeout 2147483647;
    locksettings set-disabled true; input keyevent KEYCODE_WAKEUP; wm dismiss-keyguard;
    settings put global window_animation_scale 0; settings put global transition_animation_scale 0;
    settings put global animator_duration_scale 0; input keyevent KEYCODE_HOME' >/dev/null 2>&1 || true
}

# salud TEL: system_server vivo, el servicio settings contesta y el búfer de
# fallos de Android no tiene ni un SIGILL ni una caída de system_server.
salud() {
  local out
  if [ "$PHONE_EXEC" = 0 ]; then
    out="$(api "$1" GET /v1/health >/dev/null || echo "kling-phoned: not healthy ($(api "$1" GET /v1/health 2>&1 | head -c 300))"
      api "$1" GET '/v1/logs?buffer=crash&lines=400' 2>/dev/null | grep -E "SIGILL|IN SYSTEM PROCESS|bad ELF magic" | head -n 3)"
    [ -z "$out" ] && return 0
    printf '%s\n' "$out" | sed "s/^/  $1: /" >&2
    return 1
  fi
  out="$(ax "$1" 30 'pidof system_server >/dev/null || echo "no system_server";
    service check settings | grep -q "found" || echo "settings service not found";
    logcat -d -b crash | grep -E "SIGILL|IN SYSTEM PROCESS|bad ELF magic" | head -n 3' 2>&1 || echo "android-sh failed")"
  [ -z "$out" ] && return 0
  printf '%s\n' "$out" | sed "s/^/  $1: /" >&2
  return 1
}

# ── memoria del Mac ──────────────────────────────────────────────────────────
# Cada teléfono restaurado ocupa toda su RAM (~2,1 GiB de footprint con 1,5 GiB).
# nivel_memoria: 0-100. macOS: kern.memorystatus_level; Linux: MemAvailable
# sobre MemTotal (en un CT de LXC, /proc/meminfo es el del contenedor).
nivel_memoria() {
  if [ "$OS" = Linux ]; then
    awk '/^MemTotal:/ { t = $2 } /^MemAvailable:/ { a = $2 } END { if (t) print int(a * 100 / t); else print 100 }' /proc/meminfo
  else
    sysctl -n kern.memorystatus_level 2>/dev/null || echo 100
  fi
}
espera_memoria() {
  local nivel fin avisado=0
  fin=$(( $(date +%s) + 300 ))
  while :; do
    nivel="$(nivel_memoria)"
    [ "$nivel" -ge "$MIN_MEMLEVEL" ] && return 0
    [ "$(date +%s)" -lt "$fin" ] || die "macOS memory level is $nivel (< $MIN_MEMLEVEL) for 5 min; free memory or pause/rm phones"
    [ "$avisado" = 1 ] || log "memoria del Mac baja (kern.memorystatus_level=$nivel < $MIN_MEMLEVEL); esperando"
    avisado=1
    sleep 3
  done
}

# ── dorado ───────────────────────────────────────────────────────────────────
dorado_existe() { k template ls -q 2>/dev/null | awk -v g="$GOLDEN" '$0 == g { f = 1 } END { exit !f }'; }

egress_dorado() {
  k template inspect -json "$GOLDEN" 2>/dev/null |
    perl -MJSON::PP -e 'local $/; my $d = eval { decode_json(<STDIN>) } || {}; print $d->{egress} || "none"'
}

dorado_crea() {
  local cold="$GOLDEN-cold" t0 t1 i
  existe "$cold" && k rm -f "$cold" >/dev/null 2>&1 || true
  espera_memoria
  local pct="$CPU_PCT" listo=""
  nucleo_listo && listo="-wait-ready -ready-timeout ${BOOT_TIMEOUT}s"
  [ -n "$pct" ] || nucleo_listo || pct=$((CPUS * 100))
  log "dorado: arranque en frío de $IMAGE ($CPUS vCPU, $MEM MiB, CPU ${pct:-de la receta} %, egress $EGRESS)"
  t0="$(now_ms)"
  # shellcheck disable=SC2086 # $listo son dos flags o nada
  local ex="-allow-exec"
  [ "$PHONE_EXEC" = 0 ] && ex=""
  # shellcheck disable=SC2086 # $listo y $ex son flags o nada
  kq run -image "$IMAGE" -name "$cold" -cpus "$CPUS" -mem "$MEM" ${pct:+-cpu-pct "$pct"} $listo $ex \
    -egress "$EGRESS" -ttl "$TTL" -label "kling.ports=5555,$PHONED_PORT" -label "$LABEL_PHONE" >/dev/null
  if ! espera_listo "$cold"; then
    if [ "$PHONE_EXEC" = 0 ]; then api "$cold" GET '/v1/logs?buffer=phoned&lines=30' >&2 || true
    else k exec -timeout 10s "$cold" -- tail -n 30 /var/log/service.log >&2 || true; fi
    die "Android did not reach sys.boot_completed=1 in ${BOOT_TIMEOUT}s ($cold kept for inspection)"
  fi
  t1="$(now_ms)"
  log "dorado: boot_completed en $(secs "$t0" "$t1") s; preparando la pantalla"
  # Sin exec, la prepara kling-phoned (ANDROID_PREP) antes de dar el listo.
  [ "$PHONE_EXEC" = 0 ] || prepara_pantalla "$cold"
  # Que el lanzador y adbd estén de pie antes de congelar: un dorado a
  # medio arrancar daría clones a medio arrancar.
  for i in $(seq 1 40); do
    [ "$PHONE_EXEC" = 0 ] && break
    ax "$cold" 5 'getprop init.svc.adbd' 2>/dev/null | tr -d '\r' | awk '$0 == "running" { f = 1 } END { exit !f }' && break
    sleep 0.5
  done
  sleep 3
  # Nada de guardar una caché de páginas rota: el dorado la repartiría a todos
  # los clones (visto con el Mac bajo presión de memoria: páginas a cero y
  # SIGILL en cada proceso nuevo; docs/telefono.md). Se suelta la caché limpia
  # y se compara la que queda con el disco.
  if [ "$PHONE_EXEC" = 0 ]; then
    log "dorado: sin exec no se compara la caché de páginas con el disco (con verity lo vigila dm-verity: /v1/health)"
  else
  k exec -timeout 30s "$cold" -- sh -c 'sync; echo 3 >/proc/sys/vm/drop_caches' >/dev/null 2>&1 || true
  log "dorado: comprobando la caché de páginas contra el disco"
  t0="$(now_ms)"
  if ! k exec -timeout 300s "$cold" -- android-sh --verify-cache >"$PHONE_ROOT/verify-golden.txt" 2>&1; then
    tail -n 5 "$PHONE_ROOT/verify-golden.txt" >&2
    k rm -f "$cold" >/dev/null 2>&1 || true
    die "the guest page cache does not match the disk (host memory pressure? memory level $(nivel_memoria)); not saving a broken golden. Retry with more free memory"
  fi
  log "dorado: $(tail -n 1 "$PHONE_ROOT/verify-golden.txt") en $(secs "$t0" "$(now_ms)") s"
  fi
  # Y que nada haya muerto por el camino: un system_server que ya se cayó una
  # vez (o un SIGILL) en el dorado se repite en cada clon.
  if ! salud "$cold"; then
    k rm -f "$cold" >/dev/null 2>&1 || true
    die "Android crashed while building the golden (see above); not saving it. Retry with more free memory"
  fi
  log "dorado: kling save $cold → $GOLDEN"
  t0="$(now_ms)"
  if ! k save -replace "$cold" "$GOLDEN" >/dev/null 2>&1; then
    # El cliente corta a los 60 s, pero el daemon puede seguir guardando.
    for i in $(seq 1 90); do dorado_existe && break; sleep 2; done
    dorado_existe || die "kling save failed ($cold kept)"
  fi
  log "dorado: guardado en $(secs "$t0" "$(now_ms)") s"
  k rm -f "$cold" >/dev/null
}

# ── identidad por clon ───────────────────────────────────────────────────────
# Un android_id nuevo (16 hex, como los de Android), el nombre del teléfono, una
# serie y las claves públicas de adb que abren ESTE clon. El JSON va por la
# entrada estándar de `kling machine secret`: nunca en argv. Una imagen sin
# kling-phoned ignora lo que no sea android_id y name.
adb_pubkey() {
  local m="$1" f
  for f in "${PHONE_ADB_KEYS_DIR:+$PHONE_ADB_KEYS_DIR/$m.pub}" "${PHONE_ADB_PUBKEY:-$HOME/.android/adbkey.pub}"; do
    if [ -n "$f" ] && [ -f "$f" ]; then cat "$f"; return 0; fi
  done
  return 0
}

identidad_json() {
  local m="$1" id="$2" serie
  serie="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n' | tr 'a-f' 'A-F')"
  adb_pubkey "$m" | perl -MJSON::PP -e '
    my ($id, $name, $serial, $ssaid, $toksum) = @ARGV; local $/; my $k = <STDIN> // "";
    my @keys = grep { /\S/ } split /\n/, $k;
    my %p = (android_id => $id, name => $name, serial => $serial);
    $p{adb_keys} = \@keys if @keys; $p{ssaid} = $ssaid if $ssaid ne "";
    $p{api_tokens} = [{sha256 => $toksum, scope => "control"}] if $toksum ne "";
    print encode_json({phone => \%p});' "$id" "$m" "${PHONE_SERIAL:-$serie}" "${PHONE_SSAID:-}" "${3:-}"
}

# token_nuevo TEL: un token para la API de kling-phoned de ESTE teléfono
# (docs/phoned.md, #110). Se guarda en $PHONE_ROOT/tokens/<tel> (0600) y al
# teléfono solo le llega su sha256. Imprime el sha256.
token_nuevo() {
  local tok
  mkdir -p "$PHONE_ROOT/tokens" && chmod 700 "$PHONE_ROOT/tokens"
  tok="kph_$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  ( umask 077; printf '%s\n' "$tok" >"$PHONE_ROOT/tokens/$1" )
  printf '%s' "$tok" | { sha256sum 2>/dev/null || shasum -a 256; } | cut -c1-64
}

identidad() {
  # PHONE_ANDROID_ID: conservar el de un teléfono que se rehace (16 hex).
  local m="$1" id="${PHONE_ANDROID_ID:-}"
  [ -n "$id" ] || id="$(od -An -N8 -tx1 /dev/urandom | tr -d ' \n')"
  local toksum
  toksum="$(token_nuevo "$m")"
  if nucleo_listo; then
    # -hooks: PUT del almacén y después los ganchos de la imagen (10-identity
    # la aplica), esperando a que acaben con éxito.
    identidad_json "$m" "$id" "$toksum" | kq machine secret "$m" -hooks || return 1
  else
    identidad_json "$m" "$id" "$toksum" | k machine secret "$m" >/dev/null || return 1
    ax "$m" 30 --identity >/dev/null || return 1
  fi
  # Aplicada: el secreto ya no hace falta en el metadata service. PUT pisa el
  # almacén entero. Con el núcleo nuevo, además levanta la marca de secretos
  # (los ganchos lo consumieron); con v0.16 la máquina sigue marcada.
  printf '{}' | k machine secret "$m" >/dev/null || return 1
}

siguiente_nombre() {
  local i=1
  while existe "$PREFIX-$i"; do i=$((i + 1)); done
  echo "$PREFIX-$i"
}

# nuevo NOMBRE: restaura un teléfono del dorado y lo deja listo. Imprime los tiempos.
nuevo() {
  local m="$1" t0 t1 t2 t3 p s
  espera_memoria
  t0="$(now_ms)"
  # `kling run -from` sin -egress pone "none" aunque el dorado diga otra cosa
  # (medido en v0.16): se le pasa la del dorado, o PHONE_EGRESS si se dio.
  # main ya lo hereda (4b95e4f, "run -from hereda el egress de la plantilla"):
  # quitar esto cuando la versión mínima de kling sea la que lo trae (v0.17).
  # -wait-ready: el dorado se guardó listo, así que la copia lo está en cuanto
  # acaban los ganchos tras restaurar (sin identidad en MMDS aún, no hacen nada).
  local listo=""
  nucleo_listo && listo="-wait-ready"
  # shellcheck disable=SC2086
  kq run -from "$GOLDEN" -name "$m" -ttl "$TTL" -egress "${PHONE_EGRESS:-$(egress_dorado)}" $listo ||
    die "kling run -from $GOLDEN failed"
  t1="$(now_ms)"
  # La identidad antes que adb: con adb con claves, la de este clon llega con ella.
  if ! identidad "$m"; then
    salud "$m" || true
    die "$m: could not apply its identity; Android is not healthy (above). Remove it: phone.sh rm $m"
  fi
  t2="$(now_ms)"
  p="$(puerto "$m" 5555)"
  if [ -n "$p" ] && adb_usable "$m"; then
    adb_listo "$p" 30 || die "$m: adb does not answer on $p"
  elif [ "$PHONE_EXEC" = 0 ]; then
    api "$m" GET /v1/health >/dev/null || die "$m: kling-phoned is not healthy"
  else
    ax "$m" 10 true >/dev/null
  fi
  t3="$(now_ms)"
  s="restore $(secs "$t0" "$t1") s, identidad $(secs "$t1" "$t2") s, adb/api listo $(secs "$t2" "$t3") s"
  printf '%s\tadb %s\t(%s)\n' "$m" "${p:--}" "$s"
}

# ── órdenes ──────────────────────────────────────────────────────────────────
cmd_up() {
  local n=1
  while [ $# -gt 0 ]; do
    case "$1" in
      -n) [ $# -ge 2 ] || die "-n needs a number"; n="$2"; shift 2 ;;
      -n*) n="${1#-n}"; shift ;;
      *) die "up: unknown argument $1" ;;
    esac
  done
  case "$n" in ''|*[!0-9]*) die "-n must be a number" ;; esac
  ensure_daemon
  dorado_existe || dorado_crea
  local i
  for i in $(seq 1 "$n"); do nuevo "$(siguiente_nombre)"; done
}

cmd_ls() {
  ensure_daemon
  {
    printf 'NAME\tSTATE\tADB\n'
    phones_tsv
  } | column -t -s "$(printf '\t')"
}

cmd_view() {
  local m s f
  [ $# -eq 1 ] || die "usage: phone.sh view <tel>"
  m="$(nombre "$1")"
  ensure_daemon
  existe "$m" || die "no phone $m (phone.sh ls)"
  if command -v scrcpy >/dev/null 2>&1 && tiene_adb; then
    s="$(serial "$m")"
    log "scrcpy -s $s"
    ( nohup scrcpy -s "$s" --window-title "$m" >/dev/null 2>&1 & )
    return 0
  fi
  # Sin scrcpy: una captura en Vista Previa (por la API de kling-phoned si no
  # hay adb o exec). El VNC de Redroid no sirve: con render por software manda
  # la pantalla en negro (#96, docs/telefono.md).
  mkdir -p "$PHONE_ROOT/screens"
  f="$PHONE_ROOT/screens/$m.png"
  if adb_usable "$m"; then
    s="$(serial "$m")"
    adb -s "$s" exec-out screencap -p >"$f" || die "screencap failed"
  else
    api "$m" GET '/v1/screen?encoding=base64' >"$f" || die "screen failed (kling-phoned)"
  fi
  log "captura de $m en $f (en vivo: brew install scrcpy y otra vez phone.sh view $m)"
  [ "$OS" = Linux ] || open "$f"
}

cmd_adb() {
  [ $# -ge 1 ] || die "usage: phone.sh adb <tel> [adb args...]"
  local m s
  m="$(nombre "$1")"; shift
  ensure_daemon
  s="$(serial "$m")"
  if [ $# -eq 0 ]; then echo "$s"; return 0; fi
  exec adb -s "$s" "$@"
}

cmd_shell() {
  [ $# -ge 1 ] || die "usage: phone.sh shell <tel> [command...]"
  local m s
  m="$(nombre "$1")"; shift
  ensure_daemon
  if tiene_adb; then
    s="$(serial "$m")"
    exec adb -s "$s" shell "$@"
  fi
  [ $# -ge 1 ] || die "without adb only 'phone.sh shell <tel> <command>' works (kling exec)"
  exec "$KLING" exec "$m" -- android-sh "$@"
}

cmd_api() {
  [ $# -ge 3 ] || die "usage: phone.sh api <tel> <METHOD> <path> [body-file]   (e.g. api 1 GET /v1/health)"
  local m
  m="$(nombre "$1")"; shift
  ensure_daemon
  api "$m" "$@"
}

cmd_pause() {
  [ $# -ge 1 ] || die "usage: phone.sh pause <tel>..."
  ensure_daemon
  local a m
  for a in "$@"; do
    m="$(nombre "$a")"
    adb_olvida "$m"
    k pause "$m" >/dev/null
    echo "$m paused"
  done
}

cmd_resume() {
  [ $# -ge 1 ] || die "usage: phone.sh resume <tel>..."
  ensure_daemon
  local a m t0 t1 p
  for a in "$@"; do
    m="$(nombre "$a")"
    t0="$(now_ms)"
    k thaw "$m" >/dev/null
    t1="$(now_ms)"
    p="$(puerto "$m" 5555)"
    if [ -n "$p" ] && adb_usable "$m"; then
      adb_listo "$p" 20 || die "$m: adb does not answer on $p after resume"
    elif [ "$PHONE_EXEC" = 0 ]; then
      api "$m" GET /v1/health >/dev/null || die "$m: kling-phoned is not healthy after resume"
    fi
    printf '%s\tadb %s\t(thaw %s s, adb listo %s s)\n' \
      "$m" "${p:--}" "$(secs "$t0" "$t1")" "$(secs "$t0" "$(now_ms)")"
  done
}

cmd_pool() {
  [ $# -eq 1 ] || die "usage: phone.sh pool <N>"
  case "$1" in ''|*[!0-9]*) die "pool: N must be a number" ;; esac
  ensure_daemon
  dorado_existe || dorado_crea
  local have m
  have="$(phones_tsv | awk -F'\t' '$2 == "paused"' | awk 'END { print NR }')"
  while [ "$have" -lt "$1" ]; do
    m="$(siguiente_nombre)"
    nuevo "$m" >/dev/null
    adb_olvida "$m"
    k pause "$m" >/dev/null
    have=$((have + 1))
    echo "$m ready (paused)"
  done
  echo "pool: $have paused phone(s); phone.sh resume <tel> to use one"
}

cmd_rm() {
  [ $# -ge 1 ] || die "usage: phone.sh rm <tel>... | -a"
  ensure_daemon
  local lista="" a m
  if [ "$1" = -a ]; then
    lista="$(phones_tsv | cut -f1)"
  else
    for a in "$@"; do lista="$lista $(nombre "$a")"; done
  fi
  for m in $lista; do
    adb_olvida "$m"
    k rm -f "$m" >/dev/null && rm -f "$PHONE_ROOT/tokens/$m" && echo "$m removed"
  done
}

cmd_golden() {
  [ "${1:-}" = rebuild ] || die "usage: phone.sh golden rebuild"
  ensure_daemon
  if dorado_existe; then
    k template rm -f "$GOLDEN" >/dev/null || die "could not remove template $GOLDEN (phones still using it? phone.sh rm -a)"
  fi
  dorado_crea
}

cmd_daemon() {
  case "${1:-}" in
    "") if daemon_up; then k status | sed -n '1,3p'; else echo "daemon: not running (root $PHONE_ROOT)"; fi ;;
    stop) daemon_stop ;;
    *) die "usage: phone.sh daemon [stop]" ;;
  esac
}

main() {
  [ $# -ge 1 ] || uso 2
  local c="$1"; shift
  case "$c" in
    up) cmd_up "$@" ;;
    ls|ps) cmd_ls ;;
    view) cmd_view "$@" ;;
    adb) cmd_adb "$@" ;;
    shell) cmd_shell "$@" ;;
    api) cmd_api "$@" ;;
    pause) cmd_pause "$@" ;;
    resume|thaw) cmd_resume "$@" ;;
    pool) cmd_pool "$@" ;;
    rm) cmd_rm "$@" ;;
    golden) cmd_golden "$@" ;;
    daemon) cmd_daemon "$@" ;;
    -h|--help|help) uso 0 ;;
    *) printf 'phone.sh: unknown command %s\n\n' "$c" >&2; uso 2 ;;
  esac
}

main "$@"
