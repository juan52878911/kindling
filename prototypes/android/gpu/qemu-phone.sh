#!/usr/bin/env bash
# Prueba de concepto del issue #95: el teléfono Android de kindling (la misma
# imagen por capas de Firecracker, sin reconstruir) en una VM de QEMU `microvm`
# con virtio-gpu, para medir virgl frente a SwiftShader en la MISMA VM.
# docs/gpu.md, "Prueba de concepto: virgl en una VM".
#
# Corre dentro de un CT LXC privilegiado con /dev/kvm, /dev/dri y /dev/net/tun
# (en pve: el CT 106 `phones`), nunca en el host. Necesita en el CT QEMU >= 10
# (en Debian 12, de bookworm-backports; el 7.2 de bookworm no sirve, ver
# docs/gpu.md) y Mesa para el iGPU:
#   apt install --no-install-recommends -t bookworm-backports qemu-system-x86 \
#     qemu-system-gui qemu-system-modules-opengl
#   apt install --no-install-recommends libgl1-mesa-dri libegl1 libgbm1 adb python3 bc
# y el kernel Android con el perfil gpu (DRM_VIRTIO_GPU, virtio-mmio, PVH):
#   TARGET_ARCH=amd64 ANDROID_PROFILE=gpu KERNEL_SHA256=... prototypes/android/kernel/build.sh
#
#   qemu-phone.sh up   N gpu|cpu    # arranca el teléfono N (netns gpN, tap0, 172.16.0.2)
#   qemu-phone.sh down N
#   qemu-phone.sh adb  N <args...>  # adb contra el teléfono N (servidor adb en su netns)
#   qemu-phone.sh monitor N "savevm s1"   # una orden del monitor de QEMU
#
# Qué hace: los tres discos de una máquina de kindling (base ro, overlay
# escribible copiado del molde, capa Android ro, en vda/vdb/vdc), la línea de
# kernel de internal/machine (bootArgs) y, en la overlay, un android.conf y el
# android-launch.sh de este árbol: ANDROID_GPU_MODE=host + ANDROID_GPU_NODE
# para gpu, ANDROID_FPS=60 en los dos. Cada teléfono en su netns con la red de
# siempre de kindling (172.16.0.1/30), así que no chocan entre sí.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
IMG="${IMG:-/var/lib/kindling/images}"
BASE="${BASE:-$IMG/android-base.ext4}"
LAYER="${LAYER:-$IMG/android13.layer.ext4}"
TEMPLATE="${TEMPLATE:-$IMG/overlay-template.ext4}"
KERNEL="${KERNEL:-/root/android-kernel-gpu/vmlinux-6.1.140-kindling-amd64-android-gpu}"
WORK="${WORK:-/root/gpu-poc}"
CPUS="${CPUS:-4}"
MEM="${MEM:-2048}"
RENDER="${RENDER:-/dev/dri/renderD128}"

# La de internal/machine (bootArgsBase + archBootArg + net.BootArg con la pila
# IPv6 cargada + layerBootArg). Sin kling.exec: aquí no hay vsock ni agente.
CMDLINE="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/sbin/overlay-init ${QUIET-quiet}"
CMDLINE+=" sysctl.vm.compaction_proactiveness=0 sysctl.vm.stat_interval=10"
CMDLINE+=" i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
CMDLINE+=" ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off ipv6.disable_ipv6=1 kling.layer=/dev/vdc"

die() { echo "qemu-phone: $*" >&2; exit 1; }

up() {
  local n="$1" mode="$2" d="$WORK/$1" ns="gp$1"
  case "$mode" in gpu | cpu) ;; *) die "mode must be gpu or cpu" ;; esac
  [ -e "$d/qemu.pid" ] && kill -0 "$(cat "$d/qemu.pid")" 2>/dev/null && die "phone $n already running"
  mkdir -p "$d"
  cp --sparse=always "$TEMPLATE" "$d/overlay.ext4"
  local mnt="$d/mnt"
  mkdir -p "$mnt"
  mount -o loop "$d/overlay.ext4" "$mnt"
  local lib="$mnt/upper/usr/local/lib/kindling-android"
  mkdir -p "$lib"
  install -m755 "$HERE/../image/android-launch.sh" "$lib/android-launch.sh"
  # android.conf de la capa + lo de esta prueba (lo lee `source`: gana lo último)
  local lmnt="$d/layer"
  mkdir -p "$lmnt"
  mount -o ro,loop,noload "$LAYER" "$lmnt"
  cp "$lmnt/upper/usr/local/lib/kindling-android/android.conf" "$lib/android.conf"
  umount "$lmnt"
  {
    echo "# qemu-phone.sh ($mode)"
    echo "ANDROID_FPS=60"
    echo "ANDROID_EXTRA_ARGS="
    if [ "$mode" = gpu ]; then
      echo "ANDROID_GPU_MODE=host"
      echo "ANDROID_GPU_NODE=/dev/dri/renderD128"
    fi
    # HOOK=script: se lanza una vez desde el lanzador (no desde android-sh, que
    # también lee este fichero) con la salida a la consola serie. Sirve para
    # diagnosticar sin agente ni adb (aquí no hay vsock).
    if [ -n "${HOOK:-}" ]; then
      echo '[ "${0##*/}" = android-launch.sh ] && [ ! -e /run/poc-hook ] && { : >/run/poc-hook; setsid bash /usr/local/lib/kindling-android/poc-hook.sh </dev/null >/dev/ttyS0 2>&1 & } || true'
    fi
  } >>"$lib/android.conf"
  [ -z "${HOOK:-}" ] || install -m755 "$HOOK" "$lib/poc-hook.sh"
  umount "$mnt"

  ip netns add "$ns" 2>/dev/null || true
  ip netns exec "$ns" ip link set lo up
  ip netns exec "$ns" ip tuntap add tap0 mode tap 2>/dev/null || true
  ip netns exec "$ns" ip addr replace 172.16.0.1/30 dev tap0
  ip netns exec "$ns" ip link set tap0 up

  local gpu=(-display none -device "virtio-gpu-device,xres=720,yres=1280")
  if [ "$mode" = gpu ]; then
    gpu=(-display "egl-headless,rendernode=$RENDER" -device "virtio-gpu-gl-device,xres=720,yres=1280")
    # VENUS=1: Vulkan por venus (blob + memoria del invitado en memfd, como pide
    # QEMU). Necesita Vulkan en el CT (mesa-vulkan-drivers). Ver docs/gpu.md.
    if [ "${VENUS:-0}" = 1 ]; then
      gpu=(-display "egl-headless,rendernode=$RENDER"
        -object "memory-backend-memfd,id=mem,size=${MEM}M,share=on" -machine memory-backend=mem
        -device "virtio-gpu-gl-device,xres=720,yres=1280,blob=on,venus=on,hostmem=${HOSTMEM:-1G}")
    fi
  fi
  date +%s.%N >"$d/t0"
  # Sin -daemonize (QEMU se bifurcaría con EGL ya iniciado): nohup y pidfile.
  # force-legacy=false: virtio-mmio de QEMU es legacy (v1) por defecto y
  # virtio-gpu exige VIRTIO_F_VERSION_1; sin esto el driver no se ata y no hay
  # /dev/dri. Firecracker ya habla virtio-mmio v2.
  nohup ip netns exec "$ns" qemu-system-x86_64 \
    -M microvm,acpi=off,pcie=off \
    -enable-kvm -cpu host -smp "$CPUS" -m "$MEM" \
    -nodefaults -no-user-config -pidfile "$d/qemu.pid" \
    -global virtio-mmio.force-legacy=false \
    -serial "file:$d/console.log" -monitor "unix:$d/monitor.sock,server=on,wait=off" \
    -kernel "$KERNEL" -append "$CMDLINE" \
    -drive id=b,file="$BASE",format=raw,if=none,readonly=on -device virtio-blk-device,drive=b \
    -drive id=o,file="$d/overlay.ext4",format=raw,if=none -device virtio-blk-device,drive=o \
    -drive id=l,file="$LAYER",format=raw,if=none,readonly=on -device virtio-blk-device,drive=l \
    -netdev tap,id=n,ifname=tap0,script=no,downscript=no -device virtio-net-device,netdev=n \
    "${gpu[@]}" >"$d/qemu.log" 2>&1 </dev/null &
  for _ in $(seq 50); do [ -s "$d/qemu.pid" ] && break; sleep 0.1; done
  [ -s "$d/qemu.pid" ] || die "qemu did not start: $(tail -3 "$d/qemu.log")"
  echo "$mode" >"$d/mode"
  echo "phone $n ($mode): qemu pid $(cat "$d/qemu.pid"), console $d/console.log"
}

down() {
  local n="$1" d="$WORK/$1"
  [ -e "$d/qemu.pid" ] && kill "$(cat "$d/qemu.pid")" 2>/dev/null || true
  rm -f "$d/qemu.pid"
  ip netns exec "gp$n" adb kill-server >/dev/null 2>&1 || true
  ip netns del "gp$n" 2>/dev/null || true
}

phone_adb() {
  local n="$1"
  shift
  ip netns exec "gp$n" adb -s 172.16.0.2:5555 "$@"
}

# monitor N "orden": una orden del monitor HMP de QEMU (p. ej. "savevm s1").
monitor() {
  python3 - "$WORK/$1/monitor.sock" "$2" <<'PY'
import socket, sys, time
s = socket.socket(socket.AF_UNIX); s.connect(sys.argv[1]); s.settimeout(60)
def leer():
    b = b""
    while not b.endswith(b"(qemu) "):
        b += s.recv(4096)
    return b.decode(errors="replace")
leer()
s.sendall(sys.argv[2].encode() + b"\n")
print(leer().replace("(qemu) ", "").strip())
PY
}

cmd="${1:-}"
case "$cmd" in
  up) [ $# -eq 3 ] || die "usage: $0 up N gpu|cpu"; up "$2" "$3" ;;
  down) [ $# -eq 2 ] || die "usage: $0 down N"; down "$2" ;;
  adb) [ $# -ge 3 ] || die "usage: $0 adb N <args>"; n="$2"; shift 2; phone_adb "$n" "$@" ;;
  monitor) [ $# -eq 3 ] || die "usage: $0 monitor N <hmp command>"; monitor "$2" "$3" ;;
  *) die "usage: $0 up|down|adb|monitor ..." ;;
esac
