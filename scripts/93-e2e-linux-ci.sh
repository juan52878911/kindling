#!/usr/bin/env bash
# e2e con microVMs de verdad en un host Linux de usar y tirar (el job
# e2e-linux de .github/workflows/e2e-linux.yml, o un runner self-hosted del
# laboratorio). Con los binarios y las imágenes ya construidos:
#
#   1. levanta un daemon PRIVADO como unidad transitoria de systemd (kt-ci,
#      raíz $BASE/root, socket /run/kt-ci/kling.sock), con KillMode=process
#      para que reiniciarlo no se lleve las microVMs, como el de verdad;
#   2. construye la imagen toolchain con ese daemon (la que usa 90-e2e.sh);
#   3. scripts/90-e2e.sh contra él (lo opcional se salta solo; sin salida a
#      HTTPBIN, KLING_E2E_OFFLINE=1);
#   4. lo para y corre scripts/e2e-diff-freeze.sh (levanta sus daemons);
#   5. scripts/94-e2e-upgrade.sh: de la release anterior publicada a esta.
#
# Corre como root (sudo) en un host desechable: instala un usuario de build
# y una unidad transitoria. NO en un host con un daemon de kindling que importe
# (no lo toca, pero comparte KVM, disco y memoria).
#
#   sudo NEW_DIR=/ruta/bins IMAGES=/ruta/imagenes ./93-e2e-linux-ci.sh
#
#   NEW_DIR   kling-linux-<arch>, kling-guest-linux-<arch> (y kling-chispa)
#             con su SHA256SUMS; kling tiene que decir una versión de release
#             (v0.21.0-e2e.<sha>) para el modo de descarga de 94
#   IMAGES    vmlinux y min.ext4
#   BASE      raíz de todo (por defecto /srv/kt-ci; mejor en XFS o btrfs, para
#             que el modo almacen de diff-freeze tenga reflink)
#   PARTS     qué correr, de "e2e diff upgrade" (por defecto todo)
#   KLING_E2E_HTTPBIN  el httpbin del proxy de credenciales (httpbin.org)
#
# Sale distinto de cero si falla alguna parte; corre todas aunque una falle.
set -uo pipefail

NEW_DIR="${NEW_DIR:?NEW_DIR: directory with the new binaries}"
IMAGES="${IMAGES:?IMAGES: directory with vmlinux and min.ext4}"
BASE="${BASE:-/srv/kt-ci}"
PARTS="${PARTS:-e2e diff upgrade}"
UNIT=kt-ci
RUN="/run/$UNIT"
SOCK="$RUN/kling.sock"
ROOT="$BASE/root"
LIB="$BASE/lib"
ARCH="$(uname -m)"; case "$ARCH" in x86_64) ARCH=amd64;; aarch64) ARCH=arm64;; esac
HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPTS="$HERE"

die() { echo "$*" >&2; exit 1; }
step() { printf "\n\033[1m== %s\033[0m\n" "$1"; }
[ "$(id -u)" = 0 ] || die "run it as root (sudo)"
[ -e /dev/kvm ] || die "no /dev/kvm: this host cannot run microVMs"
command -v systemd-run >/dev/null || die "systemd-run missing: the private daemon runs as a transient unit"

bin() { local p="$NEW_DIR/$1-linux-$ARCH"; [ -f "$p" ] || p="$NEW_DIR/$1"; echo "$p"; }
KLING_BIN=$(bin kling); GUEST_BIN=$(bin kling-guest)
[ -x "$KLING_BIN" ] || die "no kling in $NEW_DIR"
[ -f "$GUEST_BIN" ] || die "no kling-guest in $NEW_DIR"
for f in vmlinux min.ext4; do [ -f "$IMAGES/$f" ] || die "no $IMAGES/$f"; done
# El que lance el CLI: el usuario de sudo, o root.
SOCKET_USER="${SUDO_USER:-root}"

# Los constructores del daemon no corren como root (internal/daemon,
# builders_sinroot): sin este usuario, `image toolchain` se niega.
id kindling-build >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin kindling-build

failed=""
parte() { echo "$PARTS" | tr ' ' '\n' | grep -qx "$1"; }

arrancar() {
  systemd-run --unit "$UNIT" --collect -p KillMode=process \
    -E "KLING_LIB_DIR=$LIB" -E "KLING_BUILDERS_DIR=$BASE/builders" -E "KLING_SOCKET_USER=$SOCKET_USER" -E "KLING_MIN_FREE_DISK_MIB=1024" \
    "$BASE/bin/kling" daemon -root "$ROOT" -socket "$SOCK" >/dev/null || return 1
  for _ in $(seq 1 60); do "$BASE/bin/kling" ps >/dev/null 2>&1 && return 0; sleep 0.5; done
  return 1
}
parar() {
  systemctl stop "$UNIT" >/dev/null 2>&1
  # KillMode=process deja las microVMs: las que quedaran, fuera.
  pkill -f -- "$ROOT/" >/dev/null 2>&1
  grep -o " ${ROOT}[^ ]*" /proc/mounts | sort -r | while read -r m; do umount -l "$m" 2>/dev/null; done
}
limpiar() {
  parar
  systemctl reset-failed "$UNIT" >/dev/null 2>&1
  [ "${KEEP:-0}" = 1 ] || rm -rf "$BASE" "$RUN"
}
trap limpiar EXIT

export KLING_HOST="unix://$SOCK"
# Ni el contexto ni la configuración de quien corre esto cuentan.
export KLING_CONFIG="$BASE/config.json"

if parte e2e; then
  step "private daemon $UNIT ($ROOT)"
  mkdir -p "$BASE/bin" "$LIB" "$ROOT/images" "$RUN"
  chmod 755 "$BASE" "$ROOT"   # el usuario del VMM tiene que poder atravesarla
  echo '{}' > "$KLING_CONFIG"
  install -m755 "$KLING_BIN" "$BASE/bin/kling"
  install -m755 "$GUEST_BIN" "$LIB/kling-guest"
  # `image toolchain` usa el constructor "base", que corre como root: el
  # daemon solo lo acepta instalado (de root, sin escritura de otros) en
  # KLING_BUILDERS_DIR. El de packaging lanza /usr/local/bin/kling; este, el
  # kling del daemon privado. Su motor (81-base-image.sh) lo busca en
  # KLING_LIB_DIR.
  install -m755 "$SCRIPTS/81-base-image.sh" "$SCRIPTS/lib-ext4-shrink.sh" "$LIB/"
  install -d -m755 "$BASE/builders"
  printf '#!/bin/sh\nexec %s builder base "$@"\n' "$BASE/bin/kling" > "$BASE/builders/base"
  chmod 755 "$BASE/builders/base"
  cp "$IMAGES/vmlinux" "$IMAGES/min.ext4" "$ROOT/images/"
  arrancar || { journalctl -u "$UNIT" --no-pager | tail -30; die "the private daemon does not answer"; }
  out=$("$BASE/bin/kling" status -v 2>&1)
  printf '%s\n' "$out"
  case "$out" in *"$ROOT"*) ;; *) die "status -v does not show $ROOT: refusing to touch another daemon";; esac

  step "toolchain image"
  t0=$(date +%s)
  if "$BASE/bin/kling" image toolchain; then
    echo "toolchain built in $(( $(date +%s) - t0 )) s"
  else
    echo "::warning::kling image toolchain failed; 90-e2e.sh runs what it can with min"
  fi

  step "90-e2e.sh"
  if [ "${KLING_E2E_OFFLINE:-}" = "" ]; then
    if ! curl -fsS --max-time 15 -o /dev/null "https://${KLING_E2E_HTTPBIN:-httpbin.org}/get"; then
      echo "::warning::${KLING_E2E_HTTPBIN:-httpbin.org} does not answer: the credential proxy sections are skipped"
      export KLING_E2E_OFFLINE=1
    fi
  fi
  KLING="$BASE/bin/kling" KLING_E2E_RESTART="systemctl restart $UNIT && sleep 1" \
    bash "$SCRIPTS/90-e2e.sh" || failed="$failed 90-e2e"
  parar
fi

if parte diff; then
  step "e2e-diff-freeze.sh"
  # Sin reflink (ext4) el modo almacen no puede ir: solo off y nojailer.
  modos="almacen off nojailer"
  mkdir -p "$BASE"
  fs=$(stat -f -c %T "$BASE" 2>/dev/null)
  case "$fs" in xfs|btrfs) ;; *) modos="off nojailer"; echo "  ($BASE is $fs, no reflink: skipping the store mode)";; esac
  KLING="$KLING_BIN" GUEST="$GUEST_BIN" VMLINUX="$IMAGES/vmlinux" BASE="$BASE/diff" \
    MODOS="${MODOS:-$modos}" MIN_FREE_MIB="${MIN_FREE_MIB:-4096}" \
    bash "$SCRIPTS/e2e-diff-freeze.sh" || failed="$failed diff-freeze"
fi

if parte upgrade; then
  step "94-e2e-upgrade.sh"
  # Las imágenes para el daemon de 94: las mismas, y toolchain si se hizo.
  imgs="$BASE/images-upgrade"; mkdir -p "$imgs"
  cp "$IMAGES/vmlinux" "$IMAGES/min.ext4" "$imgs/"
  for f in toolchain.layer.ext4 toolchain.recipe.json; do
    [ -f "$ROOT/images/$f" ] && cp "$ROOT/images/$f" "$imgs/"
  done
  NEW_DIR="$NEW_DIR" IMAGES="$imgs" SOCKET_USER="$SOCKET_USER" NAME=kt-ci-upgrade \
    bash "$SCRIPTS/94-e2e-upgrade.sh" || failed="$failed 94-upgrade"
fi

echo
if [ -n "$failed" ]; then
  echo "failed:$failed"
  exit 1
fi
echo "all parts passed: $PARTS"
