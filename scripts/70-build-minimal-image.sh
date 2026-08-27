#!/usr/bin/env bash
# Construye una imagen base mínima sobre Alpine, pensada para herramientas
# efímeras: sin systemd, sin gestor de servicios, solo busybox y lo que añadas.
#
#   sudo ./70-build-minimal-image.sh                 # imagen "min"
#   sudo ./70-build-minimal-image.sh node            # base de runtime node
#   sudo ./70-build-minimal-image.sh python          # base de runtime python
#   sudo PKGS="nodejs npm" ./70-build-minimal-image.sh mcp-node
#
# El objetivo es el working set congelado, no el tamaño del disco: arrancar
# Ubuntu con systemd cuesta ~80 MB de snapshot; esto debería quedarse cerca de
# los ~36 MB que mide un arranque a /bin/sh pelado.
set -euo pipefail

NAME="${1:-min}"
ROOT="${KLING_ROOT:-/var/lib/kindling}"
SIZE="${SIZE:-256M}"
PKGS="${PKGS:-}"
HERE="$(cd "$(dirname "$0")" && pwd)"
BRIDGE="${BRIDGE:-./kling-bridge}"

# FAMILIAS DE RUNTIME CON NOMBRE. Todo el ahorro de las imágenes por capas sale
# de que el runtime viva en la base (docs/three-layers.md): sobre `min`, la capa
# de un servidor node arrastra nodejs+npm (~126 MiB) y no ahorra nada. Estas
# bases se construyen UNA vez por host, y con nombre fijo por dos razones:
#   - el operador no tiene que recordar la lista de paquetes de cada runtime, y
#     dos operadores no acaban con dos bases "python" distintas;
#   - `kling add` busca una base que se llame COMO LA FAMILIA (node, python)
#     para elegirla sola cuando no se le pasa -base. Renombrarlas aquí rompe
#     esa detección (hay un test que ata los dos extremos).
# PKGS explícito gana, para poder añadir extras sobre el preset.
if [ -z "$PKGS" ]; then
  case "$NAME" in
    node)   PKGS="nodejs npm" ;;
    python) PKGS="python3 py3-pip" ;;
  esac
fi

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
for c in curl mkfs.ext4; do
  command -v "$c" >/dev/null || { echo "falta $c" >&2; exit 1; }
done

case "$(uname -m)" in
  x86_64)  ALPINE_ARCH=x86_64 ;;
  aarch64) ALPINE_ARCH=aarch64 ;;
  *) echo "arquitectura no soportada: $(uname -m)" >&2; exit 1 ;;
esac

BASE="https://dl-cdn.alpinelinux.org/alpine/latest-stable/releases/$ALPINE_ARCH"
echo "buscando el minirootfs de Alpine más reciente ($ALPINE_ARCH)..."
TARBALL="$(curl -sfL "$BASE/" \
  | grep -oE "alpine-minirootfs-[0-9.]+-$ALPINE_ARCH\.tar\.gz" | sort -V | tail -1)"
[ -n "$TARBALL" ] || { echo "no pude resolver el minirootfs" >&2; exit 1; }
echo "  -> $TARBALL"

work="$(mktemp -d)"
mnt="$(mktemp -d)"
cleanup() { umount "$mnt" 2>/dev/null || true; rm -rf "$work"; rmdir "$mnt" 2>/dev/null || true; }
trap cleanup EXIT

curl -#fL "$BASE/$TARBALL" -o "$work/rootfs.tar.gz"

mkdir -p "$ROOT/images"
DEST="$ROOT/images/$NAME.ext4"

# Disperso: el tamaño lógico es un techo, no una reserva.
rm -f "$DEST"
truncate -s "$SIZE" "$DEST"
mkfs.ext4 -q -F -E nodiscard "$DEST"
mount -o loop "$DEST" "$mnt"

echo "extrayendo..."
tar -xzf "$work/rootfs.tar.gz" -C "$mnt"

if [ -n "$PKGS" ]; then
  echo "instalando: $PKGS"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  chroot "$mnt" /sbin/apk add --no-cache $PKGS
  umount "$mnt/proc" 2>/dev/null || true
fi

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

# EL PUENTE, EN LA BASE.
#
# El puente es el PID 1 de los servicios stdio y hasta ahora viajaba dentro de
# cada imagen: actualizarlo eran N ficheros, uno por servicio, y cada uno exigía
# montar su ext4. Horneado aquí es UNO —`kling images refresh min`— y todas las
# capas que se apoyen en esta base lo heredan sin tocarlas.
#
# 80-mcp-image.sh sigue metiéndolo en la capa cuando el binario que trae la base
# no es el mismo, así que una base sin puente (o con otro) no rompe nada: la
# imagen del servicio se queda con el suyo, que gana por estar en la lower de
# delante.
if [ -f "$BRIDGE" ]; then
  # -D: el rootfs mínimo puede no traer /usr/local/bin, e install no crea padres.
  install -Dm755 "$BRIDGE" "$mnt/usr/local/bin/kling-bridge"
  echo "puente horneado en la base desde $BRIDGE"
else
  echo "aviso: sin puente en $BRIDGE — cada imagen de servicio llevará el suyo"
  echo "       (compílalo con 'make bridge' y repite para ahorrarte N copias)"
fi

umount "$mnt"
e2fsck -fp "$DEST" >/dev/null 2>&1 || true
resize2fs -M "$DEST" >/dev/null 2>&1 || true   # encoge al contenido real

echo
echo "imagen '$NAME' lista:"
echo "  lógico: $(du -h --apparent-size "$DEST" | cut -f1)   real: $(du -h "$DEST" | cut -f1)"
echo "  úsala con:  kling run -image $NAME"
