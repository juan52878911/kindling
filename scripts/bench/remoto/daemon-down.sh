#!/usr/bin/env bash
# Para y borra el daemon privado kt-bench (receta parar/limpiar de 93-e2e-linux-ci.sh).
BASE=/srv/kt-bench; ROOT=$BASE/root; UNIT=kt-bench
systemctl stop "$UNIT" >/dev/null 2>&1
pkill -f -- "$ROOT/" >/dev/null 2>&1
sleep 1
# Lo que un VMM vivo o un montaje retenido dejaría a la variante siguiente (solo en la instancia del
# banco): firecracker restantes, netns kl-*, el almacén CoW (cow.xfs) y sus dispositivos loop.
pkill -9 -x firecracker >/dev/null 2>&1
sleep 1
ip netns list 2>/dev/null | awk '/^kl-/{print $1}' | while read -r n; do ip netns delete "$n" 2>/dev/null; done
grep 'cow\.xfs' /proc/mounts | awk '{print $2}' | sort -r | while read -r m; do umount -l "$m" 2>/dev/null; done
losetup -nO NAME,BACK-FILE 2>/dev/null | awk '/cow\.xfs/{print $1}' | while read -r l; do losetup -d "$l" 2>/dev/null; done
grep -o " ${ROOT}[^ ]*" /proc/mounts | sort -r | while read -r m; do umount -l "$m" 2>/dev/null; done
systemctl reset-failed "$UNIT" >/dev/null 2>&1
rm -rf "$BASE" "/run/$UNIT"
ls -d "$BASE" 2>/dev/null || echo "kt-bench borrado"
