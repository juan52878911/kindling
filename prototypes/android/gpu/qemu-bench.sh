#!/usr/bin/env bash
# Mide un teléfono de qemu-phone.sh con las pruebas de docs/gpu.md: arranque,
# memoria, UI (arrastre continuo en Ajustes, 30 s) y WebGL (raymarch a pantalla
# completa, 30 s). La CPU es la del proceso QEMU entero (vCPU + virgl en el
# host: utime+stime de /proc/PID/stat); la GPU, drm-engine-render del fdinfo
# del i915 de ese proceso.
#
#   qemu-bench.sh N gpu|cpu        # arranca el teléfono N, mide y lo deja vivo
#   SKIP_UP=1 qemu-bench.sh N gpu  # sobre un teléfono ya arrancado
#   BOOT_ONLY=1 qemu-bench.sh N gpu  # solo el tiempo de arranque
#
# Salida: líneas "clave valor" (boot_s, rss_mib, pss_mib, ui_fps, ui_cores,
# ui_gpu_pct, webgl_fps, webgl_cores, webgl_gpu_pct, renderer...).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
N="${1:?usage: $0 N gpu|cpu}"
MODE="${2:?usage: $0 N gpu|cpu}"
WORK="${WORK:-/root/gpu-poc}"
D="$WORK/$N"
SECS="${SECS:-30}"
Q="$HERE/qemu-phone.sh"

out() { echo "$1 $2"; }
A() { ip netns exec "gp$N" timeout 60 adb -s 172.16.0.2:5555 "$@" </dev/null; }
pid() { cat "$D/qemu.pid"; }
ticks() { awk '{print $14 + $15}' "/proc/$(pid)/stat"; }
# ns de motor render del i915 sumados por cliente DRM (cada cliente una vez).
gpu_ns() {
  local f
  for f in /proc/"$(pid)"/fdinfo/*; do
    grep -q '^drm-driver:[[:space:]]*i915' "$f" 2>/dev/null || continue
    awk '/^drm-client-id:/{id=$2} /^drm-engine-render:/{ns=$2} END{if(id!="")print id, ns}' "$f"
  done | awk '$2 > m[$1] {m[$1] = $2} END {for (k in m) s += m[k]; printf "%.0f\n", s}'
}
# mide "$@" durante su ejecución: núcleos medios y % de GPU
measure() {
  local k0 g0 t0 k1 g1 t1
  k0=$(ticks) g0=$(gpu_ns) t0=$(date +%s.%N)
  "$@" >/dev/null 2>&1 || true
  k1=$(ticks) g1=$(gpu_ns) t1=$(date +%s.%N)
  echo "$(echo "($k1 - $k0) / 100 / ($t1 - $t0)" | bc -l) $(echo "($g1 - $g0) / 10000000 / ($t1 - $t0)" | bc -l)"
}

if [ "${SKIP_UP:-0}" != 1 ]; then
  "$Q" up "$N" "$MODE" >/dev/null
fi
(ip netns exec "gp$N" adb start-server </dev/null >/dev/null 2>&1) || true

# ── arranque: de lanzar QEMU a sys.boot_completed=1 (sondeo cada 0,5 s) ──
end=$(($(date +%s) + 300))
until [ "$(A shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = 1 ]; do
  [ "$(date +%s)" -lt "$end" ] || { echo "boot timeout" >&2; exit 1; }
  ip netns exec "gp$N" timeout 2 adb connect 172.16.0.2:5555 </dev/null >/dev/null 2>&1 || true
  sleep 0.5
done
out boot_s "$(printf '%.1f' "$(echo "$(date +%s.%N) - $(cat "$D/t0")" | bc -l)")"
out renderer "$(A shell dumpsys SurfaceFlinger | grep -m1 '^GLES' | tr -d '\r' | cut -c1-120 | tr ' ' '_')"
[ "${BOOT_ONLY:-0}" != 1 ] || exit 0

# pantalla encendida, sin bloqueo ni animaciones de ventana (como prepScript)
A shell 'svc power stayon true; input keyevent KEYCODE_WAKEUP; wm dismiss-keyguard;
  settings put system screen_off_timeout 2147483647' >/dev/null 2>&1 || true
sleep "${SETTLE:-30}"

# ── memoria y reposo ──
out rss_mib "$(awk '/^VmRSS/{print int($2/1024)}' "/proc/$(pid)/status")"
out pss_mib "$(awk '/^Pss:/{print int($2/1024)}' "/proc/$(pid)/smaps_rollup")"
read -r c g < <(measure sleep 10)
out idle_cores "$(printf '%.2f' "$c")"

# ── UI: arrastre continuo arriba/abajo en Ajustes ──
A shell am start -W -n com.android.settings/.Settings >/dev/null
sleep 3
A shell dumpsys gfxinfo com.android.settings reset >/dev/null
A shell dumpsys SurfaceFlinger --timestats -enable -clear >/dev/null
drag() {
  A shell "end=\$((\$(date +%s) + $SECS)); while [ \$(date +%s) -lt \$end ]; do
    input swipe 360 1050 360 350 500; input swipe 360 350 360 1050 500; done"
}
read -r c g < <(measure drag)
gi="$(A shell dumpsys gfxinfo com.android.settings | tr -d '\r')"
frames="$(sed -n 's/^Total frames rendered: //p' <<<"$gi" | head -1)"
out ui_fps "$(printf '%.1f' "$(echo "${frames:-0} / $SECS" | bc -l)")"
out ui_janky "$(sed -n 's/^Janky frames: //p' <<<"$gi" | head -1 | tr ' ' '_')"
out ui_p90_ms "$(sed -n 's/^90th percentile: //p' <<<"$gi" | head -1)"
out ui_sf_fps "$(A shell dumpsys SurfaceFlinger --timestats -dump | tr -d '\r' 2>/dev/null |
  awk '/layerName = .*[Ss]ettings/{f=1} f && /averageFPS/{print $3; exit}' || true)"
out ui_cores "$(printf '%.2f' "$c")"
out ui_gpu_pct "$(printf '%.1f' "$g")"
A shell dumpsys SurfaceFlinger --timestats -disable >/dev/null 2>&1 || true

# ── WebGL: la página de este directorio servida desde el CT ──
pkill -f "http.server 8000 --bind 172.16.0.1" 2>/dev/null || true
ip netns exec "gp$N" python3 -m http.server 8000 --bind 172.16.0.1 -d "$HERE" </dev/null >/dev/null 2>&1 &
web=$!
trap 'kill $web 2>/dev/null || true' EXIT
sleep 1
# El Chromium de WebView pone virgl en su lista negra de GPU ("WebGL1
# blocklisted"): sin esta bandera no hay contexto WebGL. Redroid es debuggable,
# así que WebView lee /data/local/tmp/webview-command-line.
A shell "echo '_ --ignore-gpu-blocklist' > /data/local/tmp/webview-command-line;
  chmod 644 /data/local/tmp/webview-command-line; am force-stop org.chromium.webview_shell" >/dev/null
A shell am start -W -a android.intent.action.VIEW -d "http://172.16.0.1:8000/webgl-raymarch.html?passes=${PASSES:-1}" \
  -n org.chromium.webview_shell/.WebViewBrowserActivity >/dev/null
sleep 12 # compilar el shader y calentar
info="$(A logcat -d | tr -d '\r' | grep -o 'WEBGLINFO.*' | tail -1 | cut -d'"' -f1 | tr ' ' '_' || true)"
A logcat -c
read -r c g < <(measure sleep "$SECS")
lc="$(A logcat -d | tr -d '\r')"
out webgl_info "$info"
out webgl_passes "${PASSES:-1}"
out webgl_fps "$( (grep -o 'WEBGLFPS [0-9.]*' <<<"$lc" || true) | awk '{s += $2; n++} END {if (n) printf "%.1f", s / n; else print "n/a"}')"
out webgl_cores "$(printf '%.2f' "$c")"
out webgl_gpu_pct "$(printf '%.1f' "$g")"
A shell am force-stop org.chromium.webview_shell >/dev/null 2>&1 || true
