#!/usr/bin/env bash
# e2e de `kling upgrade`: de la release anterior a la de este árbol, y vuelta.
#
# Lo que los tests de internal/upgrade no pueden ver: que un daemon de verdad,
# con máquinas congeladas, paradas y un dorado hechos por la versión anterior,
# pase a la nueva por systemd sin perder nada, que eso de verdad despierte, y
# que -rollback (y la vuelta atrás automática cuando el nuevo no arranca)
# devuelvan la versión anterior con su estado. Necesita KVM y root: corre en el
# host del daemon (docs/actualizar.md §3.5), y es obligatorio antes de cada
# release (docs/releases.md).
#
# No toca el daemon del sistema: monta uno privado con su propia unidad de
# systemd (de tiempo de ejecución, en /run/systemd/system: desaparece al
# reiniciar), su raíz bajo /srv y su socket bajo /run, y lo borra todo al acabar.
#
#   sudo OLD_DIR=/ruta/v0.17 NEW_DIR=/ruta/head ./94-e2e-upgrade.sh
#
#   OLD_DIR   kling (y kling-guest) de la versión anterior; sin él, OLD_TAG se
#             baja con install.sh (necesita red)
#   OLD_TAG   la etiqueta anterior (por defecto v0.17.0)
#   NEW_DIR   los binarios nuevos, con nombre de release (kling-linux-amd64…) o
#             a secas (kling, kling-guest), y SHA256SUMS si se quiere verificar
#   NAME      nombre del daemon privado (por defecto kt-e2e-upgrade): unidad
#             NAME.service, raíz /srv/NAME, socket /run/NAME/kling.sock
#   IMAGES    de dónde copiar el kernel y las imágenes (/var/lib/kindling/images)
#   IMAGE     imagen con agente para las máquinas (toolchain; min si no hay)
#   SOCKET_USER  a quién se cede el socket (por defecto $SUDO_USER)
#   KEEP=1    no limpia al terminar
#
# Cada comprobación dice qué esperaba y qué obtuvo; un fallo no aborta el
# resto. Salida en inglés, como el CLI.
set -uo pipefail

OLD_TAG="${OLD_TAG:-v0.17.0}"
OLD_DIR="${OLD_DIR:-}"
NEW_DIR="${NEW_DIR:?NEW_DIR: directory with the new binaries}"
NAME="${NAME:-kt-e2e-upgrade}"
IMAGES="${IMAGES:-/var/lib/kindling/images}"
IMAGE="${IMAGE:-toolchain}"
SOCKET_USER="${SOCKET_USER:-${SUDO_USER:-}}"
KEEP="${KEEP:-0}"

BASE="/srv/$NAME"
ROOT="$BASE/root"
BIN="$BASE/bin"
LIB="$BASE/lib"
RUN="/run/$NAME"
SOCK="$RUN/kling.sock"
UNIT="/run/systemd/system/$NAME.service"
ARCH="$(uname -m)"; case "$ARCH" in x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; esac

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFAIL\033[0m  %s\n     want: %s\n     got:  %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }
die()  { echo "$*" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "run it as root (sudo): it installs a systemd unit and kling upgrade needs root"
case "$NAME" in *[!A-Za-z0-9_-]*|"") die "invalid NAME";; esac
[ -e "$BASE" ] && die "$BASE already exists: another run? remove it or pick NAME"
command -v python3 >/dev/null || die "python3 missing"

# (El daemon dice su versión sin la "v": se compara sin ella.)
# El CLI viejo para crear estado y el nuevo para actualizar (el viejo no tiene
# upgrade), los dos contra el socket del daemon privado y nunca contra el del
# sistema.
export KLING_HOST="unix://$SOCK"
NEWK="$NEW_DIR/kling-linux-$ARCH"; [ -x "$NEWK" ] || NEWK="$NEW_DIR/kling"
[ -x "$NEWK" ] || die "no kling in $NEW_DIR"
OLDK="$BIN/kling.old-cli"   # copia aparte: upgrade cambia $BIN/kling
k()  { "$OLDK" "$@"; }
kn() { "$NEWK" "$@"; }
# Las dos versiones tienen que ser las de verdad: con binarios sin
# -ldflags "-X main.Version=..." (los dos "dev") upgrade contesta "nothing to
# do" y todo lo demás falla en cascada.
NEWV=$(kn version 2>/dev/null | awk 'NR==1{print $2}')
case "$NEWV" in ""|dev|"${OLD_TAG}"|"${OLD_TAG#v}") die "the new kling says version \"$NEWV\": build NEW_DIR with -ldflags \"-X main.Version=\$(git describe --tags)\"";; esac

cleanup() {
  if [ "$KEEP" = 1 ]; then echo; echo "KEEP=1: $BASE and $NAME.service left as they are"; return; fi
  echo; echo "cleaning up..."
  for m in $(kn ps -a -q 2>/dev/null); do kn rm "$m" >/dev/null 2>&1; done
  for t in $(kn template ls -q 2>/dev/null); do kn template rm "$t" >/dev/null 2>&1; done
  systemctl stop "$NAME" >/dev/null 2>&1
  # KillMode=process deja vivas las microVMs: las que quedaran, fuera.
  pkill -f -- "$ROOT/" >/dev/null 2>&1
  rm -f "$UNIT"; systemctl daemon-reload
  # Montajes que el daemon dejara bajo su raíz.
  grep -o " $ROOT[^ ]*" /proc/mounts | sort -r | while read -r mnt; do umount -l "$mnt" 2>/dev/null; done
  rm -rf "$BASE" "$RUN"
}
trap cleanup EXIT

# ── 0. daemon privado con la versión anterior ────────────────────────────────
step "0. Private $OLD_TAG daemon ($NAME.service, $ROOT)"
mkdir -p "$BIN" "$LIB" "$ROOT/images" "$RUN"
chmod 755 "$BASE" "$ROOT"   # el usuario del VMM tiene que poder atravesarla
if [ -z "$OLD_DIR" ]; then
  OLD_DIR="$BASE/old"; mkdir -p "$OLD_DIR"
  curl -fsSL "https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh" \
    | sh -s -- --tag "$OLD_TAG" --prefix "$OLD_DIR" --no-rc --force >/dev/null || die "install.sh --tag $OLD_TAG failed"
fi
install -m755 "$OLD_DIR/kling" "$BIN/kling"
install -m755 "$OLD_DIR/kling" "$OLDK"
[ -f "$OLD_DIR/kling-guest" ] && install -m755 "$OLD_DIR/kling-guest" "$LIB/kling-guest"
for f in vmlinux min.ext4; do cp "$IMAGES/$f" "$ROOT/images/$f" || die "no $IMAGES/$f"; done
if [ "$IMAGE" != min ]; then
  cp "$IMAGES/$IMAGE.layer.ext4" "$IMAGES/$IMAGE.recipe.json" "$ROOT/images/" 2>/dev/null \
    || { echo "  (no $IMAGE image in $IMAGES: using min)"; IMAGE=min; }
fi
cat > "$UNIT" <<EOF
[Unit]
Description=kling e2e upgrade daemon ($NAME)
[Service]
Environment=KLING_LIB_DIR=$LIB
Environment=KLING_SOCKET_USER=$SOCKET_USER
Environment=KLING_MIN_FREE_DISK_MIB=1024
ExecStart=$BIN/kling daemon -root $ROOT -socket $SOCK
KillMode=process
EOF
systemctl daemon-reload
systemctl start "$NAME" || die "systemctl start $NAME failed"
for _ in $(seq 1 40); do k ps >/dev/null 2>&1 && break; sleep 0.5; done
v=$(k version 2>&1 | sed -n 2p)
# Un OLD_DIR sin su versión (ver arriba): parar aquí, no fallar en cascada.
contiene "$v" "${OLD_TAG#v}" || die "the old daemon says \"$v\", not $OLD_TAG: build OLD_DIR with -ldflags \"-X main.Version=$OLD_TAG\" (or leave OLD_DIR empty to download it)"
ok "daemon $OLD_TAG answers"

# ── 1. estado hecho por la versión anterior ──────────────────────────────────
step "1. State made by $OLD_TAG"
idde() { k ps -a -json 2>/dev/null | python3 -c "import json,sys; print(next((m['id'] for m in json.load(sys.stdin) if m['name']==sys.argv[1]), ''))" "$1"; }
estado() { kn ps -a -json 2>/dev/null | python3 -c "import json,sys; print(next((m['state'] for m in json.load(sys.stdin) if m['id']==sys.argv[1]), 'missing'))" "$1"; }
EXEC=""; [ "$IMAGE" != min ] && EXEC="-allow-exec"
out=$(k run -name up-frozen -image "$IMAGE" $EXEC 2>&1) || bad "run" "a machine" "$out"
if [ -n "$EXEC" ]; then
  # Una marca en RAM (/tmp) que tiene que seguir ahí al despertar con el daemon nuevo.
  for _ in $(seq 1 30); do k exec up-frozen -- sh -c 'echo before-upgrade > /tmp/mark' >/dev/null 2>&1 && break; sleep 1; done
fi
out=$(k freeze up-frozen 2>&1); contiene "$out" "frozen" && ok "frozen machine" || bad "freeze" "frozen" "$out"
out=$(k run -name up-stopped -image min 2>&1) && k stop up-stopped >/dev/null 2>&1 \
  && ok "stopped machine" || bad "stopped machine" "stopped" "$out"
out=$(k run -name up-gold-src -image "$IMAGE" $EXEC 2>&1) && out=$(k save up-gold-src up-gold 2>&1) \
  && ok "template up-gold" || bad "save" "a template" "$out"
k rm up-gold-src >/dev/null 2>&1
out=$(k volume create up-vol -size 64M 2>&1) && ok "volume" || bad "volume create" "a volume" "$out"
FROZEN=$(idde up-frozen); STOPPED=$(idde up-stopped)
schema=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('schema',0) if isinstance(d,dict) else 0)" "$ROOT/state.json")
[ "$schema" = 0 ] && ok "state.json written by $OLD_TAG (schema 0)" || bad "old state.json" "schema 0" "$schema"
cp "$ROOT/state.json" "$BASE/state.before"

# ── 2. upgrade fallido: el nuevo no arranca y se vuelve solo ─────────────────
step "2. A new daemon that does not start is rolled back"
ROTO="$BASE/roto"; mkdir -p "$ROTO"
# Dice ser la versión nueva y entender el estado, pero no arranca como daemon.
cat > "$ROTO/kling" <<EOF
#!/bin/sh
case "\$1" in
  upgrade) echo '{"kling":"$NEWV","api":1,"schemas":{"state":2,"meta":1,"credentials":1}}' ;;
  version) echo "kling $NEWV" ;;
  *) echo "broken on purpose" >&2; exit 1 ;;
esac
EOF
chmod +x "$ROTO/kling"
# Lo demás que se cambia, bueno: solo falla el daemon.
for g in kling-guest kling-chispa; do
  [ -f "$LIB/$g" ] || continue
  src="$NEW_DIR/$g-linux-$ARCH"; [ -f "$src" ] || src="$NEW_DIR/$g"
  cp "$src" "$ROTO/$g"
done
out=$(kn upgrade -unit "$NAME" -from-dir "$ROTO" -timeout 15s 2>&1); rc=$?
[ $rc != 0 ] && contiene "$out" "rolled back" && ok "upgrade refused and rolled back" \
  || bad "broken upgrade" "an error and a rollback" "rc=$rc: $out"
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after rollback" "$OLD_TAG" "$v"
cmp -s "$BIN/kling" "$OLDK" && ok "old binary back in place" || bad "binary" "the old one" "something else"
st=$(estado "$FROZEN"); [ "$st" = frozen ] && ok "frozen machine still frozen" || bad "frozen after rollback" frozen "$st"

# ── 3. upgrade de verdad ─────────────────────────────────────────────────────
step "3. Upgrade $OLD_TAG -> $NEWV"
out=$(kn upgrade -unit "$NAME" -from-dir "$NEW_DIR" -dry-run 2>&1)
contiene "$out" "state.json: schema 0 -> 1" && cmp -s "$BIN/kling" "$OLDK" \
  && ok "dry run lists the migration and changes nothing" || bad "dry run" "the plan, nothing changed" "$out"
t0=$(date +%s%N)
out=$(kn upgrade -unit "$NAME" -from-dir "$NEW_DIR" 2>&1); rc=$?
ms=$(( ($(date +%s%N) - t0) / 1000000 ))
[ $rc = 0 ] && contiene "$out" "upgraded" && ok "kling upgrade in ${ms} ms" || bad "upgrade" "upgraded" "rc=$rc: $out"
v=$(kn version 2>&1 | sed -n 2p)
contiene "$v" "${NEWV#v}" && ok "daemon answers as $NEWV" || bad "new daemon" "$NEWV" "$v"
cmp -s "$BIN/kling" "$NEWK" && ok "$BIN/kling replaced" || bad "binary" "the new one" "something else"
[ -f "$LIB/kling-guest" ] && { cmp -s "$LIB/kling-guest" "$OLD_DIR/kling-guest" && bad "kling-guest" "replaced" "the old one" || ok "kling-guest replaced"; }
schema=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('schema'))" "$ROOT/state.json" 2>&1)
[ "$schema" = 1 ] && ok "state.json migrated to schema 1" || bad "state.json" "schema 1" "$schema"
cmp -s "$ROOT/state.json.v0.bak" "$BASE/state.before" && ok "state.json.v0.bak is the old state" || bad "v0.bak" "the old state.json" "missing or different"
ls "$ROOT/upgrade/backups"/*/manifest.json >/dev/null 2>&1 && ok "backup with manifest" || bad "backup" "upgrade/backups/*/manifest.json" "none"
st=$(estado "$FROZEN"); [ "$st" = frozen ] && ok "frozen machine still frozen" || bad "frozen" frozen "$st"
st=$(estado "$STOPPED"); [ "$st" = stopped ] && ok "stopped machine still stopped" || bad "stopped" stopped "$st"
out=$(kn template ls -q 2>&1); contiene "$out" "up-gold" && ok "template still listed" || bad "template" "up-gold" "$out"
out=$(kn volume ls -q 2>&1); contiene "$out" "up-vol" && ok "volume still listed" || bad "volume" "up-vol" "$out"

# ── 4. lo de antes despierta con el daemon nuevo ─────────────────────────────
step "4. What $OLD_TAG froze wakes up under $NEWV"
out=$(kn thaw up-frozen 2>&1)
contiene "$out" "running" && ok "thaw of the old frozen machine: $(echo "$out" | grep -o '[0-9]* ms' | head -1)" || bad "thaw" "running" "$out"
if [ -n "$EXEC" ]; then
  out=$(kn exec up-frozen -- cat /tmp/mark 2>&1)
  [ "$out" = before-upgrade ] && ok "its RAM survived (/tmp/mark)" || bad "RAM after thaw" before-upgrade "$out"
fi
out=$(kn run -name up-from-gold -from up-gold 2>&1)
contiene "$out" "up-from-gold" && ok "run -from the old template" || bad "run -from" "a machine" "$out"
out=$(kn start up-stopped 2>&1)
contiene "$out" "running" && ok "start of the old stopped machine" || bad "start" "running" "$out"
kn rm up-from-gold >/dev/null 2>&1
kn stop up-stopped >/dev/null 2>&1
# up-frozen se queda despierta: en el state.json al que vuelve -rollback está
# congelada, y upgrade tiene que congelarla otra vez con el nuevo antes de parar.

# ── 5. -rollback a mano ───────────────────────────────────────────────────────
step "5. kling upgrade -rollback"
out=$(kn upgrade -unit "$NAME" -rollback 2>&1); rc=$?
[ $rc = 0 ] && contiene "$out" "rolled back to $OLD_TAG" && ok "rollback" || bad "rollback" "rolled back to $OLD_TAG" "rc=$rc: $out"
contiene "$out" "freezing $FROZEN again" && ok "the machine the new daemon woke was frozen again first" \
  || bad "refreeze before rollback" "freezing $FROZEN again" "$out"
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after -rollback" "$OLD_TAG" "$v"
schema=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('schema',0) if isinstance(d,dict) else 0)" "$ROOT/state.json")
[ "$schema" = 0 ] && ok "state.json back to schema 0" || bad "state.json after -rollback" "schema 0" "$schema"
[ ! -e "$ROOT/state.json.v0.bak" ] && ok "migration copy consumed" || bad "v0.bak" "gone" "still there"
st=$(estado "$FROZEN"); [ "$st" = frozen ] && ok "frozen machine still frozen under $OLD_TAG" || bad "frozen" frozen "$st"
out=$(k thaw up-frozen 2>&1)
contiene "$out" "running" && ok "and $OLD_TAG thaws it" || bad "thaw under $OLD_TAG" "running" "$out"
if [ -n "$EXEC" ]; then
  out=$(k exec up-frozen -- cat /tmp/mark 2>&1)
  [ "$out" = before-upgrade ] && ok "its RAM came back through both freezes" || bad "RAM under $OLD_TAG" before-upgrade "$out"
fi
k freeze up-frozen >/dev/null 2>&1

# ── 6. -rollback con el daemon caído ─────────────────────────────────────────
step "6. kling upgrade -rollback with the daemon down"
out=$(kn upgrade -unit "$NAME" -from-dir "$NEW_DIR" 2>&1); rc=$?
[ $rc = 0 ] && contiene "$out" "upgraded" && ok "upgraded again" || bad "second upgrade" "upgraded" "rc=$rc: $out"
systemctl stop "$NAME"
out=$(kn upgrade -unit "$NAME" -rollback 2>&1); rc=$?
[ $rc = 0 ] && contiene "$out" "no daemon answers" && contiene "$out" "rolled back to $OLD_TAG" \
  && ok "rollback found the backup through the unit" || bad "rollback without a daemon" "rolled back to $OLD_TAG" "rc=$rc: $out"
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after -rollback" "$OLD_TAG" "$v"
st=$(estado "$FROZEN"); [ "$st" = frozen ] && ok "frozen machine still frozen" || bad "frozen" frozen "$st"

echo
echo "upgrade e2e: $pass ok, $fail failed"
[ "$fail" = 0 ]
