#!/usr/bin/env bash
# Construye una imagen base mínima sobre Debian (glibc), hermana de
# 70-build-minimal-image.sh.
#
#   sudo ./71-build-glibc-base.sh                  # base "min-glibc"
#   sudo ./71-build-glibc-base.sh node-glibc       # + nodejs
#   sudo ./71-build-glibc-base.sh chrome           # + nodejs + chrome-headless-shell (solo x86_64)
#
# POR QUÉ EXISTE, SI YA HAY UNA BASE.
#
# Alpine usa musl, y `chrome-headless-shell` —el Chromium sin capa de interfaz,
# el que Google publica para automatización— está enlazado contra glibc. No es
# cuestión de instalar un paquete: con gcompat le faltan 52 bibliotecas. Medido.
#
# Y la diferencia justifica la base entera: contra el Chromium de Alpine, el
# headless-shell midió 637 MB frente a 986,8 MB y 116 ms de arranque frente a
# 401 ms, extrayendo EXACTAMENTE los mismos caracteres de las mismas páginas.
#
# Alpine sigue siendo la base por defecto para todo lo demás: para un servidor
# MCP de node sin navegador, musl es más pequeño y no hay nada que ganar aquí.
set -euo pipefail

NAME="${1:-min-glibc}"
ROOT="${KLING_ROOT:-/var/lib/kindling}"
SUITE="${SUITE:-bookworm}"
MIRROR="${MIRROR:-http://deb.debian.org/debian}"
PKGS="${PKGS:-}"
HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib-ext4-shrink.sh"
BRIDGE="${BRIDGE:-./kling-bridge}"

# Familias con nombre, igual que en la base de Alpine: el operador no tiene que
# recordar la lista de paquetes, y dos operadores no acaban con dos bases
# "chrome" distintas.
CHROME=no
NODE=no
if [ -z "$PKGS" ]; then
  case "$NAME" in
    node-glibc) NODE=si ;;
    chrome)     NODE=si; CHROME=si ;;
  esac
else
  case "$NAME" in chrome) NODE=si; CHROME=si ;; esac
fi

# iproute2 SIEMPRE. El puente necesita `ip` dentro del invitado para poner la
# ruta a 169.254.169.254, que es por donde llegan los secretos por MMDS. Sin el,
# el puente avisa y sigue —"si el servicio no usa secretos, es inofensivo"— y el
# fallo aparece mucho despues, en el unico servicio que si los usa.
PKGS="iproute2 ca-certificates ${PKGS}"

# El tamaño es un TECHO, no una reserva: el fichero es disperso y al final se
# encoge al contenido. Chrome necesita mucho más sitio durante el desempaquetado.
if [ "$CHROME" = si ]; then SIZE="${SIZE:-1400M}"; else SIZE="${SIZE:-700M}"; fi

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
for c in debootstrap mkfs.ext4 curl; do
  command -v "$c" >/dev/null 2>&1 || [ -x "/usr/sbin/$c" ] || [ -x "/sbin/$c" ] || {
    echo "falta '$c'. En Debian:  apt-get install debootstrap e2fsprogs curl" >&2; exit 1; }
done
PATH="$PATH:/usr/sbin:/sbin"

case "$(uname -m)" in
  x86_64)  DEB_ARCH=amd64; CHROME_ARCH=linux64 ;;
  aarch64) DEB_ARCH=arm64; CHROME_ARCH=""      ;;
  *) echo "arquitectura no soportada: $(uname -m)" >&2; exit 1 ;;
esac
if [ "$CHROME" = si ] && [ -z "$CHROME_ARCH" ]; then
  echo "Google no publica chrome-headless-shell para $(uname -m)" >&2; exit 1
fi

# Node y chrome-headless-shell, fijados con su sha256 y no "el más reciente": lo
# que se descarga acaba dentro de cada microVM de esta base, y antes no se
# comprobaba nada. Se deciden aquí, antes del debootstrap, para fallar pronto.
#   - Node: el hash es el de SHASUMS256.txt de nodejs.org para esa versión. Otra
#     versión: NODE_VERSION=v22.x.y NODE_SHA256=<...>.
#   - Chrome for Testing no publica sha256, pero Google Cloud Storage sí da el
#     MD5 y el CRC32C de cada objeto (cabecera x-goog-hash de un HEAD). El hash
#     fijado abajo es el sha256 de un zip cuyo tamaño y MD5 coincidían con los de
#     GCS. Otra versión: CHROME_VERSION=… CHROME_SHA256=<…>, sacado así:
#       u=https://storage.googleapis.com/chrome-for-testing-public/$V/linux64/chrome-headless-shell-linux64.zip
#       curl -sI "$u" | grep -i x-goog-hash          # md5=<base64>
#       curl -sfLo chs.zip "$u"; openssl md5 -binary chs.zip | base64   # el mismo
#       sha256sum chs.zip                            # el que se fija
#     Sin hash no se instala.
NODE_VERSION="${NODE_VERSION:-v22.23.3}"
NARCH=x64; [ "$DEB_ARCH" = arm64 ] && NARCH=arm64
if [ "$NODE" = si ] && [ -z "${NODE_SHA256:-}" ]; then
  case "$NODE_VERSION-$NARCH" in
    v22.23.3-x64)   NODE_SHA256=df450af89261115ef9f9e3830c3eeb2cc9213b63c720b1af623cb5dcbe2e02de ;;
    v22.23.3-arm64) NODE_SHA256=a44aeb94849a299b22df10b9e622ec2f605c2183501bc40590705131de7c740f ;;
    *) echo "no hay sha256 fijado para Node $NODE_VERSION ($NARCH): pásalo en NODE_SHA256" >&2; exit 1 ;;
  esac
fi
CHROME_VERSION="${CHROME_VERSION:-154.0.8037.92}"
CHROME_SHA256="${CHROME_SHA256:-}"
if [ "$CHROME" = si ] && [ -z "$CHROME_SHA256" ]; then
  case "$CHROME_VERSION-$CHROME_ARCH" in
    # 120477194 bytes, md5 (GCS) qwuFqD7Srpqly/tYvTQSlQ==, comprobado 2026-09-30.
    154.0.8037.92-linux64) CHROME_SHA256=636aa5c79f2693632e9921b8bbb050038ba11672e02346c06c20f991aed096f9 ;;
  esac
fi
if [ "$CHROME" = si ] && [ -z "$CHROME_SHA256" ]; then
  echo "falta CHROME_SHA256: el sha256 de chrome-headless-shell-$CHROME_ARCH.zip $CHROME_VERSION" >&2
  echo "(Chrome for Testing no publica hashes; bájalo una vez, compruébalo y pásalo)" >&2
  exit 1
fi

sha256_de() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}
# comprobar_sha256 FICHERO ESPERADO: borra el fichero y sale si no coincide.
comprobar_sha256() {
  local got
  got="$(sha256_de "$1")"
  [ "$got" = "$2" ] || {
    rm -f "$1"
    echo "sha256 de $(basename "$1") no coincide: $got, se esperaba $2" >&2; exit 1; }
}

mnt="$(mktemp -d)"
cleanup() {
  umount "$mnt/proc" 2>/dev/null || true
  umount "$mnt" 2>/dev/null || true
  rmdir "$mnt" 2>/dev/null || true
}
trap cleanup EXIT

mkdir -p "$ROOT/images"
DEST="$ROOT/images/$NAME.ext4"
rm -f "$DEST"
truncate -s "$SIZE" "$DEST"
ext4_mkfs_sin_resize_inode "$DEST"
mount -o loop "$DEST" "$mnt"

echo "debootstrap $SUITE/$DEB_ARCH (tarda unos minutos)..."
# --variant=minbase: sin "standard", que arrastra ~120 MB de utilidades que una
# microVM efímera no usa jamás.
debootstrap --variant=minbase --arch="$DEB_ARCH" "$SUITE" "$mnt" "$MIRROR"

cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
mount --bind /proc "$mnt/proc" 2>/dev/null || true

if [ -n "$PKGS" ]; then
  echo "instalando: $PKGS"
  chroot "$mnt" env DEBIAN_FRONTEND=noninteractive sh -c \
    "apt-get update -qq && apt-get install -y --no-install-recommends $PKGS"
fi

if [ "$NODE" = si ]; then
  # Node desde nodejs.org y no desde apt: bookworm empaqueta el 18, y Playwright
  # se niega a arrancar con menos del 20. Versión y sha256 fijados arriba.
  NODE_TAR="node-$NODE_VERSION-linux-$NARCH.tar.xz"
  echo "  $NODE_TAR"
  curl -#fL "https://nodejs.org/dist/$NODE_VERSION/$NODE_TAR" -o "$mnt/tmp/node.tar.xz"
  comprobar_sha256 "$mnt/tmp/node.tar.xz" "$NODE_SHA256"
  chroot "$mnt" env DEBIAN_FRONTEND=noninteractive sh -c \
    "apt-get install -y --no-install-recommends xz-utils >/dev/null 2>&1" || true
  # --strip-components=1: el tarball lo trae todo bajo node-vX/, y lo que se
  # quiere es que bin/, lib/ e include/ caigan directos en /usr/local.
  chroot "$mnt" sh -c "tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 && rm -f /tmp/node.tar.xz"
  echo "  node $(chroot "$mnt" /usr/local/bin/node --version 2>/dev/null || echo '?')"
fi

if [ "$CHROME" = si ]; then
  # Las bibliotecas de las que depende el binario de Google. Sin capa de
  # interfaz siguen haciendo falta las de render y fuentes.
  echo "instalando las dependencias de chrome-headless-shell..."
  chroot "$mnt" env DEBIAN_FRONTEND=noninteractive sh -c \
    "apt-get install -y --no-install-recommends \
       libnss3 libnspr4 libdbus-1-3 libatk1.0-0 libatk-bridge2.0-0 libcups2 \
       libdrm2 libxkbcommon0 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
       libgbm1 libpango-1.0-0 libcairo2 libasound2 fonts-liberation ca-certificates"

  # Versión y sha256 fijados arriba (Chrome for Testing no publica hashes).
  VER="$CHROME_VERSION"
  echo "  chrome-headless-shell $VER"
  URL="https://storage.googleapis.com/chrome-for-testing-public/$VER/$CHROME_ARCH/chrome-headless-shell-$CHROME_ARCH.zip"
  curl -#fL "$URL" -o "$mnt/tmp/chs.zip"
  comprobar_sha256 "$mnt/tmp/chs.zip" "$CHROME_SHA256"
  chroot "$mnt" env DEBIAN_FRONTEND=noninteractive sh -c \
    "apt-get install -y --no-install-recommends unzip >/dev/null && \
     unzip -q /tmp/chs.zip -d /opt && rm /tmp/chs.zip && \
     apt-get purge -y unzip >/dev/null"
  mv "$mnt/opt/chrome-headless-shell-$CHROME_ARCH" "$mnt/opt/chrome-headless-shell"
  chmod +x "$mnt/opt/chrome-headless-shell/chrome-headless-shell"
  echo "$VER" > "$mnt/opt/chrome-headless-shell/VERSION"

  # EL MARCADOR. El puente no sabe —ni le importa— qué motor arranca: lee esto,
  # lanza el sidecar, espera a ready_url y añade session_args. Mismo CDP que
  # Chromium, así que ningún cliente MCP cambia.
  mkdir -p "$mnt/etc/kling"
  cat > "$mnt/etc/kling/browser.json" <<'BJSON'
{
  "sidecar": ["/opt/chrome-headless-shell/chrome-headless-shell","--no-sandbox","--disable-dev-shm-usage","--disable-gpu","--remote-debugging-address=127.0.0.1","--remote-debugging-port=9222","about:blank"],
  "ready_url": "http://127.0.0.1:9222/json/version",
  "session_args": []
}
BJSON
fi

# ADELGAZAR. Un rootfs de Debian trae documentacion, manuales y locales que en
# una microVM no lee nadie, y que ademas se pagan en el mem.file del dorado.
echo "adelgazando..."
chroot "$mnt" env DEBIAN_FRONTEND=noninteractive sh -c \
  "apt-get clean && rm -rf /var/lib/apt/lists/*" 2>/dev/null || true
rm -rf "$mnt/usr/share/doc" "$mnt/usr/share/man" "$mnt/usr/share/info" \
       "$mnt/usr/share/locale" "$mnt/usr/share/i18n" "$mnt/var/cache/apt" \
       "$mnt/var/log"/* 2>/dev/null || true
mkdir -p "$mnt/var/log"

umount "$mnt/proc" 2>/dev/null || true

# EL RESOLVER DEL INVITADO, no el del anfitrion.
#
# Durante la construccion se copia el resolv.conf del host para que apk/apt
# resuelvan. Dejarlo asi es un fallo silencioso: en un host con systemd-resolved
# ese fichero dice "nameserver 127.0.0.53", que dentro de la microVM no existe;
# y el resolver de verdad del host suele ser una IP privada (aqui 192.168.2.1)
# que el cortafuegos de salida BLOQUEA a proposito.
#
# El sintoma no aparece al construir ni al arrancar: aparece la primera vez que
# el servicio intenta resolver un nombre, como un ERR_NAME_NOT_RESOLVED que no
# apunta a nada. Se deja el mismo resolver publico que usa el modo allowlist
# (internal/net.DNSResolver), para que las dos vias coincidan.
printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > "$mnt/etc/resolv.conf"

install -m755 "$HERE/minimal-init.sh" "$mnt/sbin/overlay-init"
mkdir -p "$mnt/overlay" "$mnt/rom" "$mnt/run"

if [ -f "$BRIDGE" ]; then
  install -Dm755 "$BRIDGE" "$mnt/usr/local/bin/kling-bridge"
  echo "puente horneado en la base desde $BRIDGE"
else
  echo "aviso: sin puente en $BRIDGE — cada imagen de servicio llevará el suyo"
fi

umount "$mnt"
ext4_shrink_safe "$DEST"

# HOLGURA. resize2fs -M deja la imagen ajustada al byte, y entonces no cabe ni
# su propio recambio del puente: sustituirlo escribe al lado y renombra, asi que
# conviven dos copias un instante. `kling mcp refresh-bridge` sabe hacerla crecer,
# pero dejarla ya con aire ahorra ese ciclo de montaje y resize.
CUR=$(stat -c%s "$DEST")
truncate -s $((CUR + 32 * 1024 * 1024)) "$DEST"
resize2fs "$DEST" >/dev/null 2>&1 || true

echo
echo "imagen '$NAME' lista:"
echo "  lógico: $(du -h --apparent-size "$DEST" | cut -f1)   real: $(du -h "$DEST" | cut -f1)"
[ "$CHROME" = si ] && echo "  navegador: chrome-headless-shell $(cat "$ROOT/images/.chrome-version" 2>/dev/null || echo "$VER")"
echo "  úsala con:  BASE_IMAGE=$NAME ./80-mcp-image.sh <servicio>"
