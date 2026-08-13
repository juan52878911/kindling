#!/usr/bin/env bash
# Activa/desactiva un dispositivo zram como swap comprimido para densificar
# microVMs de kindling. OPT-IN: lo llama zram-kindling.service, no se ejecuta
# solo. Ver docs/densidad-zram.md.
#
# Cero código de kindling: esto solo toca el sysfs del host (/sys/block/zram*).
# No requiere ninguna herramienta especial más allá de modprobe y swapon.
#
# Uso:
#   zram-kindling.sh start   # crea el dispositivo y lo activa como swap
#   zram-kindling.sh stop    # lo desactiva y lo libera
set -euo pipefail

CONF="${ZRAM_KINDLING_CONF:-/etc/kling/zram-kindling.conf}"
# Apuntamos aquí qué dispositivo creamos, para que stop desactive SOLO el
# nuestro y no un zram de la distro (p.ej. de un zram-generator).
STATE="${ZRAM_KINDLING_STATE:-/run/zram-kindling.dev}"
DEV_NAME=""   # se rellena en start; p.ej. zram3

# Valores por defecto por si el fichero de conf no define algo.
ZRAM_ALGO=zstd
ZRAM_SIZE_PERCENT=50
ZRAM_MEM_LIMIT_PERCENT=25
ZRAM_PRIORITY=100
ZRAM_STREAMS=""

log() { echo "zram-kindling: $*"; }
die() { echo "zram-kindling: error: $*" >&2; exit 1; }

# RAM total del host en bytes, leída de /proc/meminfo (viene en kB).
mem_total_bytes() {
	local kb
	kb=$(awk '/^MemTotal:/ {print $2; exit}' /proc/meminfo)
	[ -n "$kb" ] || die "no pude leer MemTotal de /proc/meminfo"
	echo $(( kb * 1024 ))
}

load_conf() {
	if [ -r "$CONF" ]; then
		# shellcheck disable=SC1090
		. "$CONF"
		log "configuración leída de $CONF"
	else
		log "sin $CONF; uso valores por defecto"
	fi
}

start() {
	[ -e /dev/kvm ] || log "aviso: este host no tiene /dev/kvm; ¿seguro que aquí corren microVMs?"
	load_conf

	local total limit_pct disksize memlimit
	total=$(mem_total_bytes)
	disksize=$(( total * ZRAM_SIZE_PERCENT / 100 ))
	memlimit=$(( total * ZRAM_MEM_LIMIT_PERCENT / 100 ))

	[ "$ZRAM_MEM_LIMIT_PERCENT" -lt "$ZRAM_SIZE_PERCENT" ] \
		|| log "aviso: mem_limit ($ZRAM_MEM_LIMIT_PERCENT%) no es menor que disksize ($ZRAM_SIZE_PERCENT%); la red de seguridad no hará nada"

	# Cargar el módulo. Sin num_devices para no pisar zram existentes: pedimos
	# uno nuevo por el controlador de abajo.
	modprobe zram || die "no pude cargar el módulo zram (¿lo trae el kernel?)"

	# hot_add devuelve el número de un dispositivo recién creado, sin colisionar
	# con los que ya use el sistema (p.ej. un zram-generator de la distro).
	local num
	if [ -e /sys/class/zram-control/hot_add ]; then
		num=$(cat /sys/class/zram-control/hot_add)
	else
		num=0   # kernel viejo sin control dinámico: caemos a zram0
	fi
	DEV_NAME="zram${num}"
	local sysfs="/sys/block/${DEV_NAME}"
	[ -d "$sysfs" ] || die "no apareció $sysfs"

	# Orden importa: algoritmo y límites ANTES de fijar disksize.
	echo "$ZRAM_ALGO" > "${sysfs}/comp_algorithm" \
		|| die "el kernel no acepta el algoritmo '$ZRAM_ALGO' (mira ${sysfs}/comp_algorithm)"

	if [ -n "$ZRAM_STREAMS" ] && [ -w "${sysfs}/max_comp_streams" ]; then
		echo "$ZRAM_STREAMS" > "${sysfs}/max_comp_streams" || true
	fi

	# mem_limit es el tope FÍSICO del pool comprimido; disksize es la capacidad
	# LÓGICA que ve el swap. Fijar disksize también inicializa el dispositivo.
	echo "$memlimit"  > "${sysfs}/mem_limit"
	echo "$disksize"  > "${sysfs}/disksize"

	mkswap "/dev/${DEV_NAME}" >/dev/null || die "mkswap falló en /dev/${DEV_NAME}"
	swapon --priority "$ZRAM_PRIORITY" "/dev/${DEV_NAME}" \
		|| die "swapon falló en /dev/${DEV_NAME}"

	echo "$DEV_NAME" > "$STATE"
	log "activo: /dev/${DEV_NAME} algo=${ZRAM_ALGO} disksize=$(( disksize / 1024 / 1024 ))MiB mem_limit=$(( memlimit / 1024 / 1024 ))MiB prio=${ZRAM_PRIORITY}"
}

stop() {
	# Solo tocamos el dispositivo que anotamos en start; nunca un zram ajeno.
	if [ ! -r "$STATE" ]; then
		log "no hay estado en $STATE; no había swap zram nuestro que quitar"
		return 0
	fi
	local dev num sysfs
	dev=$(cat "$STATE")
	sysfs="/sys/block/${dev}"

	if grep -q "^/dev/${dev} " /proc/swaps 2>/dev/null; then
		swapoff "/dev/${dev}" || log "aviso: swapoff /dev/${dev} falló"
	fi

	# Liberar el dispositivo si el control dinámico está disponible; si no, reset.
	num=${dev#zram}
	if [ -e /sys/class/zram-control/hot_remove ]; then
		echo "$num" > /sys/class/zram-control/hot_remove 2>/dev/null || true
	elif [ -w "${sysfs}/reset" ]; then
		echo 1 > "${sysfs}/reset" 2>/dev/null || true
	fi

	rm -f "$STATE"
	log "swap zram /dev/${dev} desactivado"
}

case "${1:-}" in
	start) start ;;
	stop)  stop ;;
	*) die "uso: $0 {start|stop}" ;;
esac
