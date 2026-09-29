#!/bin/bash
# Cuánto ensucia cada teléfono restaurado en Firecracker (Linux): ahí los clones
# mapean el mem.file del dorado con MAP_PRIVATE y solo pagan las páginas que
# escriben (Private_Dirty del proceso de firecracker). En el Mac (vz) no hay
# equivalente: restaurar copia toda la RAM. Ver docs/densidad.md.
#
#   KLING_HOST=unix:///ruta/kling.sock mide-fc.sh IMAGEN MEM N
#
# Arranca IMAGEN en frío con MEM MiB, la deja lista (pantalla despierta, sin
# animaciones), la guarda como dorado y restaura N clones. Para cada uno da
# Private_Dirty recién restaurado, a los 30 s y tras un trabajo típico
# (5 × uiautomator dump + screencap, abrir Ajustes y volver).
# Corre como root en el host del daemon (lee /proc/<pid>/smaps de firecracker).
set -u
img="${1:?imagen}"; mem="${2:?mem}"; N="${3:-4}"
K="${KLING:-kling}"
G="fcg-$img-$mem"
now() { date +%s.%N; }
ax() { $K exec -timeout 90s "$1" -- android-sh "$2"; }
dump() { $K exec -timeout 90s "$1" -- android-sh 'uiautomator dump /data/local/tmp/ui.xml >/dev/null 2>&1 && cat /data/local/tmp/ui.xml' 2>/dev/null | grep -o '<node' | wc -l; }
pidde() { # el firecracker de una máquina: su id aparece en la línea de órdenes
  local id p; id="$($K ps -json | python3 -c 'import json,sys; print(next(m["id"] for m in json.load(sys.stdin) if m["name"]==sys.argv[1]))' "$1" 2>/dev/null)"
  for p in $(pgrep firecracker); do tr '\0' ' ' </proc/"$p"/cmdline | grep -q "${id:0:12}" && { echo "$p"; return; }; done
}
priv() {
  local pid; pid="$(pidde "$1")"
  [ -n "$pid" ] || { echo "sin proceso"; return; }
  awk '/^Private_Dirty/ {pd+=$2} /^Shared_Clean/ {sc+=$2} /^Rss/ {r+=$2}
       END {printf "Rss %d MiB · Private_Dirty %d MiB · Shared_Clean %d MiB\n", r/1024, pd/1024, sc/1024}' /proc/"$pid"/smaps
}

t0=$(now)
$K run -image "$img" -name fcgold -cpus 2 -mem "$mem" -cpu-pct 200 -egress none -allow-exec >/dev/null || exit 1
v=""; for _ in $(seq 1 300); do v=$(ax fcgold 'getprop sys.boot_completed' 2>/dev/null | tr -d '\r\n '); [ "$v" = 1 ] && break; sleep 1; done
echo "dorado: boot_completed=$v a los $(echo "$(now) - $t0" | bc) s"
ax fcgold 'svc power stayon true; settings put system screen_off_timeout 2147483647; locksettings set-disabled true; input keyevent KEYCODE_WAKEUP; wm dismiss-keyguard; settings put global window_animation_scale 0; settings put global transition_animation_scale 0; settings put global animator_duration_scale 0; input keyevent KEYCODE_HOME' >/dev/null 2>&1
sleep 30
echo "dorado: dump $(dump fcgold) nodos"
$K template rm -f "$G" >/dev/null 2>&1
$K save -replace fcgold "$G" >/dev/null && $K rm -f fcgold >/dev/null

for i in $(seq 1 "$N"); do
  t0=$(now); $K run -from "$G" -name "fcc$i" >/dev/null || { echo "clon $i no arrancó"; break; }
  t1=$(now)
  n=0; while [ "$(dump "fcc$i")" -lt 1 ] && [ $n -lt 100 ]; do n=$((n + 1)); sleep 0.3; done
  echo "clon $i: restaurar $(echo "$t1 - $t0" | bc) s · restaurar→dump $(echo "$(now) - $t0" | bc) s · $(priv "fcc$i")"
done
sleep 30
for i in $(seq 1 "$N"); do echo "clon $i a los 30 s: $(priv "fcc$i")"; done
for i in $(seq 1 "$N"); do
  for _ in 1 2 3 4 5; do dump "fcc$i" >/dev/null; ax "fcc$i" 'screencap -p' >/dev/null 2>&1; done
  ax "fcc$i" 'am start -W -a android.settings.SETTINGS' >/dev/null 2>&1; sleep 3
  ax "fcc$i" 'input keyevent KEYCODE_HOME' >/dev/null 2>&1
  echo "clon $i tras trabajo: $(priv "fcc$i") · dump $(dump "fcc$i") nodos"
done
free -m
[ -n "${KEEP:-}" ] || { for i in $(seq 1 "$N"); do $K rm -f "fcc$i" >/dev/null; done; $K template rm -f "$G" >/dev/null; }
