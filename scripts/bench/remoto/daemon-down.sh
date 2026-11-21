#!/usr/bin/env bash
# Para y borra el daemon privado kt-bench (receta parar/limpiar de 93-e2e-linux-ci.sh).
BASE=/srv/kt-bench; ROOT=$BASE/root; UNIT=kt-bench
systemctl stop "$UNIT" >/dev/null 2>&1
pkill -f -- "$ROOT/" >/dev/null 2>&1
sleep 1
grep -o " ${ROOT}[^ ]*" /proc/mounts | sort -r | while read -r m; do umount -l "$m" 2>/dev/null; done
systemctl reset-failed "$UNIT" >/dev/null 2>&1
rm -rf "$BASE" "/run/$UNIT"
ls -d "$BASE" 2>/dev/null || echo "kt-bench borrado"
