#!/bin/bash
# watchdog.sh — corta una prueba si el CT se acerca al límite (docs/proxmox.md).
#
#   x86_64/watchdog.sh <patrón-de-pkill> [<prefijo-de-máquinas-a-borrar>]
#
# Cada 2 s mira MemAvailable, la swap usada del CT (respecto a la del arranque)
# y la presión de memoria de su cgroup. Si MemAvailable < MIN_AVAIL_MIB (1024),
# la swap crece más de MAX_SWAP_MIB (64) o some avg10 ≥ MAX_PSI (5 %), mata
# los procesos que casan con el patrón, borra las máquinas del prefijo (si se
# da) y sale con 1. Sale con 0 cuando ya no queda ningún proceso del patrón.
set -uo pipefail
PAT="${1:?usage: watchdog.sh <pkill pattern> [machine prefix]}"
PFX="${2:-}"
MIN_AVAIL_MIB="${MIN_AVAIL_MIB:-1024}"
MAX_SWAP_MIB="${MAX_SWAP_MIB:-64}"
MAX_PSI="${MAX_PSI:-5}"
mi() { awk -v k="$1" '$1 == k":" { print int($2 / 1024) }' /proc/meminfo; }
psi() { awk '$1 == "some" { split($2, a, "="); print int(a[2]) }' /sys/fs/cgroup/memory.pressure 2>/dev/null || echo 0; }
# Los PID que casan con el patrón, sin este mismo script (su línea de órdenes
# también lleva el patrón: con un pkill -f a secas se mataba a sí mismo).
# (Ni sus subshells, que llevan la misma línea con otro PID.)
suyos() {
  local p c
  for p in $(pgrep -f -- "$PAT"); do
    # (El 2>/dev/null fuera de las llaves: el < falla antes que el de dentro
    # cuando el proceso ya terminó; uno que ya no existe no cuenta.)
    { c="$(tr '\0' ' ' <"/proc/$p/cmdline")"; } 2>/dev/null || continue
    case "$c" in *watchdog.sh*) ;; *) echo "$p" ;; esac
  done
}
sw0=$(( $(mi SwapTotal) - $(mi SwapFree) ))
while [ -n "$(suyos)" ]; do
  av="$(mi MemAvailable)"; sw=$(( $(mi SwapTotal) - $(mi SwapFree) - sw0 )); p="$(psi)"
  why=""
  [ "$av" -lt "$MIN_AVAIL_MIB" ] && why="MemAvailable $av MiB"
  [ "$sw" -gt "$MAX_SWAP_MIB" ] && why="$why swap +$sw MiB"
  [ "$p" -ge "$MAX_PSI" ] && why="$why psi $p %"
  if [ -n "$why" ]; then
    echo "$(date +%T) watchdog: STOP ($why)"
    # shellcheck disable=SC2046
    kill $(suyos) 2>/dev/null
    if [ -n "$PFX" ]; then
      for m in $(kling ps -json | python3 -c 'import json,sys; [print(m["name"]) for m in json.load(sys.stdin) if m["name"].startswith(sys.argv[1])]' "$PFX"); do
        kling rm -f "$m" >/dev/null 2>&1 && echo "  removed $m"
      done
    fi
    exit 1
  fi
  sleep 2
done
echo "$(date +%T) watchdog: nothing left to watch; min ok"
