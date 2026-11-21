#!/usr/bin/env bash
# Higiene entre variantes del banco (solo en la instancia del banco, como root; daemon-down.sh mata
# procesos por nombre, así que NUNCA en el laboratorio).
#   higiene.sh inicio     apunta el espacio libre del disco raíz
#   higiene.sh rm         kling rm -f de todas las máquinas del daemon del banco
#   higiene.sh verificar  avisa de interfaces vh-* y firecracker ajenos al banco; comprueba 0 firecracker, 0 netns kl-*, 0 montajes de cow.xfs y el mismo
#                         espacio libre (±100 MiB) que en `inicio`; si no, sale con 1 y el banco aborta
KB=${KB:-/opt/kbench}; LIBRE=$KB/run/libre-mib; TOL_MIB=${TOL_LIBRE_MIB:-100}
libre() { df -BM --output=avail / | tail -1 | tr -dc '0-9'; }
case "${1:-}" in
inicio) mkdir -p "$KB/run"; libre > "$LIBRE" ;;
rm)
  export KLING_HOST=unix:///run/kt-bench/kling.sock KLING_CONFIG=/srv/kt-bench/config.json
  K=/srv/kt-bench/bin/kling
  [ -x "$K" ] && "$K" ps -a -q 2>/dev/null | xargs -r "$K" rm -f >/dev/null 2>&1
  true ;;
verificar)
  fc=$(pgrep -xc firecracker); ns=$(ip netns list 2>/dev/null | grep -c '^kl-')
  mt=$(grep -c 'cow\.xfs' /proc/mounts); mt=$((mt + $(losetup -nO BACK-FILE 2>/dev/null | grep -c 'cow\.xfs')))
  # Ajenos al daemon del banco: avisan (no abortan; el host limpio lo decide quien lee el aviso).
  vh=$(ip -o link show 2>/dev/null | grep -c ' vh-')
  ajenos=$(pgrep -x firecracker | while read -r p; do tr '\0' ' ' < /proc/$p/cmdline 2>/dev/null | grep -q kt-bench || echo "$p"; done | wc -l)
  [ "$vh" = 0 ] || echo "higiene: AVISO $vh interfaces vh-* en el host" >&2
  [ "$ajenos" = 0 ] || echo "higiene: AVISO $ajenos procesos firecracker que no son del daemon del banco" >&2
  ahora=$(libre); antes=$(cat "$LIBRE" 2>/dev/null || echo "$ahora"); dif=$((ahora - antes)); dif=${dif#-}
  echo "higiene: firecracker=$fc netns=$ns cow.xfs=$mt libre=${ahora}MiB (inicio ${antes}MiB, dif ${dif})"
  if [ "$fc" != 0 ] || [ "$ns" != 0 ] || [ "$mt" != 0 ] || [ "$dif" -gt "$TOL_MIB" ]; then
    echo "higiene: ¡la variante dejó restos! el banco aborta para no contaminar la siguiente" >&2; exit 1
  fi ;;
*) echo "uso: higiene.sh inicio|rm|verificar" >&2; exit 2 ;;
esac
