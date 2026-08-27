#!/bin/sh
# init de las imágenes mínimas de kindling.
#
# Hace lo mismo que overlay-init pero sin ceder a systemd: monta el overlay,
# prepara los pseudo-filesystems y arranca directamente la herramienta.
#
# Saltarse systemd es la diferencia entre ~80 MB y ~36 MB de working set
# congelado. Una microVM efímera que solo sirve una herramienta no necesita
# gestor de servicios, ni journal, ni resolución de dependencias de arranque.
set -e

# El kernel arranca a PID 1 sin entorno. Sin PATH, cualquier programa invocado
# por nombre —y no por ruta absoluta— falla con "executable file not found".
export PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export HOME=/root

OVERLAY_DEV="${OVERLAY_DEV:-/dev/vdb}"

mount -t proc     proc     /proc
mount -t sysfs    sysfs    /sys
mount -t devtmpfs devtmpfs /dev 2>/dev/null || true

mkdir -p /overlay
mount -t ext4 "$OVERLAY_DEV" /overlay

mkdir -p /overlay/upper /overlay/work /overlay/merged

# IMÁGENES POR CAPAS. Si el anfitrión engancha una capa de servicio, su device
# llega en kling.layer=/dev/vdX y se monta de lower POR DELANTE de la base:
# lowerdir=<capa>/upper:/. Así la base deja de duplicarse por servicio. El
# contenido de la capa cuelga de /upper dentro de su ext4 porque se construyó
# como el upperdir de un overlay sobre la base (scripts/80-mcp-image.sh).
#
# Sin ese parámetro, el camino es el de siempre: una sola lower, la raíz. Las
# imágenes monolíticas ya construidas arrancan exactamente igual.
#
# La capa se monta dentro de /overlay y no en la raíz: la raíz es de solo
# lectura, así que aquí no se puede crear un punto de montaje nuevo.
#
# La línea de comandos se parte por palabras y se compara entera, sin regex: el
# sed de busybox va contra el regex de musl, que no trae las extensiones de GNU,
# y un patrón que no case dejaría la capa sin montar — un invitado sin
# /entrypoint, que no se parece en nada a la causa.
LOWER="/"
LAYER_DEV=""
for tok in $(cat /proc/cmdline); do
  case "$tok" in
    kling.layer=*) LAYER_DEV="${tok#kling.layer=}" ;;
  esac
done
if [ -n "$LAYER_DEV" ]; then
  mkdir -p /overlay/svc
  mount -t ext4 -o ro "$LAYER_DEV" /overlay/svc
  LOWER="/overlay/svc/upper:/"
fi

mount -t overlay overlay \
  -o "lowerdir=$LOWER,upperdir=/overlay/upper,workdir=/overlay/work" \
  /overlay/merged

mkdir -p /overlay/merged/rom
cd /overlay/merged
pivot_root . rom

# Rehacer los montajes dentro de la nueva raíz.
mount -t proc     proc     /proc     2>/dev/null || true
mount -t sysfs    sysfs    /sys      2>/dev/null || true
mount -t devtmpfs devtmpfs /dev      2>/dev/null || true
mount -t tmpfs    tmpfs    /tmp      2>/dev/null || true
mount -t tmpfs    tmpfs    /run      2>/dev/null || true

umount /rom/proc /rom/sys /rom/dev 2>/dev/null || true

# /entrypoint es lo que convierte esta microVM en "una herramienta". Se
# reemplaza al construir la imagen de cada servidor MCP.
if [ -x /entrypoint ]; then
    exec /entrypoint
fi

echo "kindling: sin /entrypoint, cayendo a shell"
exec /bin/sh
