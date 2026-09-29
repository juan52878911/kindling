#!/usr/bin/env bash
# Comprobador del perfil "gpu" (config-android-gpu). Lo encadena
# `ANDROID_PROFILE=gpu kernel/build.sh`.
#
#   check-android-gpu-config.sh <.config|->
#
# El comprobador del núcleo (scripts/check-kernel-config.sh) prohíbe DRM y FB
# para todo invitado de kindling, y con razón. Este perfil los necesita, así
# que build.sh le pasa al del núcleo el .config SIN esas dos líneas (todo lo
# demás lo sigue mirando él: USB, sonido, módulos...) y aquí se exige lo que
# el perfil añade y que no se cuele nada más del subsistema gráfico.
set -euo pipefail

[ $# -eq 1 ] || { echo "uso: $0 <.config|->" >&2; exit 2; }
if [ "$1" = "-" ]; then cfg="$(cat)"; else cfg="$(cat "$1")"; fi

valor() {
  local l
  l="$(grep -E "^CONFIG_$1=" <<<"$cfg" | tail -1 || true)"
  [ -n "$l" ] && echo "${l#*=}" || echo n
}

fallos=0
for o in DRM DRM_VIRTIO_GPU DRM_FBDEV_EMULATION FB; do
  [ "$(valor "$o")" = y ] || { echo "  FAIL: CONFIG_$o must be y (is $(valor "$o"))"; fallos=$((fallos + 1)); }
done
# fbcon pintaría encima del espejo; VT es lo que lo arrastra.
for o in FRAMEBUFFER_CONSOLE VT DRM_LEGACY; do
  [ "$(valor "$o")" = n ] || { echo "  FAIL: CONFIG_$o must not be set (is $(valor "$o"))"; fallos=$((fallos + 1)); }
done
# Ningún otro driver DRM que el de virtio: cualquier otro es hardware real.
otros="$(grep -E '^CONFIG_DRM_[A-Z0-9_]+=y$' <<<"$cfg" | cut -d= -f1 |
  grep -v -E '^CONFIG_DRM_(VIRTIO_GPU|VIRTIO_GPU_KMS|FBDEV_EMULATION|KMS_HELPER|GEM_SHMEM_HELPER|PANEL|BRIDGE|PANEL_BRIDGE|PANEL_ORIENTATION_QUIRKS|DISPLAY_HELPER|TTM|TTM_HELPER|EXEC|SUBALLOC_HELPER|FBDEV_LEAK_PHYS_SMEM|NOMODESET)$' |
  tr '\n' ' ' || true)"
[ -z "$otros" ] || { echo "  FAIL: unexpected DRM options: $otros"; fallos=$((fallos + 1)); }

if [ "$fallos" -gt 0 ]; then
  echo "android gpu kernel config: $fallos problem(s)" >&2
  exit 1
fi
echo "android gpu kernel config: ok"
