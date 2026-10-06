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

# Rehacer los montajes dentro de la nueva raíz. Una imagen aplanada desde OCI
# puede no traer los puntos de montaje: sin ellos el mount falla en silencio.
mkdir -p /proc /sys /dev /tmp /run 2>/dev/null || true
mount -t proc     proc     /proc     2>/dev/null || true
mount -t sysfs    sysfs    /sys      2>/dev/null || true
mount -t devtmpfs devtmpfs /dev      2>/dev/null || true
mount -t tmpfs    tmpfs    /tmp      2>/dev/null || true
# /run: en una imagen de Docker (constructor oci, /etc/kindling/oci.json) se
# deja el de la imagen, como hace Docker: hay imágenes que traen ahí
# directorios con su dueño (mariadb, /run/mysqld del usuario mysql) y su
# entrypoint falla si un tmpfs vacío los tapa. En las bases de kindling, tmpfs.
[ -e /etc/kindling/oci.json ] || mount -t tmpfs tmpfs /run 2>/dev/null || true

umount /rom/proc /rom/sys /rom/dev 2>/dev/null || true

# CONTRATO DE RUNTIME. Lo que Docker (y systemd en la base con systemd) da por
# hecho y devtmpfs no crea: /dev/fd y /dev/std* (la sustitución de procesos de
# bash, <(...), abre /dev/fd/N; el initdb de las imágenes de Postgres muere sin
# él), /dev/shm (la memoria compartida POSIX de Postgres, Chromium, Python
# multiprocessing) y un /etc/hosts que resuelva localhost y el propio nombre.
# Nada de esto pisa lo que la imagen ya traiga.
for l in fd:/proc/self/fd stdin:/proc/self/fd/0 stdout:/proc/self/fd/1 stderr:/proc/self/fd/2; do
  [ -e "/dev/${l%%:*}" ] || [ -L "/dev/${l%%:*}" ] || ln -s "${l#*:}" "/dev/${l%%:*}" 2>/dev/null || true
done
if mkdir -p /dev/shm 2>/dev/null; then
  mount -t tmpfs -o mode=1777,nosuid,nodev tmpfs /dev/shm 2>/dev/null || true
fi

# El nombre: el que fije el kernel (ip=...:<nombre>:...) manda. Sin él, el
# kernel deja la IP del invitado (172.16.0.2) o "(none)", que no es un nombre:
# entonces "kindling". Se escribe en /proc y no con hostname(1), que no está en
# todas las imágenes.
HN=""
read -r HN < /proc/sys/kernel/hostname 2>/dev/null || true
case "$HN" in
  "(none)") HN="" ;;
  *[!0-9.]*) ;;   # un nombre de verdad
  *) HN="" ;;     # vacío o una IPv4
esac
if [ -z "$HN" ]; then
  HN=kindling
  echo "$HN" > /proc/sys/kernel/hostname 2>/dev/null || true
fi

# Solo IPv4: el invitado arranca con IPv6 apagado (ipv6.disable=1 o
# disable_ipv6=1, ver internal/net), así que ::1 no existe y un "::1 localhost"
# haría que un cliente probara primero una dirección inalcanzable. El nombre va
# a 127.0.1.1, como en Debian: siempre alcanzable, también con -egress none.
{
  grep -qsE '^127\.0\.0\.1[[:space:]]+([^#]*[[:space:]])?localhost([[:space:]]|$)' /etc/hosts || echo "127.0.0.1	localhost" >> /etc/hosts
  grep -qswF "$HN" /etc/hosts || echo "127.0.1.1	$HN" >> /etc/hosts
} 2>/dev/null || true

# /entrypoint es lo que convierte esta microVM en "una herramienta". Se
# reemplaza al construir la imagen de cada servidor MCP.
if [ -x /entrypoint ]; then
    exec /entrypoint
fi

echo "kindling: sin /entrypoint, cayendo a shell"
exec /bin/sh
