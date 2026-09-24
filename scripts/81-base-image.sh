#!/usr/bin/env bash
# Constructor genérico de kindling: una capa sobre una imagen base con paquetes
# del sistema y el agente de invitado (kling-guest) como PID 1.
#
# Es el motor del constructor "base" (`kling builder base`), que valida la
# petición en Go y llama aquí con todo ya comprobado. No se ejecuta a mano: lo
# lanza el daemon, como root, a través de /usr/local/lib/kindling/builders/base.
#
# Entorno (lo pone `kling builder base`):
#   KLING_ROOT  directorio de datos del daemon
#   NAME        nombre de la imagen            BASE   imagen base (min por defecto)
#   GROW        MiB reservados para la capa    PKGS   paquetes apk/apt, separados por espacios
#   AGENT       ruta del kling-guest a meter   ENV_FILE  líneas KEY=VALUE a exportar
#   ROOTFS_DIR  (opcional) árbol de ficheros que se copia tal cual a la raíz
#   SERVICE     (opcional) ruta absoluta, dentro de la imagen, de un ejecutable
#               que el entrypoint arranca en segundo plano y relanza si muere,
#               antes de ceder el PID 1 al agente. Su salida: /var/log/service.log
#
# ROOTFS_DIR y SERVICE los usa el constructor "llm" (llama-server + un GGUF, ver
# docs/von.md), pero no saben nada de modelos: sirven a cualquier imagen que sea
# "el agente de siempre más un servidor propio".
#
# El motor de capas (overlay sobre la base, e2fsck, encogido) es el mismo que el
# de 80-mcp-image.sh; ver docs/three-layers.md.
set -euo pipefail

ROOT="${KLING_ROOT:-/var/lib/kindling}"
NAME="${NAME:?falta NAME}"
BASE="${BASE:-min}"
GROW="${GROW:-0}"
PKGS="${PKGS:-}"
AGENT="${AGENT:?falta AGENT}"
ENV_FILE="${ENV_FILE:-}"
ROOTFS_DIR="${ROOTFS_DIR:-}"
SERVICE="${SERVICE:-}"

[ "$(id -u)" -eq 0 ] || { echo "ejecútalo como root" >&2; exit 1; }
BASE_IMG="$ROOT/images/$BASE.ext4"
[ -f "$BASE_IMG" ] || { echo "falta la imagen base '$BASE'" >&2; exit 1; }
if [ -n "$ROOTFS_DIR" ] && [ ! -d "$ROOTFS_DIR" ]; then
  echo "ROOTFS_DIR $ROOTFS_DIR is not a directory" >&2; exit 1
fi
case "$SERVICE" in
  ""|/*) ;;
  *) echo "SERVICE must be an absolute path inside the image: $SERVICE" >&2; exit 1 ;;
esac
[ "$(head -c4 "$AGENT" 2>/dev/null | od -An -tx1 | tr -d ' \n')" = "7f454c46" ] || {
  echo "$AGENT no es un binario ELF: compila kling-guest para linux (make guest)" >&2; exit 1; }

# Entrecomillado POSIX: el entrypoint es #!/bin/sh (busybox en Alpine).
sq() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }

pkg_add() {
  [ $# -gt 0 ] || return 0
  if [ -x "$mnt/sbin/apk" ]; then
    chroot "$mnt" /sbin/apk add --no-cache "$@"
  else
    chroot "$mnt" env DEBIAN_FRONTEND=noninteractive sh -c \
      "apt-get update -qq && apt-get install -y --no-install-recommends $*" \
      && chroot "$mnt" sh -c "apt-get clean; rm -rf /var/lib/apt/lists/*" 2>/dev/null
  fi
}

LAYER="$ROOT/images/$NAME.layer.ext4"
[ "$GROW" -gt 0 ] || GROW=256
rm -f "$LAYER"
truncate -s "${GROW}M" "$LAYER"
mkfs.ext4 -q -F -O '^has_journal' -E nodiscard "$LAYER"

mnt="$(mktemp -d)"; base_mnt="$(mktemp -d)"; layer_mnt="$(mktemp -d)"
ov_up() {
  mount -o loop,ro "$BASE_IMG" "$base_mnt"
  mount -o loop "$LAYER" "$layer_mnt"
  mkdir -p "$layer_mnt/upper" "$layer_mnt/work"
  mount -t overlay overlay \
    -o "lowerdir=$base_mnt,upperdir=$layer_mnt/upper,workdir=$layer_mnt/work" "$mnt"
}
ov_down() {
  umount "$mnt/proc" 2>/dev/null || true
  umount "$mnt"       2>/dev/null || true
  umount "$layer_mnt" 2>/dev/null || true
  umount "$base_mnt"  2>/dev/null || true
}
cleanup() { ov_down; rmdir "$mnt" "$layer_mnt" "$base_mnt" 2>/dev/null || true; }
trap cleanup EXIT
ov_up

if [ -n "$PKGS" ]; then
  echo "instalando en la imagen: $PKGS"
  cp /etc/resolv.conf "$mnt/etc/resolv.conf" 2>/dev/null || true
  mount --bind /proc "$mnt/proc" 2>/dev/null || true
  # shellcheck disable=SC2086 # PKGS ya viene validado y separado por espacios
  pkg_add $PKGS
  umount "$mnt/proc" 2>/dev/null || true
fi

install -Dm755 "$AGENT" "$mnt/usr/local/bin/kling-guest"

if [ -n "$ROOTFS_DIR" ]; then
  # -a para conservar los enlaces simbólicos de las bibliotecas (libfoo.so.0 ->
  # libfoo.so.0.1.2) en vez de duplicar cada una. Lo prepara root, así que la
  # propiedad que se conserva es la buena.
  cp -a "$ROOTFS_DIR/." "$mnt/"
fi
if [ -n "$SERVICE" ] && [ ! -x "$mnt$SERVICE" ]; then
  echo "SERVICE $SERVICE is not an executable inside the image" >&2; exit 1
fi

{
  echo '#!/bin/sh'
  echo '# Generado por el constructor base de kindling: el agente de invitado es PID 1.'
  echo 'export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin'
  echo 'export HOME=/root'
  if [ -n "$ENV_FILE" ] && [ -s "$ENV_FILE" ]; then
    while IFS= read -r kv; do
      [ -n "$kv" ] || continue
      printf 'export %s=%s\n' "${kv%%=*}" "$(sq "${kv#*=}")"
    done < "$ENV_FILE"
  fi
  if [ -n "$SERVICE" ]; then
    # El servicio arranca ANTES que el agente y en segundo plano: el agente es
    # PID 1 (recoge huérfanos, monta volúmenes, sirve exec) y el servicio queda
    # como su hijo. El bucle lo relanza si muere, que un servidor caído en una
    # réplica restaurada no tiene a nadie más que lo levante.
    echo "( while :; do $(sq "$SERVICE"); echo \"service exited with \$?, restarting in 1s\"; sleep 1; done ) </dev/null >>/var/log/service.log 2>&1 &"
  fi
  echo 'exec /usr/local/bin/kling-guest -listen :8080'
} > "$mnt/entrypoint"
chmod 755 "$mnt/entrypoint"

# El resolver del host se copió para instalar; dentro de la microVM no vale
# (127.0.0.53, IP privadas bloqueadas por el egress). Ver 80-mcp-image.sh.
printf 'nameserver 1.1.1.1\nnameserver 8.8.8.8\n' > "$mnt/etc/resolv.conf"
sync
ov_down

mount -o loop "$LAYER" "$layer_mnt"
rm -rf "$layer_mnt/work"
umount "$layer_mnt"
sync

# Comprobar ANTES de encoger: resize2fs se niega a tocar un fs sin comprobar.
if ! e2fsck -fp "$LAYER" >/dev/null 2>&1; then
  e2fsck -fy "$LAYER" >/dev/null 2>&1 || { echo "ERROR: la capa quedó irreparable" >&2; exit 1; }
fi
if resize2fs -M "$LAYER" >/dev/null 2>&1; then
  blocks=$(dumpe2fs -h "$LAYER" 2>/dev/null | awk -F: '/^Block count/{gsub(/ /,"",$2); print $2}')
  bsize=$(dumpe2fs -h "$LAYER" 2>/dev/null | awk -F: '/^Block size/{gsub(/ /,"",$2); print $2}')
  [ -n "$blocks" ] && [ -n "$bsize" ] && truncate -s "$((blocks * bsize))" "$LAYER"
  e2fsck -fp "$LAYER" >/dev/null 2>&1 || { echo "ERROR: la capa quedó dañada al encogerla" >&2; exit 1; }
else
  echo "AVISO: no se pudo encoger la capa; ocupará más disco del necesario" >&2
fi
echo "imagen '$NAME' lista — capa sobre base '$BASE' ($(du -h "$LAYER" | cut -f1) reales)"
