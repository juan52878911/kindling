#!/usr/bin/env bash
# e2e de `kling upgrade` en el Mac: de la release anterior a la de este árbol,
# y vuelta, con el agente de launchd de docs/mac.md.
#
# Es el 94-e2e-upgrade.sh del Mac: lo que los tests de cmd/kling no ven del
# camino de launchd (bootout y bootstrap de verdad, el `program` que dice
# `launchctl print`, el plist que se lee con el daemon caído, kling-vz junto a
# kling) con máquinas congeladas, paradas y un dorado hechos por la versión
# anterior. Obligatorio antes de cada release (docs/releases.md, paso 3b).
#
# `kling upgrade` reinicia el agente dev.kindling.daemon del usuario, así que
# la prueba instala UNO SUYO con esa etiqueta, apuntando a un daemon privado
# (sus binarios, su raíz y su socket), y lo quita al acabar. Si el usuario ya
# tiene ese agente (cargado o su plist), se niega: no lo toca.
#
#   OLD_DIR=/ruta/v0.17 NEW_DIR=/ruta/head IMAGES_DIR=/ruta/imagenes ./95-e2e-upgrade-mac.sh
#
#   OLD_DIR     kling y kling-vz (firmado) de la versión anterior
#   OLD_TAG     su etiqueta (por defecto v0.17.0)
#   NEW_DIR     kling y kling-vz de este árbol (nombres de release o a secas)
#   IMAGES_DIR  vmlinux, min.ext4 y, si está, toolchain.layer.ext4 con su receta
#               (los de un daemon Linux arm64: docs/mac.md)
#   BASE        dónde vive todo (por defecto $TMPDIR/kt-e2e-upgrade-mac)
#   KEEP=1      no limpia al terminar
#
# Cada comprobación dice qué esperaba y qué obtuvo; un fallo no aborta el
# resto. Salida en inglés, como el CLI.
set -uo pipefail

OLD_TAG="${OLD_TAG:-v0.17.0}"
OLD_DIR="${OLD_DIR:?OLD_DIR: directory with the previous kling and kling-vz}"
NEW_DIR="${NEW_DIR:?NEW_DIR: directory with the new kling and kling-vz}"
IMAGES_DIR="${IMAGES_DIR:?IMAGES_DIR: directory with vmlinux and min.ext4}"
TMPBASE="${TMPDIR:-/tmp}"; TMPBASE="${TMPBASE%/}"
BASE="${BASE:-$TMPBASE/kt-e2e-upgrade-mac}"
KEEP="${KEEP:-0}"

ROOT="$BASE/root"
BIN="$BASE/bin"
SOCK="/tmp/kt-e2e-upgrade-$(id -u).sock"   # corto: sun_path son 104 bytes
LABEL="dev.kindling.daemon"                  # la que busca kling upgrade
DOMINIO="gui/$(id -u)"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOG="$BASE/daemon.log"

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFAIL\033[0m  %s\n     want: %s\n     got:  %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }
die()  { echo "$*" >&2; exit 1; }
now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
# mismo_estado compara dos state.json por su contenido: v0.17 los escribe
# desde un mapa, en otro orden cada vez.
mismo_estado() {
  python3 - "$1" "$2" <<'PY'
import json, sys
def norma(x):
    if isinstance(x, dict):
        return {k: norma(v) for k, v in x.items()}
    if isinstance(x, list):
        x = [norma(v) for v in x]
        if all(isinstance(v, dict) and "id" in v for v in x):
            x.sort(key=lambda v: v["id"])
        return x
    return x
try:
    a, b = (norma(json.load(open(p))) for p in sys.argv[1:3])
except (OSError, ValueError):
    sys.exit(1)
sys.exit(0 if a == b else 1)
PY
}

[ "$(uname -s)" = Darwin ] || die "this test is for macOS; on Linux use 94-e2e-upgrade.sh"
[ "$(id -u)" != 0 ] || die "run it as your user, not root: launchd agents live in your gui domain"
command -v python3 >/dev/null || die "python3 missing"
# El agente del usuario no se toca: ni uno cargado ni un plist suyo.
launchctl print "$DOMINIO/$LABEL" >/dev/null 2>&1 && die "$LABEL is loaded in $DOMINIO: this test would replace your daemon's agent; stop it first (launchctl bootout $DOMINIO/$LABEL)"
[ -e "$PLIST" ] && die "$PLIST exists: this test would replace your daemon's agent; move it away first"
[ -e "$BASE" ] && die "$BASE already exists: another run? remove it or set BASE"
[ -e "$SOCK" ] && die "$SOCK already exists: another daemon?"

pieza() { local p="$1/$2-darwin-arm64"; [ -x "$p" ] || p="$1/$2"; [ -x "$p" ] || die "no $2 in $1"; echo "$p"; }
NEWK=$(pieza "$NEW_DIR" kling); NEWVZ=$(pieza "$NEW_DIR" kling-vz)
OLDK0=$(pieza "$OLD_DIR" kling); OLDVZ=$(pieza "$OLD_DIR" kling-vz)

# Los dos CLI contra el socket privado, con una configuración vacía: ni el
# contexto ni los valores por defecto del usuario cuentan.
export KLING_HOST="unix://$SOCK"
export KLING_CONFIG="$BASE/config.json"
OLDK="$BASE/kling.old-cli"   # copia aparte: upgrade cambia $BIN/kling
k()  { "$OLDK" "$@"; }
kn() { "$NEWK" "$@"; }
NEWV=$(kn version 2>/dev/null | awk 'NR==1{print $2}')
case "$NEWV" in ""|dev|"${OLD_TAG}"|"${OLD_TAG#v}") die "the new kling says version \"$NEWV\": build NEW_DIR with -ldflags \"-X main.Version=\$(git describe --tags)\"";; esac

our_procs() { pgrep -f -- "$ROOT/" | wc -l | tr -d ' '; }
cleanup() {
  if [ "$KEEP" = 1 ]; then echo; echo "KEEP=1: $BASE and $PLIST left as they are"; return; fi
  echo; echo "cleaning up..."
  for m in $(kn ps -a -q 2>/dev/null); do kn rm -f "$m" >/dev/null 2>&1; done
  for t in $(kn template ls -q 2>/dev/null); do kn template rm "$t" >/dev/null 2>&1; done
  launchctl bootout "$DOMINIO/$LABEL" >/dev/null 2>&1
  rm -f "$PLIST" "$SOCK"
  # Lo que quedara de las máquinas de esta raíz (kling-vz y sus frenos).
  pkill -f -- "$ROOT/" >/dev/null 2>&1
  sleep 1
  rm -rf "$BASE"
  local n; n=$(our_procs)
  [ "$n" = 0 ] && echo "no processes left" || echo "WARNING: $n processes of $ROOT still alive"
  launchctl print "$DOMINIO/$LABEL" >/dev/null 2>&1 && echo "WARNING: $LABEL still loaded" || echo "agent removed"
}
trap cleanup EXIT

# ── 0. daemon privado con la versión anterior, por launchd ───────────────────
step "0. Private $OLD_TAG daemon ($LABEL in $DOMINIO, $ROOT)"
mkdir -p "$BIN" "$ROOT/images"
echo '{}' > "$KLING_CONFIG"
install -m755 "$OLDK0" "$BIN/kling"
install -m755 "$OLDK0" "$OLDK"
cp "$OLDVZ" "$BIN/kling-vz"   # cp conserva la firma con su entitlement
copiar() { cp -c "$1" "$2" 2>/dev/null || cp "$1" "$2"; }
for f in vmlinux min.ext4; do copiar "$IMAGES_DIR/$f" "$ROOT/images/$f" || die "no $IMAGES_DIR/$f"; done
IMAGE=min
if [ -f "$IMAGES_DIR/toolchain.layer.ext4" ] && [ -f "$IMAGES_DIR/toolchain.recipe.json" ]; then
  copiar "$IMAGES_DIR/toolchain.layer.ext4" "$ROOT/images/toolchain.layer.ext4"
  cp "$IMAGES_DIR/toolchain.recipe.json" "$ROOT/images/"
  IMAGE=toolchain
fi
mkdir -p "$(dirname "$PLIST")"
cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN/kling</string>
    <string>daemon</string>
    <string>-root</string>
    <string>$ROOT</string>
    <string>-socket</string>
    <string>$SOCK</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>KLING_CONFIG</key>
    <string>$KLING_CONFIG</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>ExitTimeOut</key>
  <integer>30</integer>
  <key>StandardOutPath</key>
  <string>$LOG</string>
  <key>StandardErrorPath</key>
  <string>$LOG</string>
</dict>
</plist>
EOF
launchctl bootstrap "$DOMINIO" "$PLIST" || die "launchctl bootstrap failed"
for _ in $(seq 1 40); do k ps >/dev/null 2>&1 && break; sleep 0.5; done
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" || die "the old daemon says \"$v\", not $OLD_TAG: build OLD_DIR with -ldflags \"-X main.Version=$OLD_TAG\" (log: $LOG)"
ok "daemon $OLD_TAG answers, run by launchd"

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
k rm -f up-gold-src >/dev/null 2>&1
out=$(k volume create up-vol -size 64M 2>&1) && ok "volume" || bad "volume create" "a volume" "$out"
FROZEN=$(idde up-frozen); STOPPED=$(idde up-stopped)
schema=$(python3 -c "import json,sys; d=json.load(open(sys.argv[1])); print(d.get('schema',0) if isinstance(d,dict) else 0)" "$ROOT/state.json")
[ "$schema" = 0 ] && ok "state.json written by $OLD_TAG (schema 0)" || bad "old state.json" "schema 0" "$schema"

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
cp "$NEWVZ" "$ROTO/kling-vz"   # solo falla el daemon
out=$(kn upgrade -from-dir "$ROTO" -timeout 15s 2>&1); rc=$?
[ $rc != 0 ] && contiene "$out" "rolled back" && ok "upgrade refused and rolled back" \
  || bad "broken upgrade" "an error and a rollback" "rc=$rc: $out"
for _ in $(seq 1 20); do k ps >/dev/null 2>&1 && break; sleep 0.5; done
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after rollback" "$OLD_TAG" "$v"
cmp -s "$BIN/kling" "$OLDK" && ok "old kling back in place" || bad "kling" "the old one" "something else"
cmp -s "$BIN/kling-vz" "$OLDVZ" && ok "old kling-vz back in place" || bad "kling-vz" "the old one" "something else"
st=$(estado "$FROZEN"); [ "$st" = frozen ] && ok "frozen machine still frozen" || bad "frozen after rollback" frozen "$st"

# ── 3. upgrade de verdad ─────────────────────────────────────────────────────
step "3. Upgrade $OLD_TAG -> $NEWV"
out=$(kn upgrade -from-dir "$NEW_DIR" -dry-run 2>&1)
contiene "$out" "state.json: schema 0 -> 1" && cmp -s "$BIN/kling" "$OLDK" \
  && ok "dry run lists the migration and changes nothing" || bad "dry run" "the plan, nothing changed" "$out"
# Lo de justo antes: el viejo reescribe su state.json al pararse y arrancar.
cp "$ROOT/state.json" "$BASE/state.before"
t0=$(now_ms)
out=$(kn upgrade -from-dir "$NEW_DIR" 2>&1); rc=$?
ms=$(( $(now_ms) - t0 ))
[ $rc = 0 ] && contiene "$out" "upgraded" && ok "kling upgrade in ${ms} ms" || bad "upgrade" "upgraded" "rc=$rc: $out"
v=$(kn version 2>&1 | sed -n 2p)
contiene "$v" "${NEWV#v}" && ok "daemon answers as $NEWV" || bad "new daemon" "$NEWV" "$v"
cmp -s "$BIN/kling" "$NEWK" && ok "$BIN/kling replaced" || bad "kling" "the new one" "something else"
cmp -s "$BIN/kling-vz" "$NEWVZ" && ok "$BIN/kling-vz replaced" || bad "kling-vz" "the new one" "something else"
ent=$(codesign -d --entitlements - "$BIN/kling-vz" 2>&1)
contiene "$ent" "com.apple.security.virtualization" && ok "kling-vz keeps the virtualization entitlement" \
  || bad "kling-vz signature" "com.apple.security.virtualization" "$ent"
prog=$(launchctl print "$DOMINIO/$LABEL" 2>/dev/null | awk '$1=="program" {print $3; exit}')
[ "$prog" = "$BIN/kling" ] && ok "launchd runs $BIN/kling" || bad "launchd program" "$BIN/kling" "$prog"
schema=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1])).get('schema'))" "$ROOT/state.json" 2>&1)
[ "$schema" = 1 ] && ok "state.json migrated to schema 1" || bad "state.json" "schema 1" "$schema"
mismo_estado "$ROOT/state.json.v0.bak" "$BASE/state.before" && ok "state.json.v0.bak is the old state" || bad "v0.bak" "the old state.json" "missing or different"
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
kn rm -f up-from-gold >/dev/null 2>&1
kn stop up-stopped >/dev/null 2>&1
# up-frozen se queda despierta: -rollback tiene que congelarla otra vez.

# ── 5. -rollback a mano ───────────────────────────────────────────────────────
step "5. kling upgrade -rollback"
out=$(kn upgrade -rollback 2>&1); rc=$?
[ $rc = 0 ] && contiene "$out" "rolled back to $OLD_TAG" && ok "rollback" || bad "rollback" "rolled back to $OLD_TAG" "rc=$rc: $out"
contiene "$out" "freezing $FROZEN again" && ok "the machine the new daemon woke was frozen again first" \
  || bad "refreeze before rollback" "freezing $FROZEN again" "$out"
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after -rollback" "$OLD_TAG" "$v"
cmp -s "$BIN/kling-vz" "$OLDVZ" && ok "old kling-vz back in place" || bad "kling-vz" "the old one" "something else"
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
out=$(kn upgrade -from-dir "$NEW_DIR" 2>&1); rc=$?
[ $rc = 0 ] && contiene "$out" "upgraded" && ok "upgraded again" || bad "second upgrade" "upgraded" "rc=$rc: $out"
launchctl bootout "$DOMINIO/$LABEL"
for _ in $(seq 1 20); do kn ps >/dev/null 2>&1 || break; sleep 0.5; done
out=$(kn upgrade -rollback 2>&1); rc=$?
[ $rc = 0 ] && contiene "$out" "no daemon answers" && contiene "$out" "rolled back to $OLD_TAG" \
  && ok "rollback found the backup through the plist" || bad "rollback without a daemon" "rolled back to $OLD_TAG" "rc=$rc: $out"
for _ in $(seq 1 20); do k ps >/dev/null 2>&1 && break; sleep 0.5; done
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after -rollback" "$OLD_TAG" "$v"
st=$(estado "$FROZEN"); [ "$st" = frozen ] && ok "frozen machine still frozen" || bad "frozen" frozen "$st"

echo
echo "upgrade e2e (mac): $pass ok, $fail failed"
[ "$fail" = 0 ]
