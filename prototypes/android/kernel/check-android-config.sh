#!/usr/bin/env bash
# Comprueba que un .config de kernel sirve para arrancar Redroid (Android 13,
# 64 bits) dentro de una microVM de kindling. Complementa, no sustituye, a
# scripts/check-kernel-config.sh (el del núcleo, que sigue mandando sobre lo
# que un invitado de kindling debe o no llevar).
#
#   check-android-config.sh <.config>
#   zcat /proc/config.gz | check-android-config.sh -     # dentro del invitado
#
# Sale con 0 si están todas las OBLIGATORIAS, 1 si falta alguna (las lista
# todas) y 2 si se usa mal. Las RECOMENDADAS solo avisan.
#
# OBLIGATORIAS: lo que Redroid documenta como imprescindible (binderfs,
# memfd, IPv6, DMA-BUF heaps, páginas de 4 KiB; redroid-doc/deploy/README.md)
# y lo que usa el lanzador de este prototipo (espacios de nombres, cgroups).
# RECOMENDADAS: lo que Android usa y no está documentado por Redroid como
# imprescindible (netd, lmkd, entrada...). Si una recomendada resulta hacer
# falta en la fase 0, se sube a obligatoria.
#
# Va aparte del comprobador del núcleo a propósito: en la fase 0 no se toca
# nada fuera de prototypes/android/ (ver README).
set -euo pipefail

usage() { echo "uso: $0 <.config|->" >&2; exit 2; }
[ $# -eq 1 ] || usage
case "$1" in -h|--help) usage ;; esac

if [ "$1" = "-" ]; then
  cfg="$(cat)"
else
  [ -r "$1" ] || { echo "cannot read $1" >&2; exit 2; }
  cfg="$(cat "$1")"
fi

declare -A VAL=()
while IFS= read -r line; do
  case "$line" in
    CONFIG_*=*) k="${line%%=*}"; VAL["${k#CONFIG_}"]="${line#*=}" ;;
  esac
done <<<"$cfg"
valor() { printf '%s' "${VAL[$1]:-n}"; }

REQUIRED=(
  # binder y binderfs [redroid]
  ANDROID_BINDER_IPC ANDROID_BINDERFS
  # memfd en lugar de ashmem [redroid]; ashmem no existe en 6.1
  MEMFD_CREATE
  # DMA-BUF heaps en lugar de ION [redroid]
  DMABUF_HEAPS DMABUF_HEAPS_SYSTEM
  # IPv6 [redroid]
  IPV6
  # /proc/sys/vm/mmap_rnd_compat_bits: el init de Android 13 aborta sin él (fase 0)
  COMPAT
  # el lanzador: unshare -m -p -i -u -n y chroot
  NAMESPACES PID_NS IPC_NS UTS_NS NET_NS
  # cgroups que monta el init de Android (cgroups.json) y el v2 que prepara el lanzador
  CGROUPS MEMCG CPUSETS CGROUP_SCHED
  # BPF: bpfloader y netd adjuntan programas a cgroups
  BPF_SYSCALL CGROUP_BPF
  # almacenamiento emulado (/sdcard) va por FUSE desde Android 11
  FUSE_FS
  # pseudo-sistemas que monta el lanzador
  PROC_FS SYSFS TMPFS DEVTMPFS UNIX98_PTYS POSIX_MQUEUE
  # el init de Android usa epoll, signalfd, timerfd, eventfd, futex
  EPOLL SIGNALFD TIMERFD EVENTFD FUTEX
)
# 4 KiB de página [redroid, extra arm64]; en x86_64 la página siempre es de 4 KiB.
[ "$(valor ARM64)" = y ] && REQUIRED+=(ARM64_4K_PAGES)
RECOMMENDED=(
  # lmkd
  PSI
  # compositor y asignador gráfico
  SYNC_FILE UDMABUF
  # entrada (input tap/swipe se inyecta por InputManager; uinput por si acaso)
  INPUT INPUT_EVDEV INPUT_UINPUT
  # init/keystore
  KEYS
  # netd: iptables, políticas de enrutamiento, tc+BPF, SOCK_DESTROY
  NETFILTER NETFILTER_XTABLES IP_NF_IPTABLES IP_NF_FILTER IP_NF_MANGLE IP_NF_RAW
  IP6_NF_IPTABLES IP6_NF_FILTER IP6_NF_MANGLE IP6_NF_RAW
  NETFILTER_XT_MATCH_BPF NETFILTER_XT_MATCH_OWNER NETFILTER_XT_TARGET_IDLETIMER
  IP_MULTIPLE_TABLES IPV6_MULTIPLE_TABLES
  NET_SCH_INGRESS NET_CLS_BPF NET_ACT_BPF INET_DIAG_DESTROY
  # APEX sin aplanar
  BLK_DEV_LOOP
  # VpnService
  TUN
  # dm-verity sobre la capa (docs/verity.md)
  BLK_DEV_DM DM_VERITY DM_VERITY_FEC CRYPTO_SHA256
)

fallos=0
avisos=0
for o in "${REQUIRED[@]}"; do
  v="$(valor "$o")"
  if [ "$v" != y ]; then
    echo "  FAIL: CONFIG_$o must be y (is $v)"
    fallos=$((fallos + 1))
  fi
done
for o in "${RECOMMENDED[@]}"; do
  v="$(valor "$o")"
  if [ "$v" != y ]; then
    echo "  WARN: CONFIG_$o is recommended for Android (is $v)"
    avisos=$((avisos + 1))
  fi
done

# Los tres nodos de binder que espera Android.
devs="$(valor ANDROID_BINDER_DEVICES)"
devs="${devs#\"}"; devs="${devs%\"}"
for d in binder hwbinder vndbinder; do
  case ",$devs," in
    *",$d,"*) ;;
    *) echo "  FAIL: CONFIG_ANDROID_BINDER_DEVICES must include $d (is \"$devs\")"
       fallos=$((fallos + 1)) ;;
  esac
done

# PSI compilado pero apagado por defecto no le sirve a lmkd sin psi=1.
if [ "$(valor PSI)" = y ] && [ "$(valor PSI_DEFAULT_DISABLED)" = y ]; then
  echo "  WARN: CONFIG_PSI_DEFAULT_DISABLED=y: lmkd will not see /proc/pressure unless the cmdline says psi=1"
  avisos=$((avisos + 1))
fi

# Sin módulos, "=m" no llega al invitado (igual que el comprobador del núcleo).
mods="$(grep -E '^CONFIG_[A-Za-z0-9_]+=m$' <<<"$cfg" | cut -d= -f1 | head -5 | tr '\n' ' ' || true)"
if [ -n "$mods" ]; then
  echo "  FAIL: options built as modules (kindling has no modules): ${mods}"
  fallos=$((fallos + 1))
fi

src="$1"; [ "$src" = "-" ] && src="(stdin)"
if [ "$fallos" -gt 0 ]; then
  echo "android kernel config $src: $fallos problem(s), $avisos warning(s)" >&2
  exit 1
fi
echo "android kernel config $src: ok ($avisos warning(s))"
