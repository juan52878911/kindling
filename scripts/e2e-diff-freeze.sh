#!/usr/bin/env bash
# Congelado diferencial de punta a punta (docs/imagenes.md, «Congelado
# diferencial»; docs/cow.md, «La memoria también») contra daemons PRIVADOS que
# levanta la propia prueba, uno por montaje:
#
#   almacen    KLING_COW=reflink-store, con jailer
#   off        KLING_COW=off, con jailer
#   nojailer   KLING_COW=off, KLING_JAILER=0
#
# En cada uno: un Postgres (`kling run -image postgres:17-alpine`) con 1000
# filas y 8 MiB aleatorios en /dev/shm del invitado se guarda como dorado; una
# copia (`run -from`) pone a cero esos 8 MiB y da tres vueltas de freeze→thaw,
# escribiendo en cada una 4 MiB nuevos en /dev/shm y 500 filas. Comprueba las
# filas, que la página puesta a cero vuelve a cero (un hueco en el diff diría
# «como en el dorado» y devolvería lo del dorado), la RAM nueva, que el sello
# (machines/<id>/volcado.ok) apunta al mem.file del dorado, que el diff es
# pequeño, que state.json lleva schema 2 mientras existe la copia y 1 al
# borrarla, y que no quedan restos en machines/ ni cow/m/.
#
# Corre EN el host Linux con KVM, con sudo, y no toca el daemon del sistema:
#
#   make daemon guest GOARCH=amd64          # en el repo
#   scp kling-linux-amd64 kling-guest scripts/e2e-diff-freeze.sh lab:e2e/
#   ssh lab 'cd e2e && flock /home/juan/.kling-lab.lock \
#            env KLING=./kling-linux-amd64 GUEST=./kling-guest ./e2e-diff-freeze.sh'
#
# Variables: KLING y GUEST (los binarios recién compilados), BASE (raíz de
# usar y tirar, /srv/kt-e2e-diff: no bajo /root, que el usuario del VMM no
# atraviesa sin jailer), SOCK, VMLINUX (núcleo de los invitados), MODOS
# (subconjunto de «almacen off nojailer»), MIN_FREE_MIB (8192: por debajo no
# empieza un montaje), IMAGE. La clave de Postgres sale de POSTGRES_PASSWORD
# o se genera; va por el entorno, nunca en argv.
#
# Sale con código distinto de cero ante cualquier fallo y limpia siempre
# (daemon parado, montajes fuera, raíz borrada).
set -uo pipefail

KLING="${KLING:?KLING: the freshly built kling binary (make daemon)}"
GUEST="${GUEST:?GUEST: the freshly built kling-guest (make guest)}"
BASE="${BASE:-/srv/kt-e2e-diff}"
SOCK="${SOCK:-/run/kt-e2e-diff/kling.sock}"
VMLINUX="${VMLINUX:-/var/lib/kindling/images/vmlinux}"
MODOS="${MODOS:-almacen off nojailer}"
MIN_FREE_MIB="${MIN_FREE_MIB:-8192}"
IMAGE="${IMAGE:-postgres:17-alpine}"
SCRIPTS="$(cd "$(dirname "$0")" && pwd)"
R="$BASE/root"
P=e2ed   # prefijo de máquinas y dorados

[ "$(uname -s)" = Linux ] || { echo "this test runs on the Linux host with KVM" >&2; exit 2; }
case "$BASE" in /root/*|/root) echo "BASE under /root: the VMM user can't traverse it without the jailer" >&2; exit 2;; esac
for f in "$KLING" "$GUEST"; do [ -x "$f" ] || { echo "missing $f" >&2; exit 2; }; done
KLING="$(cd "$(dirname "$KLING")" && pwd)/$(basename "$KLING")"
GUEST="$(cd "$(dirname "$GUEST")" && pwd)/$(basename "$GUEST")"
for c in python3 sudo md5sum findmnt; do command -v "$c" >/dev/null || { echo "missing $c" >&2; exit 2; }; done
sudo test -f "$VMLINUX" || { echo "missing kernel $VMLINUX (VMLINUX=)" >&2; exit 2; }
if pgrep -f "[k]ling daemon -root $R" >/dev/null; then echo "a daemon is already running on $R" >&2; exit 2; fi

export KLING_HOST="unix://$SOCK"
# Que la configuración del usuario (contexto activo, defaults) no cambie nada.
TMPC="$(mktemp -d)"
export KLING_CONFIG="$TMPC/config.json"
if [ -z "${POSTGRES_PASSWORD:-}" ]; then
  POSTGRES_PASSWORD="$(head -c 18 /dev/urandom | base64 | tr -d '/+=')"
fi
export POSTGRES_PASSWORD

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFAIL\033[0m  %s\n        expected: %s\n        got:      %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
igual() { if [ "$2" = "$3" ]; then ok "$1 = $2"; else bad "$1" "$3" "$2"; fi; }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }
info() { printf "        %s\n" "$1"; }
k() { "$KLING" "$@"; }
ms() { date +%s%3N; }
seg() { local d=$(( $(ms) - $1 )); printf '%d.%03d' $((d/1000)) $((d%1000)); }
libre_mib() { df --output=avail -m "$(dirname "$BASE")" | tail -1 | tr -d ' '; }

maquinas() { # nombres de todo lo que hay en el daemon
  k ps -a -json 2>/dev/null | python3 -c '
import sys, json
d = json.load(sys.stdin)
d = d if isinstance(d, list) else d.get("machines", d)
print(" ".join(m["name"] for m in d))' 2>/dev/null
}
id_de() {
  k ps -a -json | python3 -c '
import sys, json
d = json.load(sys.stdin)
d = d if isinstance(d, list) else d.get("machines", d)
print([m for m in d if m["name"] == sys.argv[1]][0]["id"])' "$1" 2>/dev/null
}
esquema() { sudo python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("schema"))' "$R/state.json" 2>&1 | tail -1; }
diff_base() { sudo python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get("diff_base") or "FULL")' "$R/machines/$1/volcado.ok" 2>&1 | tail -1; }
md5g() { k exec "$1" -- sh -c "md5sum /dev/shm/$2 | cut -c1-12" 2>&1 | tail -1; }
filas() { k exec "$1" -- psql -U postgres -tAc "select count(*) from t" 2>&1 | tr -d ' ' | tail -1; }

DPID=""
arrancar() { # $@ = entorno del montaje
  sudo mkdir -p "$BASE/lib" "$R/images" "$(dirname "$SOCK")"
  sudo cp "$KLING" "$BASE/kling"
  sudo cp "$GUEST" "$BASE/lib/kling-guest"
  for s in 81-base-image.sh lib-ext4-shrink.sh minimal-init.sh overlay-init.sh; do
    sudo cp "$SCRIPTS/$s" "$BASE/lib/"
  done
  sudo cp "$VMLINUX" "$R/images/vmlinux"
  # El log es del usuario a propósito: lo lee la prueba sin sudo.
  # KLING_BUILDERS_DIR a un directorio vacío: si no, el constructor oci
  # instalado en el host (/usr/local/lib/kindling/builders/oci, que lanza el
  # kling del sistema) haría las imágenes con OTRO binario que el probado.
  # shellcheck disable=SC2024
  sudo env "$@" KLING_LIB_DIR="$BASE/lib" KLING_GUEST_AGENT="$BASE/lib/kling-guest" \
    KLING_BUILDERS_DIR="$BASE/no-builders" \
    KLING_COW_STORE_GIB=3 KLING_MIN_FREE_DISK_MIB=4096 KLING_GC_DISK_HIGH=99 \
    KLING_SOCKET_USER="$(id -un)" \
    setsid "$BASE/kling" daemon -root "$R" -socket "$SOCK" >>"$TMPC/daemon.log" 2>&1 </dev/null &
  for _ in $(seq 1 150); do k ps >/dev/null 2>&1 && break; sleep 0.2; done
  DPID=$(pgrep -f "^$BASE/kling daemon -root $R" | head -1)
  k ps >/dev/null 2>&1
}
parar() {
  local pid
  pid=$(pgrep -f "^$BASE/kling daemon -root $R" | head -1)
  [ -n "$pid" ] || return 0
  sudo kill "$pid"
  for _ in $(seq 1 150); do sudo kill -0 "$pid" 2>/dev/null || return 0; sleep 0.1; done
  sudo kill -9 "$pid" 2>/dev/null; sleep 0.5
}
desmontar() { # todo lo montado bajo BASE, de dentro afuera
  local t
  for t in $(findmnt -rn -o TARGET | grep "^$BASE/" | sort -r); do sudo umount -l "$t"; done
}
limpiar_montaje() {
  for m in $(maquinas); do k rm -f "$m" >/dev/null 2>&1; done
  parar
  desmontar
  # La caché OCI sobrevive entre montajes: el pull de la imagen no es lo que
  # se prueba.
  # La caché entera: cache/oci (constructores como root) y cache/builder/oci
  # (el oci sin root, con su dueño: mv lo conserva).
  sudo sh -c "mkdir -p '$BASE/keep' && if [ -d '$R/cache' ]; then rm -rf '$BASE/keep/cache'; mv '$R/cache' '$BASE/keep/cache'; fi; rm -rf '$R'"
}
limpiar() {
  limpiar_montaje
  sudo rm -rf "$BASE" "$(dirname "$SOCK")"
  rm -rf "$TMPC"
  local quedan; quedan=$(findmnt -rn -o TARGET | grep -c "^$BASE" || true)
  echo; echo "cleanup: daemon stopped, $BASE removed, $quedan mounts left; $(libre_mib) MiB free"
}
trap limpiar EXIT
trap 'exit 130' INT TERM

escenario() { # $1 = nombre, $2.. = entorno del daemon
  local modo=$1; shift
  step "== $modo ($*)"
  local libre; libre=$(libre_mib)
  if [ "$libre" -lt "$MIN_FREE_MIB" ]; then bad "$modo: free disk" ">= $MIN_FREE_MIB MiB" "$libre MiB"; return; fi
  if [ -d "$BASE/keep/cache" ]; then sudo mkdir -p "$R" && sudo mv "$BASE/keep/cache" "$R/cache"; fi
  : >"$TMPC/daemon.log"
  if ! arrancar "$@"; then bad "$modo: daemon" "started" "$(tail -3 "$TMPC/daemon.log")"; limpiar_montaje; return; fi
  ok "daemon started (pid ${DPID:-?})"

  # ── El dorado ───────────────────────────────────────────────────────────
  local g0=$P-g0 gold=$P-gold n=$P-c-$modo s out
  s=$(ms)
  if ! out=$(k run -image "$IMAGE" -e POSTGRES_PASSWORD -name "$g0" -mem 512M -allow-exec -wait-ready 2>&1); then bad "$modo: run -image $IMAGE" "running" "$(printf '%s' "$out" | tail -2)"; limpiar_montaje; return; fi
  info "run -image (import + boot): $(seg "$s")s"
  k exec "$g0" -- psql -U postgres -c "create table t(n int); insert into t select generate_series(1,1000)" >/dev/null 2>&1
  k exec "$g0" -- sh -c "dd if=/dev/urandom of=/dev/shm/a bs=1M count=8 2>/dev/null; sync"
  local a_dorado a_ceros
  a_dorado=$(md5g "$g0" a)
  a_ceros=$(k exec "$g0" -- sh -c "head -c 8388608 /dev/zero | md5sum | cut -c1-12" 2>&1 | tail -1)
  igual "golden rows" "$(filas "$g0")" 1000
  out=$(k save "$g0" "$gold" 2>&1) || { bad "$modo: save" "a golden" "$(printf '%s' "$out" | tail -1)"; limpiar_montaje; return; }
  k rm -f "$g0" >/dev/null 2>&1
  info "golden $gold: /dev/shm/a=$a_dorado"

  # ── La copia ────────────────────────────────────────────────────────────
  s=$(ms)
  if ! out=$(k run -from "$gold" -name "$n" -allow-exec -wait-ready 2>&1); then bad "$modo: run -from" "running" "$(printf '%s' "$out" | tail -1)"; limpiar_montaje; return; fi
  info "copy ready in $(seg "$s")s"
  local id; id=$(id_de "$n")
  [ -n "$id" ] || { bad "$modo: copy id" "an id" "nothing"; limpiar_montaje; return; }
  igual "RAM inherited from the golden" "$(md5g "$n" a)" "$a_dorado"
  # Las páginas que el invitado pone a cero tienen que volver a cero, no con
  # lo del dorado.
  k exec "$n" -- sh -c "dd if=/dev/zero of=/dev/shm/a bs=1M count=8 conv=notrunc 2>/dev/null"

  local vuelta b_md5 esperadas=1000 fz th mem rc donde ocupa
  for vuelta in 1 2 3; do
    k exec "$n" -- sh -c "dd if=/dev/urandom of=/dev/shm/b bs=1M count=4 2>/dev/null"
    b_md5=$(md5g "$n" b)
    k exec "$n" -- psql -U postgres -c "insert into t select generate_series(1,500)" >/dev/null 2>&1
    esperadas=$((esperadas+500))

    s=$(ms); out=$(k freeze "$n" 2>&1); rc=$?; fz=$(seg "$s")
    [ $rc -eq 0 ] || bad "cycle $vuelta: freeze" "ok" "$(printf '%s' "$out" | tail -1)"
    # Tamaño del diff: lo que ocupa en disco (-L: en el almacén es un enlace).
    mem=$(sudo sh -c "du -L --block-size=1M '$R/machines/$id/mem.file' | cut -f1; du -L --apparent-size --block-size=1M '$R/machines/$id/mem.file' | cut -f1" 2>/dev/null | tr '\n' ' ')
    donde=$(sudo readlink "$R/machines/$id/mem.file" 2>/dev/null || echo "machines/$id")
    info "cycle $vuelta: freeze ${fz}s, diff $(echo "$mem" | awk '{print $1 " MiB on disk of " $2}') MiB ($donde)"
    igual "cycle $vuelta: seal diff_base" "$(diff_base "$id")" "$R/snapshots/$gold/mem.file"
    ocupa=$(echo "$mem" | awk '{print $1}')
    if [ -n "$ocupa" ] && [ "$ocupa" -lt 256 ]; then ok "cycle $vuelta: diff is small (${ocupa} MiB of 512)"
    else bad "cycle $vuelta: diff size" "< 256 MiB (only dirty pages)" "${ocupa:-?} MiB"; fi
    igual "cycle $vuelta: state.json schema while frozen" "$(esquema)" 2

    s=$(ms); out=$(k thaw "$n" 2>&1); rc=$?; th=$(seg "$s")
    [ $rc -eq 0 ] || bad "cycle $vuelta: thaw" "ok" "$(printf '%s' "$out" | tail -1)"
    info "cycle $vuelta: thaw ${th}s"
    igual "cycle $vuelta: rows" "$(filas "$n")" "$esperadas"
    igual "cycle $vuelta: zeroed page" "$(md5g "$n" a)" "$a_ceros"
    igual "cycle $vuelta: RAM written in the copy" "$(md5g "$n" b)" "$b_md5"
    igual "cycle $vuelta: state.json schema while running" "$(esquema)" 2
  done

  k rm -f "$n" >/dev/null 2>&1; sleep 1
  igual "schema 1 s after rm" "$(esquema)" 1
  igual "leftovers in machines/" "$(sudo sh -c "ls -A '$R/machines' 2>/dev/null | wc -l")" 0
  igual "leftovers in cow/m/" "$(sudo sh -c "ls -A '$R/cow/m' 2>/dev/null | wc -l")" 0
  local pan; pan=$(grep -c "panic" "$TMPC/daemon.log" || true)
  igual "panics in the daemon log" "$pan" 0
  local avisos; avisos=$(grep -iE "warning|error|fail" "$TMPC/daemon.log" | grep -viE "deprecated|SECURITY WARNING" | tail -5 | cut -c1-200)
  [ -z "$avisos" ] || { info "daemon warnings (last 5):"; printf '%s\n' "$avisos" | sed 's/^/          /'; }
  limpiar_montaje
}

for modo in $MODOS; do
  case "$modo" in
    almacen)  escenario almacen KLING_COW=reflink-store ;;
    off)      escenario off KLING_COW=off ;;
    nojailer) escenario nojailer KLING_COW=off KLING_JAILER=0 ;;
    *) bad "mode" "almacen, off or nojailer" "$modo" ;;
  esac
done

echo; echo "$pass ok, $fail failed"
[ "$fail" -eq 0 ]
