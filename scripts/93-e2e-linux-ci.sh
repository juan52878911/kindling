#!/usr/bin/env bash
# e2e con microVMs de verdad en un host Linux de usar y tirar (el job
# e2e-linux de .github/workflows/e2e-linux.yml, o un runner self-hosted del
# laboratorio). Con los binarios y las imágenes ya construidos:
#
#   1. levanta un daemon PRIVADO como unidad transitoria de systemd (kt-ci,
#      raíz $BASE/root, socket /run/kt-ci/kling.sock), con KillMode=process
#      para que reiniciarlo no se lleve las microVMs, como el de verdad;
#   2. construye la imagen toolchain con ese daemon (la que usa 90-e2e.sh);
#   2b. parte db: la imagen pg16 y la plantilla Postgres (scripts/db-golden.sh)
#      con ese daemon, y la extensión kling-db de DB_BIN;
#   3. scripts/90-e2e.sh contra él (lo opcional se salta solo; sin salida a
#      HTTPBIN, KLING_E2E_OFFLINE=1). Lo que este script prepara NO es
#      opcional: si 90 se salta las carpetas vivas (daemon.share_roots va en
#      la configuración del daemon) o, con la parte db, la sección de kling
#      db, la parte falla aunque 90 salga con 0. Lo que pide algo que este
#      host no tiene sí se salta, y cada salto sale como aviso y en el
#      resumen del job: el proxy de Postgres (KLING_E2E_PG_URL), el clon de
#      un Postgres de origen (KLING_E2E_CLONE_ADMIN_URL), MariaDB
#      (KLING_E2E_MYSQL_GOLDEN), Redis y SQLite (KLING_E2E_*_GOLDEN), la
#      autorización por inquilino (KLING_E2E_AUTHZ y sus tokens), `ask` con
#      un proveedor real y, sin salida a HTTPBIN, el proxy de credenciales;
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
#   DB_BIN    kling-db (linux, de este commit) para la parte db
#   BASE      raíz de todo (por defecto /srv/kt-ci; mejor en XFS o btrfs, para
#             que el modo almacen de diff-freeze tenga reflink)
#   PARTS     qué correr, de "e2e db diff upgrade" (por defecto todo; db va
#             con e2e: la sección 7e de 90 es la que la prueba)
#   KLING_E2E_HTTPBIN  el httpbin del proxy de credenciales (httpbin.org)
#
# Sale distinto de cero si falla alguna parte; corre todas aunque una falle.
set -uo pipefail

NEW_DIR="${NEW_DIR:?NEW_DIR: directory with the new binaries}"
IMAGES="${IMAGES:?IMAGES: directory with vmlinux and min.ext4}"
BASE="${BASE:-/srv/kt-ci}"
PARTS="${PARTS:-e2e db diff upgrade}"
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
# El usuario sin privilegios del jailer (lo que pide `kling up`): sin él, con
# el jailer instalado, el daemon se niega a arrancar máquinas (internal/machine,
# decidirJailer) en vez de correr Firecracker como root.
if ! id kindling >/dev/null 2>&1; then
  getent group kvm >/dev/null || groupadd --system kvm
  useradd --system --no-create-home --shell /usr/sbin/nologin -G kvm kindling
fi
id -nG kindling | tr ' ' '\n' | grep -qx kvm || usermod -aG kvm kindling

failed=""
parte() { echo "$PARTS" | tr ' ' '\n' | grep -qx "$1"; }
for p in $PARTS; do case "$p" in e2e|db|diff|upgrade) ;; *) die "PARTS: e2e, db, diff and/or upgrade, not \"$p\"";; esac; done
if parte db; then
  parte e2e || die "PARTS: db needs e2e (section 7e of 90-e2e.sh is what tests it)"
  DB_BIN="${DB_BIN:?DB_BIN: kling-db for the db part}"
  [ -f "$DB_BIN" ] || die "no kling-db at $DB_BIN"
fi
# La plantilla Postgres de la parte db, y dónde deja db-golden.sh su estado
# (el $HOME del que corre esto, el mismo que lee 90-e2e.sh).
DBG=kt-ci-pg
DBSTATE="$HOME/.local/state/kling-db/$DBG"
# Las carpetas vivas de 90 (sección 6): bajo daemon.share_roots o se saltan.
export SHARE_ROOT="$BASE/shares"

arrancar() {
  systemd-run --unit "$UNIT" --collect -p KillMode=process -E "KLING_CONFIG=$KLING_CONFIG" \
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
  [ "${KEEP:-0}" = 1 ] || rm -rf "$BASE" "$RUN" "$DBSTATE"
}
trap limpiar EXIT

export KLING_HOST="unix://$SOCK"
# Ni el contexto ni la configuración de quien corre esto cuentan.
export KLING_CONFIG="$BASE/config.json"

if parte e2e; then
  step "private daemon $UNIT ($ROOT)"
  mkdir -p "$BASE/bin" "$LIB" "$ROOT/images" "$RUN" "$SHARE_ROOT"
  chmod 755 "$BASE" "$ROOT"   # el usuario del VMM tiene que poder atravesarla
  # La del daemon y la del CLI son la misma (arrancar le pasa KLING_CONFIG):
  # con '{}' no había share_roots y 90 se saltaba las carpetas vivas en verde.
  printf '{"daemon":{"share_roots":["%s"]}}\n' "$SHARE_ROOT" > "$KLING_CONFIG"
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

  # Docker (lo traen los runners de GitHub) deja la política del FORWARD del
  # host en DROP, y kindling solo añade DROPs a esa cadena (HostEgressRules):
  # con DROP, egress internet y allowlist no salen. Este host es desechable.
  if iptables -S FORWARD 2>/dev/null | grep -qx -- '-P FORWARD DROP'; then
    echo "::warning::the host FORWARD policy is DROP (Docker?): setting it to ACCEPT on this disposable host"
    iptables -P FORWARD ACCEPT
  fi

  step "toolchain image"
  t0=$(date +%s)
  if "$BASE/bin/kling" image toolchain; then
    echo "toolchain built in $(( $(date +%s) - t0 )) s"
  else
    echo "::warning::kling image toolchain failed; 90-e2e.sh runs what it can with min"
  fi

  if parte db; then
    step "kling db: pg16 image and the $DBG golden (db-golden.sh)"
    # La extensión, de root y sin escritura de otros (pkg/plugin la exige así).
    install -d -m755 "$BASE/plugins"
    install -m755 "$DB_BIN" "$BASE/plugins/kling-db"
    export KLING_PLUGIN_PATH="$BASE/plugins"
    t0=$(date +%s)
    if KLING="$BASE/bin/kling" bash "$SCRIPTS/db-golden.sh" image \
       && KLING="$BASE/bin/kling" bash "$SCRIPTS/db-golden.sh" build "$DBG"; then
      echo "pg16 and $DBG built in $(( $(date +%s) - t0 )) s"
      export KLING_E2E_DB_GOLDEN="$DBG"
    else
      # Sin plantilla 90 se saltaría 7e y saldría verde: falla aquí, y 90
      # corre igual para que se vea el resto.
      echo "::error::the $DBG golden could not be built: the db part fails"
      failed="$failed db-golden"
    fi
  fi

  step "90-e2e.sh"
  if [ "${KLING_E2E_OFFLINE:-}" = "" ]; then
    if ! curl -fsS --max-time 15 -o /dev/null "https://${KLING_E2E_HTTPBIN:-httpbin.org}/get"; then
      echo "::warning::${KLING_E2E_HTTPBIN:-httpbin.org} does not answer: the credential proxy sections are skipped"
      export KLING_E2E_OFFLINE=1
    fi
  fi
  log90="$BASE/90-e2e.log"
  KLING="$BASE/bin/kling" KLING_E2E_RESTART="systemctl restart $UNIT && sleep 1" \
    bash "$SCRIPTS/90-e2e.sh" 2>&1 | tee "$log90"
  [ "${PIPESTATUS[0]}" = 0 ] || failed="$failed 90-e2e"
  # Lo que 90 se salta diciendo «skip» y este host sí tiene preparado: aquí
  # saltarlo es un fallo (obligatorio <parte> <texto de 90 al saltarlo>).
  obligatorio() {
    if grep -qF -- "$2" "$log90"; then
      echo "::error::90-e2e.sh skipped what this host set up ($1): $2"
      failed="$failed $1-skipped"
    fi
  }
  obligatorio shares "live shares skipped"
  if parte db && [ -n "${KLING_E2E_DB_GOLDEN:-}" ]; then
    obligatorio db "KLING_E2E_DB_GOLDEN no está"
    obligatorio db "el plugin kling-db no está instalado"
  fi
  # El resto de lo que 90 se salta lo pide algo que este host no prepara (ver
  # la cabecera): no es un fallo, pero tampoco se ha probado. Se cuenta y se
  # lista en el resumen del job para que el verde no lo esconda.
  mapfile -t saltos < <(sed 's/\x1b\[[0-9;]*m//g' "$log90" | sed -n 's/^ *skip  *//p')
  if [ "${#saltos[@]}" -gt 0 ]; then
    echo "::warning::90-e2e.sh skipped ${#saltos[@]} section(s) this host does not set up (listed in the job summary)"
    if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
      {
        echo "### 90-e2e.sh: ${#saltos[@]} skipped section(s)"
        echo
        printf -- '- %s\n' "${saltos[@]}"
      } >> "$GITHUB_STEP_SUMMARY"
    fi
  fi
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
