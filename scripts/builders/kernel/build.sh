#!/usr/bin/env bash
# Construye el kernel de invitado de kindling (6.1 LTS) de forma reproducible.
#
#   scripts/builders/kernel/build.sh                    # arquitectura del anfitrión
#   TARGET_ARCH=arm64 scripts/builders/kernel/build.sh  # cruzado (aarch64-linux-gnu-)
#   OUT=/opt/fc scripts/builders/kernel/build.sh        # dónde dejar el resultado
#   CONFIG_ONLY=1 scripts/builders/kernel/build.sh      # solo .config + comprobación
#   scripts/builders/kernel/build.sh --print-name       # nombre del fichero y nada más
#
# No es un constructor del daemon (/usr/local/lib/kindling/builders/): corre en
# la máquina de build o en el laboratorio, no hace falta root.
#
# Pasos:
#   1. descarga linux-$KERNEL_VERSION.tar.xz de kernel.org (versión y sha256
#      fijados en kernel.pin) a una cache y comprueba el sha256;
#   2. lo extrae limpio en un directorio temporal;
#   3. aplica config-common + config-$TARGET_ARCH con `allnoconfig` y después
#      `olddefconfig`; avisa de cada opción pedida que no llegó al .config
#      (dependencia no satisfecha o nombre que no existe en esta versión);
#   4. pasa scripts/check-kernel-config.sh: si falla, no se compila;
#   5. compila vmlinux (amd64, el ELF que carga Firecracker) o Image (arm64,
#      que es lo que cargan Firecracker y Virtualization.framework).
#
# Deja en $OUT (por defecto ./out):
#   vmlinux-<versión>-kindling-<arch>             el kernel
#   vmlinux-<versión>-kindling-<arch>.config      el .config final
#   vmlinux-<versión>-kindling-<arch>.sha256
#   vmlinux-<versión>-kindling-<arch>.buildinfo   toolchain, hashes de entrada
#
# REPRODUCIBLE: fecha, usuario, host y número de build fijos (KBUILD_BUILD_*),
# SOURCE_DATE_EPOCH sacado del propio tarball, sin información de depuración ni
# versión local automática. Con el mismo compilador sale el mismo binario; el
# .buildinfo apunta cuál se usó.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
CHECK="$REPO/scripts/check-kernel-config.sh"

# ── versión fijada ───────────────────────────────────────────────────────────
# kernel.pin se lee, no se ejecuta. KERNEL_VERSION/KERNEL_SHA256 del entorno
# mandan sobre él; si se cambia la versión, el sha256 del pin ya no vale.
pin() { sed -n "s/^$1=\([^ ]*\)\$/\1/p" "$HERE/kernel.pin" | tail -1; }
if [ -n "${KERNEL_VERSION:-}" ]; then
  KERNEL_SHA256="${KERNEL_SHA256:-}"
else
  KERNEL_VERSION="$(pin KERNEL_VERSION)"
  KERNEL_SHA256="${KERNEL_SHA256:-$(pin KERNEL_SHA256)}"
fi
[[ "$KERNEL_VERSION" =~ ^[0-9]+\.[0-9]+(\.[0-9]+)?$ ]] || {
  echo "invalid KERNEL_VERSION '$KERNEL_VERSION' (scripts/builders/kernel/kernel.pin)" >&2; exit 1; }

# ── arquitectura ─────────────────────────────────────────────────────────────
host_arch() {
  case "$(uname -m)" in
    x86_64|amd64) echo amd64 ;;
    aarch64|arm64) echo arm64 ;;
    *) uname -m ;;
  esac
}
HOST_ARCH="$(host_arch)"
TARGET_ARCH="${TARGET_ARCH:-$HOST_ARCH}"
case "$TARGET_ARCH" in
  amd64) KARCH=x86_64; TARGET=vmlinux;               IMG=vmlinux;                 CROSS_DEF=x86_64-linux-gnu- ;;
  arm64) KARCH=arm64;  TARGET=Image;                 IMG=arch/arm64/boot/Image;   CROSS_DEF=aarch64-linux-gnu- ;;
  *) echo "unsupported TARGET_ARCH '$TARGET_ARCH' (amd64|arm64)" >&2; exit 1 ;;
esac
if [ "$TARGET_ARCH" = "$HOST_ARCH" ]; then
  CROSS_COMPILE="${CROSS_COMPILE:-}"
else
  CROSS_COMPILE="${CROSS_COMPILE:-$CROSS_DEF}"
fi

NAME="vmlinux-${KERNEL_VERSION}-kindling-${TARGET_ARCH}"
if [ "${1:-}" = "--print-name" ]; then
  echo "$NAME"
  exit 0
fi
[ $# -eq 0 ] || { echo "uso: $0 [--print-name]" >&2; exit 2; }

OUT="${OUT:-$PWD/out}"
CACHE="${CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/kindling/kernel}"
MIRROR="${KERNEL_MIRROR:-https://cdn.kernel.org/pub/linux/kernel}"
JOBS="${JOBS:-$(nproc)}"
CONFIG_ONLY="${CONFIG_ONLY:-0}"
STRICT_FRAGMENT="${STRICT_FRAGMENT:-0}"

# ── herramientas ─────────────────────────────────────────────────────────────
falta=()
for c in curl tar xz sha256sum make gcc flex bison bc perl; do
  command -v "$c" >/dev/null || falta+=("$c")
done
if [ "$CONFIG_ONLY" != 1 ]; then
  command -v "${CROSS_COMPILE}gcc" >/dev/null || falta+=("${CROSS_COMPILE}gcc")
  # objtool (amd64) y resolve_btfids enlazan contra libelf.
  echo '#include <libelf.h>' | gcc -E -x c - >/dev/null 2>&1 || falta+=("libelf-dev")
fi
if [ ${#falta[@]} -gt 0 ]; then
  cruzado=""
  [ -z "$CROSS_COMPILE" ] || cruzado=" gcc-${CROSS_COMPILE%-}"
  echo "missing: ${falta[*]}" >&2
  echo "  apt install build-essential flex bison bc libelf-dev xz-utils curl${cruzado}" >&2
  exit 1
fi

# ── 1. descarga y verificación ───────────────────────────────────────────────
TARBALL="linux-${KERNEL_VERSION}.tar.xz"
URL="$MIRROR/v${KERNEL_VERSION%%.*}.x/$TARBALL"
mkdir -p "$CACHE" "$OUT"
if [ ! -f "$CACHE/$TARBALL" ]; then
  echo "descargando $URL"
  curl -fL --retry 3 -o "$CACHE/$TARBALL.part" "$URL"
  mv "$CACHE/$TARBALL.part" "$CACHE/$TARBALL"
fi
got="$(sha256sum "$CACHE/$TARBALL" | cut -d' ' -f1)"
if [ -z "$KERNEL_SHA256" ]; then
  cat >&2 <<EOF
KERNEL_SHA256 is not pinned for $TARBALL.
  computed: $got
Check it against the signed list and then write it to
scripts/builders/kernel/kernel.pin (or pass KERNEL_SHA256=... for this run):
  curl -fsSL $MIRROR/v${KERNEL_VERSION%%.*}.x/sha256sums.asc | gpg --verify
  curl -fsSL $MIRROR/v${KERNEL_VERSION%%.*}.x/sha256sums.asc | grep ' $TARBALL\$'
EOF
  exit 1
fi
if [ "$got" != "$KERNEL_SHA256" ]; then
  echo "sha256 mismatch for $CACHE/$TARBALL: expected $KERNEL_SHA256, got $got" >&2
  echo "  (deleting it; run again to re-download)" >&2
  rm -f "$CACHE/$TARBALL"
  exit 1
fi
echo "  $TARBALL ok ($got)"

# ── 2. árbol limpio ──────────────────────────────────────────────────────────
WORK="$(mktemp -d "${TMPDIR:-/tmp}/kindling-kernel.XXXXXX")"
if [ "${KEEP_WORK:-0}" = 1 ]; then
  echo "  árbol de trabajo: $WORK (KEEP_WORK=1, no se borra)"
else
  trap 'rm -rf "$WORK"' EXIT
fi
tar -xJf "$CACHE/$TARBALL" -C "$WORK"
SRC="$WORK/linux-${KERNEL_VERSION}"

# Reproducibilidad: la fecha sale del tarball, no del reloj.
SOURCE_DATE_EPOCH="$(stat -c %Y "$SRC/Makefile")"
export SOURCE_DATE_EPOCH
export KBUILD_BUILD_TIMESTAMP
KBUILD_BUILD_TIMESTAMP="$(LC_ALL=C date -u -d "@$SOURCE_DATE_EPOCH" '+%a %b %e %H:%M:%S UTC %Y')"
export KBUILD_BUILD_USER=kindling KBUILD_BUILD_HOST=kindling KBUILD_BUILD_VERSION=1
export LC_ALL=C TZ=UTC
# Un ARCH o LOCALVERSION del entorno se colaría en el build.
unset ARCH LOCALVERSION KCONFIG_CONFIG

kmake() { make -C "$SRC" ARCH="$KARCH" CROSS_COMPILE="$CROSS_COMPILE" "$@"; }

# ── 3. configuración ─────────────────────────────────────────────────────────
FRAG="$WORK/fragment.config"
cat "$HERE/config-common" "$HERE/config-$TARGET_ARCH" >"$FRAG"
kmake -s KCONFIG_ALLCONFIG="$FRAG" allnoconfig
kmake -s olddefconfig

# Deriva: lo pedido que no llegó. La última línea de cada opción manda, como
# hace Kconfig (config-$arch va detrás de config-common).
declare -A PEDIDO=() FINAL=()
while IFS= read -r l; do
  case "$l" in
    CONFIG_*=*)                  PEDIDO["${l%%=*}"]="${l#*=}" ;;
    "# CONFIG_"*" is not set")   k="${l#\# }"; PEDIDO["${k%% *}"]=n ;;
  esac
done <"$FRAG"
while IFS= read -r l; do
  case "$l" in CONFIG_*=*) FINAL["${l%%=*}"]="${l#*=}" ;; esac
done <"$SRC/.config"
deriva=0
for k in $(printf '%s\n' "${!PEDIDO[@]}" | sort); do
  want="${PEDIDO[$k]}"; have="${FINAL[$k]:-n}"
  if [ "$want" != "$have" ]; then
    [ "$deriva" -gt 0 ] || echo "fragmento: opciones que no quedaron como se pidieron:"
    echo "  $k: requested $want, got $have"
    deriva=$((deriva + 1))
  fi
done
if [ "$deriva" -gt 0 ] && [ "$STRICT_FRAGMENT" = 1 ]; then
  echo "$deriva fragment option(s) did not apply (STRICT_FRAGMENT=1)" >&2
  exit 1
fi

# ── 4. comprobación ──────────────────────────────────────────────────────────
cp "$SRC/.config" "$OUT/$NAME.config"
"$CHECK" --arch "$TARGET_ARCH" "$OUT/$NAME.config"
if [ "$CONFIG_ONLY" = 1 ]; then
  echo "listo (solo configuración): $OUT/$NAME.config"
  exit 0
fi

# ── 5. compilación ───────────────────────────────────────────────────────────
t0="$(date +%s)"
kmake -j"$JOBS" "$TARGET"
cp "$SRC/$IMG" "$OUT/$NAME"
(cd "$OUT" && sha256sum "$NAME" >"$NAME.sha256")
{
  echo "kernel_version=$KERNEL_VERSION"
  echo "tarball_sha256=$KERNEL_SHA256"
  echo "target_arch=$TARGET_ARCH"
  echo "fragment_sha256=$(sha256sum "$FRAG" | cut -d' ' -f1)"
  echo "source_date_epoch=$SOURCE_DATE_EPOCH"
  echo "compiler=$("${CROSS_COMPILE}gcc" --version | head -1)"
  echo "binutils=$("${CROSS_COMPILE}ld" --version | head -1)"
  echo "kernel_sha256=$(cut -d' ' -f1 "$OUT/$NAME.sha256")"
} >"$OUT/$NAME.buildinfo"
echo
echo "listo en $(( $(date +%s) - t0 )) s: $OUT/$NAME"
ls -lh "$OUT/$NAME" | awk '{print "  " $5, $9}'
