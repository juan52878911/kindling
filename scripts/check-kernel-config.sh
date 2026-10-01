#!/usr/bin/env bash
# Verifica que un .config de kernel sirve para un invitado de kindling.
#
#   scripts/check-kernel-config.sh <.config>              # arquitectura según el .config
#   scripts/check-kernel-config.sh --arch arm64 <.config>
#   zcat /proc/config.gz | scripts/check-kernel-config.sh -   # el kernel en marcha
#
# Sale con 0 si todo cuadra, 1 si falta una opción obligatoria o sobra una
# prohibida (las lista todas, no solo la primera) y 2 si se usa mal.
#
# OBLIGATORIAS son lo que el invitado usa de verdad, no "lo razonable":
#   - virtio por MMIO: disco (vda/vdb/volúmenes), red, vsock, globo, entropía;
#   - ext4 + overlayfs (scripts/overlay-init.sh y minimal-init.sh montan la base
#     de solo lectura bajo un overlay), devtmpfs automontado (overlay-init no
#     monta /dev y usa /dev/vdb), FUSE (pkg/guest/fuse_linux.go, carpetas
#     compartidas), PTYs Unix98 (pkg/guest/pty_linux.go monta devpts);
#   - dm-verity: las capas con "verity": true se montan por /dev/mapper y,
#     sin él, el init se niega a montarlas (internal/imagen, verityBlock);
#   - ip= por línea de comandos (IP_PNP): la red la configura el kernel, sin
#     herramientas en la imagen (internal/net/net.go);
#   - consola 8250 (console=ttyS0 en internal/machine/manager.go);
#   - lo que necesita systemd en las imágenes que lo arrancan (cgroups, fhandle,
#     inotify, signalfd...). El agente en sí no toca cgroups: todo el control
#     de recursos lo hace el anfitrión.
#
# PROHIBIDAS son superficie que el invitado no usa: módulos, i8042 y teclado o
# ratón PS/2, kexec, io_uring, BPF sin privilegios, sonido/USB/DRM y drivers de
# hardware real. PCI solo se prohíbe en amd64: en arm64 el backend vz (macOS)
# pone los virtio en PCI (vz/internal/spec/spec.go, TranslateBootArgs).
#
# Un nombre "A|B" acepta cualquiera de los dos: varias opciones de mitigación se
# renombraron (RETPOLINE → MITIGATION_RETPOLINE en 6.9) y así el mismo script
# vale para 6.1 y para kernels posteriores.
set -euo pipefail

usage() { echo "uso: $0 [--arch amd64|arm64] <.config|->" >&2; exit 2; }

ARCH=""
while [ $# -gt 0 ]; do
  case "$1" in
    --arch) [ $# -ge 2 ] || usage; ARCH="$2"; shift 2 ;;
    -h|--help) usage ;;
    --) shift; break ;;
    -?*) usage ;;
    *) break ;;
  esac
done
[ $# -eq 1 ] || usage
CONFIG="$1"

if [ "$CONFIG" = "-" ]; then
  cfg="$(cat)"
else
  [ -r "$CONFIG" ] || { echo "cannot read $CONFIG" >&2; exit 2; }
  cfg="$(cat "$CONFIG")"
fi

# valor devuelve el valor de CONFIG_$1 ("y", "m", una cadena...) o "n" si no
# está puesto o no existe.
declare -A VAL=()
while IFS= read -r line; do
  case "$line" in
    CONFIG_*=*) k="${line%%=*}"; VAL["${k#CONFIG_}"]="${line#*=}" ;;
  esac
done <<<"$cfg"
valor() { printf '%s' "${VAL[$1]:-n}"; }

if [ -z "$ARCH" ]; then
  if [ "$(valor X86_64)" = y ]; then ARCH=amd64
  elif [ "$(valor ARM64)" = y ]; then ARCH=arm64
  else echo "cannot infer the architecture from $CONFIG; pass --arch" >&2; exit 2
  fi
fi
case "$ARCH" in amd64|arm64) ;; *) usage ;; esac

REQUIRED_COMMON=(
  # núcleo: sin esto ni glibc ni systemd arrancan
  MULTIUSER FUTEX EPOLL SIGNALFD TIMERFD EVENTFD SHMEM FHANDLE POSIX_TIMERS
  FILE_LOCKING SYSVIPC INOTIFY_USER BINFMT_ELF BINFMT_SCRIPT PRINTK BLOCK
  # sandbox dentro del invitado (systemd, runtimes de lenguajes)
  SECCOMP SECCOMP_FILTER
  # cgroups que monta systemd; el agente no usa ninguno
  CGROUPS MEMCG CGROUP_PIDS
  # virtio por MMIO (Firecracker)
  VIRTIO VIRTIO_MMIO VIRTIO_BLK VIRTIO_NET VSOCKETS VIRTIO_VSOCKETS
  VIRTIO_BALLOON HW_RANDOM_VIRTIO
  # las estadísticas del globo salen de estos contadores
  VM_EVENT_COUNTERS
  # red configurada por el kernel con ip=
  NET UNIX INET IP_PNP
  # sistemas de ficheros
  EXT4_FS OVERLAY_FS FUSE_FS TMPFS PROC_FS SYSFS DEVTMPFS DEVTMPFS_MOUNT
  # capas con verity (internal/imagen): sin dm-verity el init se para
  BLK_DEV_DM DM_VERITY CRYPTO_SHA256
  # consola
  TTY UNIX98_PTYS SERIAL_8250 SERIAL_8250_CONSOLE
)
# shellcheck disable=SC2034  # se leen por nombre: REQUIRED_${ARCH}
REQUIRED_amd64=(
  X86_64 SMP HYPERVISOR_GUEST PARAVIRT KVM_GUEST
  # Firecracker declara los virtio-mmio en la línea de comandos
  VIRTIO_MMIO_CMDLINE_DEVICES
  # el menú de las mitigaciones: sin él allnoconfig apaga PTI, RETPOLINE...
  # (SPECULATION_MITIGATIONS antes, CPU_MITIGATIONS en 6.1.140 y en 6.9+)
  "SPECULATION_MITIGATIONS|CPU_MITIGATIONS"
  "PAGE_TABLE_ISOLATION|MITIGATION_PAGE_TABLE_ISOLATION"
  "RETPOLINE|MITIGATION_RETPOLINE"
)
# shellcheck disable=SC2034
REQUIRED_arm64=(
  ARM64 SMP OF
  # el 8250 de Firecracker en arm64 se describe en el device tree
  SERIAL_OF_PLATFORM
  # vz: virtio por PCI y consola hvc0
  PCI VIRTIO_PCI VIRTIO_CONSOLE
)

FORBIDDEN_COMMON=(
  MODULES
  KEXEC KEXEC_FILE
  IO_URING
  SERIO SERIO_I8042 KEYBOARD_ATKBD MOUSE_PS2 INPUT_MOUSEDEV
  SOUND SND
  USB_SUPPORT USB
  DRM FB
  # hardware real
  ETHERNET WLAN WIRELESS BT ATA SCSI MEDIA_SUPPORT NFC
)
# shellcheck disable=SC2034  # ídem: FORBIDDEN_${ARCH}
FORBIDDEN_amd64=(PCI)
# shellcheck disable=SC2034
FORBIDDEN_arm64=()

fallos=0
falla() { echo "  FAIL: $*"; fallos=$((fallos + 1)); }

# Obligatoria: alguno de los nombres alternativos a "y". "m" no vale: sin
# módulos no hay dónde cargarlo.
requerida() {
  local alt nombre
  IFS='|' read -r -a alt <<<"$1"
  for nombre in "${alt[@]}"; do
    [ "$(valor "$nombre")" = y ] && return 0
  done
  falla "CONFIG_${alt[0]} must be y (is $(valor "${alt[0]}"))"
}

prohibida() {
  local v
  v="$(valor "$1")"
  [ "$v" = n ] || falla "CONFIG_$1 must not be set (is $v)"
}

req_arch="REQUIRED_${ARCH}[@]"
forb_arch="FORBIDDEN_${ARCH}[@]"
for o in "${REQUIRED_COMMON[@]}" "${!req_arch}"; do requerida "$o"; done
for o in "${FORBIDDEN_COMMON[@]}" "${!forb_arch}"; do prohibida "$o"; done

# BPF sin privilegios: o no hay bpf(2), o el sysctl arranca en "prohibido".
if [ "$(valor BPF_SYSCALL)" = y ] && [ "$(valor BPF_UNPRIV_DEFAULT_OFF)" != y ]; then
  falla "CONFIG_BPF_SYSCALL=y requires CONFIG_BPF_UNPRIV_DEFAULT_OFF=y (unprivileged BPF)"
fi

# Sin módulos, cualquier "=m" es una opción que no llega al invitado.
mods="$(grep -E '^CONFIG_[A-Za-z0-9_]+=m$' <<<"$cfg" | cut -d= -f1 | head -5 | tr '\n' ' ' || true)"
[ -z "$mods" ] || falla "options built as modules: ${mods}"

if [ "$fallos" -gt 0 ]; then
  echo "kernel config $CONFIG ($ARCH): $fallos problem(s)" >&2
  exit 1
fi
echo "kernel config $CONFIG ($ARCH): ok"
