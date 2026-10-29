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
#   NEW_DIR=/ruta/head IMAGES_DIR=/ruta/imagenes ./95-e2e-upgrade-mac.sh
#
#   OLD_DIR     kling, kling-vz (firmado) y, si lo trae, kling-hv de la versión
#               anterior; sin él, OLD_TAG se baja con install.sh (necesita red)
#   OLD_TAG     su etiqueta; por defecto la última release publicada anterior
#               a la nueva (la N-1, por el API de GitHub)
#   NEW_DIR     kling, kling-vz y kling-hv de este árbol (nombres de release o
#               a secas); SHA256SUMS si se quiere -from-dir sin -insecure
#   VMM         vz o hv: con qué VMM corre el daemon (KLING_VMM); vacío = el
#               que elija el daemon. Con hv se prueba además que la vuelta
#               atrás mira la versión KLNGHV de lo que congela el nuevo
#   KLING_UPGRADE_REPO  si está, upgrade baja de ahí la release v<nueva>
#               (un fork con la release publicada) en vez de -from-dir; en
#               macOS Go no lee SSL_CERT_FILE, así que no hay servidor local
#   IMAGES_DIR  vmlinux, min.ext4 y, si está, toolchain.layer.ext4 con su receta
#               (los de un daemon Linux arm64: docs/mac.md)
#   BASE        dónde vive todo (por defecto $TMPDIR/kt-e2e-upgrade-mac)
#   KEEP=1      no limpia al terminar
#
# Cada comprobación dice qué esperaba y qué obtuvo; un fallo no aborta el
# resto. Salida en inglés, como el CLI.
set -uo pipefail

OLD_TAG="${OLD_TAG:-}"
OLD_DIR="${OLD_DIR:-}"
VMM="${VMM:-}"
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
# vuelta_atras: upgrade dice "rolled back to <versión>" con la que da el binario
# viejo: "0.17.0" el de la release (install.sh), "v0.17.0" uno sellado a mano.
vuelta_atras() { contiene "$1" "rolled back to $OLD_TAG" || contiene "$1" "rolled back to ${OLD_TAG#v}"; }
die()  { echo "$*" >&2; exit 1; }
now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
# shellcheck source=scripts/lib-e2e-upgrade.sh
. "$(dirname "$0")/lib-e2e-upgrade.sh"

[ "$(uname -s)" = Darwin ] || die "this test is for macOS; on Linux use 94-e2e-upgrade.sh"
[ "$(id -u)" != 0 ] || die "run it as your user, not root: launchd agents live in your gui domain"
command -v python3 >/dev/null || die "python3 missing"
# El agente del usuario no se toca: ni uno cargado ni un plist suyo.
launchctl print "$DOMINIO/$LABEL" >/dev/null 2>&1 && die "$LABEL is loaded in $DOMINIO: this test would replace your daemon's agent; stop it first (launchctl bootout $DOMINIO/$LABEL)"
[ -e "$PLIST" ] && die "$PLIST exists: this test would replace your daemon's agent; move it away first"
[ -e "$BASE" ] && die "$BASE already exists: another run? remove it or set BASE"
[ -e "$SOCK" ] && die "$SOCK already exists: another daemon?"
case "$VMM" in ""|vz|hv) ;; *) die "VMM: vz or hv";; esac

pieza() { local p="$1/$2-darwin-arm64"; [ -x "$p" ] || p="$1/$2"; [ -x "$p" ] || die "no $2 in $1"; echo "$p"; }
# kling-hv es opcional (una release anterior a v0.19 no lo trae): vacío si no está.
pieza_opc() { local p="$1/$2-darwin-arm64"; [ -x "$p" ] || p="$1/$2"; [ -x "$p" ] && echo "$p"; return 0; }
NEWK=$(pieza "$NEW_DIR" kling); NEWVZ=$(pieza "$NEW_DIR" kling-vz); NEWHV=$(pieza_opc "$NEW_DIR" kling-hv)
[ "$VMM" = hv ] && [ -z "$NEWHV" ] && die "VMM=hv and no kling-hv in $NEW_DIR"

# Los dos CLI contra el socket privado, con una configuración vacía: ni el
# contexto ni los valores por defecto del usuario cuentan.
export KLING_HOST="unix://$SOCK"
export KLING_CONFIG="$BASE/config.json"
OLDK="$BASE/kling.old-cli"   # copia aparte: upgrade cambia $BIN/kling
k()  { "$OLDK" "$@"; }
kn() { "$NEWK" "$@"; }
NEWV=$(kn version 2>/dev/null | awk 'NR==1{print $2}')
case "$NEWV" in ""|dev) die "the new kling says version \"$NEWV\": build NEW_DIR with -ldflags \"-X main.Version=\$(git describe --tags)\"";; esac
if [ -z "$OLD_TAG" ]; then
  OLD_TAG=$(ultima_release_anterior "$NEWV")
  [ -n "$OLD_TAG" ] || die "cannot tell the release before $NEWV (GitHub API): set OLD_TAG"
fi
case "$NEWV" in "${OLD_TAG}"|"${OLD_TAG#v}") die "the new kling says version \"$NEWV\", the same as OLD_TAG";; esac
NEWTAG="v${NEWV#v}"
NEW_SCHEMAS=$(kn upgrade -schemas 2>/dev/null) || die "the new kling does not answer upgrade -schemas"
NEW_STATE=$(esquema_soportado "$NEWK" state)
# De dónde saca upgrade los nuevos: la release de KLING_UPGRADE_REPO o NEW_DIR.
if [ -n "${KLING_UPGRADE_REPO:-}" ]; then
  etiqueta_valida "$NEWTAG" || die "KLING_UPGRADE_REPO needs the new kling to be a release tag, and it says \"$NEWV\""
  FROM="release $NEWTAG of $KLING_UPGRADE_REPO"; NUEVO=(-tag "$NEWTAG")
elif [ -f "$NEW_DIR/SHA256SUMS" ]; then
  FROM="$NEW_DIR"; NUEVO=(-from-dir "$NEW_DIR")
else
  FROM="$NEW_DIR (unverified)"; NUEVO=(-from-dir "$NEW_DIR" -insecure)
fi

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

# upgrade_hv_version MEM: la versión KLNGHV del pie de un mem.file de kling-hv
# (vacío si no lo es), como internal/upgrade.VersionVolcadoHV.
upgrade_hv_version() {
  python3 - "$1" <<'PY' 2>/dev/null
import struct, sys
MAGIA = 0x4b4c4e4748563032
with open(sys.argv[1], "rb") as f:
    f.seek(-32, 2)
    magia, _, off, _ = struct.unpack("<QQQQ", f.read(32))
    if magia != MAGIA:
        sys.exit(0)
    f.seek(off)
    m, v = struct.unpack("<QI", f.read(12))
    if m == MAGIA:
        print(v)
PY
}
# hv_como_antes: tras una vuelta atrás, kling-hv es el de antes (o no está, si
# la versión anterior no lo traía).
hv_como_antes() {
  [ -n "$NEWHV" ] || return 0
  if [ -n "$OLDHV" ]; then
    cmp -s "$BIN/kling-hv" "$OLDHV" && ok "old kling-hv back in place ($1)" || bad "kling-hv $1" "the old one" "something else"
  else
    [ ! -e "$BIN/kling-hv" ] && ok "kling-hv removed: $OLD_TAG had none ($1)" || bad "kling-hv $1" "gone" "still there"
  fi
}

# ── 0. daemon privado con la versión anterior, por launchd ───────────────────
step "0. Private $OLD_TAG daemon ($LABEL in $DOMINIO, $ROOT)"
mkdir -p "$BIN" "$ROOT/images"
echo '{}' > "$KLING_CONFIG"
if [ -z "$OLD_DIR" ]; then
  OLD_DIR="$BASE/old"; mkdir -p "$OLD_DIR"
  curl -fsSL "https://raw.githubusercontent.com/juan52878911/kindling/$OLD_TAG/scripts/install.sh" \
    | sh -s -- --tag "$OLD_TAG" --prefix "$OLD_DIR" --no-rc --force >/dev/null || die "install.sh --tag $OLD_TAG failed"
fi
OLDK0=$(pieza "$OLD_DIR" kling); OLDVZ=$(pieza "$OLD_DIR" kling-vz); OLDHV=$(pieza_opc "$OLD_DIR" kling-hv)
[ "$VMM" = hv ] && [ -z "$OLDHV" ] && die "VMM=hv and $OLD_TAG has no kling-hv"
install -m755 "$OLDK0" "$BIN/kling"
install -m755 "$OLDK0" "$OLDK"
cp "$OLDVZ" "$BIN/kling-vz"   # cp conserva la firma con su entitlement
[ -n "$OLDHV" ] && cp "$OLDHV" "$BIN/kling-hv"
# KLING_VMM solo si se pidió uno: sin él, el daemon elige como siempre.
VMM_ENV=""
[ -n "$VMM" ] && VMM_ENV="    <key>KLING_VMM</key>
    <string>$VMM</string>
"
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
$VMM_ENV  </dict>
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
ok "daemon $OLD_TAG answers, run by launchd (vmm ${VMM:-auto})"

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
# El esquema que escribe la anterior: 0 hasta v0.17, 1 o 2 desde v0.18.
OLD_STATE=$(schema_de "$ROOT/state.json")
case "$OLD_STATE" in
  [0-9]*) [ "$OLD_STATE" -le "$NEW_STATE" ] && ok "state.json written by $OLD_TAG (schema $OLD_STATE; the new one reads up to $NEW_STATE)" \
            || bad "old state.json" "a schema the new kling reads (<= $NEW_STATE)" "$OLD_STATE" ;;
  *) bad "old state.json" "a schema" "unreadable" ;;
esac
if [ "$VMM" = hv ]; then
  v=$(upgrade_hv_version "$ROOT/machines/$FROZEN/mem.file")
  [ -n "$v" ] && ok "up-frozen was frozen by kling-hv (KLNGHV state $v)" || bad "kling-hv freeze" "a KLNGHV mem.file" "none"
fi

# ── 2. upgrade fallido: el nuevo no arranca y se vuelve solo ─────────────────
step "2. A new daemon that does not start is rolled back"
ROTO="$BASE/roto"; mkdir -p "$ROTO"
# Dice ser la versión nueva y entender el estado, pero no arranca como daemon.
cat > "$ROTO/kling" <<EOF
#!/bin/sh
case "\$1" in
  upgrade) echo '$NEW_SCHEMAS' ;;
  version) echo "kling $NEWV" ;;
  *) echo "broken on purpose" >&2; exit 1 ;;
esac
EOF
chmod +x "$ROTO/kling"
cp "$NEWVZ" "$ROTO/kling-vz"   # solo falla el daemon
[ -n "$NEWHV" ] && cp "$NEWHV" "$ROTO/kling-hv"
out=$(kn upgrade -from-dir "$ROTO" -insecure -timeout 15s 2>&1); rc=$?
[ $rc != 0 ] && contiene "$out" "rolled back" && ok "upgrade refused and rolled back" \
  || bad "broken upgrade" "an error and a rollback" "rc=$rc: $out"
for _ in $(seq 1 20); do k ps >/dev/null 2>&1 && break; sleep 0.5; done
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after rollback" "$OLD_TAG" "$v"
cmp -s "$BIN/kling" "$OLDK" && ok "old kling back in place" || bad "kling" "the old one" "something else"
cmp -s "$BIN/kling-vz" "$OLDVZ" && ok "old kling-vz back in place" || bad "kling-vz" "the old one" "something else"
hv_como_antes "after the automatic rollback"
st=$(estado "$FROZEN"); [ "$st" = frozen ] && ok "frozen machine still frozen" || bad "frozen after rollback" frozen "$st"

# ── 3. upgrade de verdad ─────────────────────────────────────────────────────
step "3. Upgrade $OLD_TAG -> $NEWV"
out=$(kn upgrade "${NUEVO[@]}" -dry-run 2>&1)
contiene "$out" "plan: " && cmp -s "$BIN/kling" "$OLDK" \
  && ok "dry run shows the plan and changes nothing" || bad "dry run" "the plan, nothing changed" "$out"
PLAN_MIGRA=no; contiene "$out" "state.json: schema $OLD_STATE ->" && PLAN_MIGRA=yes
[ "$OLD_STATE" = 0 ] && [ "$PLAN_MIGRA" = no ] && bad "dry run" "state.json: schema 0 -> 1" "$out"
# Lo de justo antes: el viejo reescribe su state.json al pararse y arrancar.
cp "$ROOT/state.json" "$BASE/state.before"
t0=$(now_ms)
out=$(kn upgrade "${NUEVO[@]}" 2>&1); rc=$?
ms=$(( $(now_ms) - t0 ))
[ $rc = 0 ] && contiene "$out" "upgraded" && ok "kling upgrade from $FROM in ${ms} ms" || bad "upgrade" "upgraded" "rc=$rc: $out"
v=$(kn version 2>&1 | sed -n 2p)
contiene "$v" "${NEWV#v}" && ok "daemon answers as $NEWV" || bad "new daemon" "$NEWV" "$v"
cmp -s "$BIN/kling" "$NEWK" && ok "$BIN/kling replaced" || bad "kling" "the new one" "something else"
cmp -s "$BIN/kling-vz" "$NEWVZ" && ok "$BIN/kling-vz replaced" || bad "kling-vz" "the new one" "something else"
ent=$(codesign -d --entitlements - "$BIN/kling-vz" 2>&1)
contiene "$ent" "com.apple.security.virtualization" && ok "kling-vz keeps the virtualization entitlement" \
  || bad "kling-vz signature" "com.apple.security.virtualization" "$ent"
if [ -n "$NEWHV" ]; then
  cmp -s "$BIN/kling-hv" "$NEWHV" && ok "$BIN/kling-hv replaced" || bad "kling-hv" "the new one" "something else"
  ent=$(codesign -d --entitlements - "$BIN/kling-hv" 2>&1)
  contiene "$ent" "com.apple.security.hypervisor" && ok "kling-hv keeps the hypervisor entitlement" \
    || bad "kling-hv signature" "com.apple.security.hypervisor" "$ent"
fi
prog=$(launchctl print "$DOMINIO/$LABEL" 2>/dev/null | awk '$1=="program" {print $3; exit}')
[ "$prog" = "$BIN/kling" ] && ok "launchd runs $BIN/kling" || bad "launchd program" "$BIN/kling" "$prog"
schema=$(schema_de "$ROOT/state.json")
BAK="$ROOT/state.json.v$OLD_STATE.bak"
MIGRO=no; [ -f "$BAK" ] && MIGRO=yes
case "$schema" in
  [0-9]*) [ "$schema" -ge "$OLD_STATE" ] && [ "$schema" -le "$NEW_STATE" ] && ok "state.json at schema $schema (was $OLD_STATE)" \
            || bad "state.json" "a schema between $OLD_STATE and $NEW_STATE" "$schema" ;;
  *) bad "state.json" "a schema" "unreadable" ;;
esac
[ "$MIGRO" = "$PLAN_MIGRA" ] && ok "the plan said what happened (state.json migration: $MIGRO)" \
  || bad "migration" "as in the dry run ($PLAN_MIGRA)" "$MIGRO"
if [ "$MIGRO" = yes ]; then
  mismo_estado "$BAK" "$BASE/state.before" && ok "$(basename "$BAK") is the old state" || bad "$(basename "$BAK")" "the old state.json" "different"
fi
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
[ $rc = 0 ] && vuelta_atras "$out" && ok "rollback" || bad "rollback" "rolled back to $OLD_TAG" "rc=$rc: $out"
contiene "$out" "freezing $FROZEN again" && ok "the machine the new daemon woke was frozen again first" \
  || bad "refreeze before rollback" "freezing $FROZEN again" "$out"
if [ "$VMM" = hv ]; then
  # Lo congeló el kling-hv nuevo; upgrade comprobó que el viejo lee su KLNGHV.
  v=$(upgrade_hv_version "$ROOT/machines/$FROZEN/mem.file")
  [ -n "$v" ] && ok "refrozen by the new kling-hv (KLNGHV state $v), readable by the old one" || bad "kling-hv refreeze" "a KLNGHV mem.file" "none"
fi
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after -rollback" "$OLD_TAG" "$v"
cmp -s "$BIN/kling-vz" "$OLDVZ" && ok "old kling-vz back in place" || bad "kling-vz" "the old one" "something else"
hv_como_antes "after -rollback"
schema=$(schema_de "$ROOT/state.json")
[ "$schema" = "$OLD_STATE" ] && ok "state.json back to schema $OLD_STATE" || bad "state.json after -rollback" "schema $OLD_STATE" "$schema"
[ "$MIGRO" = no ] || { [ ! -e "$BAK" ] && ok "migration copy consumed" || bad "$(basename "$BAK")" "gone" "still there"; }
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
out=$(kn upgrade "${NUEVO[@]}" 2>&1); rc=$?
[ $rc = 0 ] && contiene "$out" "upgraded" && ok "upgraded again" || bad "second upgrade" "upgraded" "rc=$rc: $out"
launchctl bootout "$DOMINIO/$LABEL"
for _ in $(seq 1 20); do kn ps >/dev/null 2>&1 || break; sleep 0.5; done
out=$(kn upgrade -rollback 2>&1); rc=$?
[ $rc = 0 ] && contiene "$out" "no daemon answers" && vuelta_atras "$out" \
  && ok "rollback found the backup through the plist" || bad "rollback without a daemon" "rolled back to $OLD_TAG" "rc=$rc: $out"
for _ in $(seq 1 20); do k ps >/dev/null 2>&1 && break; sleep 0.5; done
v=$(k version 2>&1 | sed -n 2p)
contiene "$v" "${OLD_TAG#v}" && ok "$OLD_TAG answers again" || bad "after -rollback" "$OLD_TAG" "$v"
st=$(estado "$FROZEN"); [ "$st" = frozen ] && ok "frozen machine still frozen" || bad "frozen" frozen "$st"

echo
echo "upgrade e2e (mac): $pass ok, $fail failed"
[ "$fail" = 0 ]
