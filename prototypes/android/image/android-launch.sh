#!/bin/bash
# Lanzador de Android (Redroid) DENTRO de la microVM, sin Docker. Fase 0.
#
# Lo arranca el /entrypoint que genera 81-base-image.sh como SERVICE: en
# segundo plano, relanzado si muere, ANTES de ceder el PID 1 a kling-guest
# (el mismo patrón que llama-server en las imágenes VON, docs/von.md). Así
# kling-guest sigue siendo PID 1 de la VM: sirve exec/cp, resync tras
# restaurar y el puerto 8080 que `kling save` exige. Su salida va a
# /var/log/service.log.
#
# Qué hace, en el orden en que lo haría `docker run --privileged` con la
# imagen de Redroid:
#   1. comprueba el kernel (binderfs, memfd, PSI...) y se para con un mensaje
#      claro si falta algo, en vez de dejar que Android muera sin decir por qué;
#   2. crea espacios de nombres nuevos de montajes, PIDs, IPC y UTS (y de red
#      si ANDROID_NET=isolated) con unshare, y dentro:
#   3. monta en el rootfs /proc, /sys (rw), cgroup2, un /dev en tmpfs con los
#      nodos del invitado, devpts, /dev/shm, mqueue y, si el init.rc de la
#      imagen no lo hace, binderfs;
#   4. chroot al rootfs y exec de /init con los argumentos de la imagen más
#      los nuestros (pantalla fija, render por software, memfd, sin asistente).
#      /init queda como PID 1 de su espacio de PIDs: es lo que espera.
#
# Uso: lo lanza el entrypoint sin argumentos. `android-launch.sh --stage2` es
# la parte que corre ya dentro de los espacios nuevos (no llamarla a mano).
set -euo pipefail

CONF=/usr/local/lib/kindling-android/android.conf
ARGS_FILE=/usr/local/lib/kindling-android/entrypoint.args
CHECK=/usr/local/lib/kindling-android/check-android-config.sh
STATE_DIR=/run/kindling-android
# shellcheck source=/dev/null
. "$CONF"
R="${ANDROID_ROOT:?}"

log() { echo "android-launch: $*"; }
estado() { mkdir -p "$STATE_DIR"; printf '%s\n' "$*" >"$STATE_DIR/state"; }

# Un fallo que no se arregla relanzando (falta algo en el kernel) no debe
# convertirse en un bucle de un segundo que llene el log: se deja el motivo en
# $STATE_DIR/state, que fase0.sh lee, y se duerme.
fatal() {
  log "FATAL: $*"
  estado "failed: $*"
  sleep 3600
  exit 1
}

stage1() {
  estado "starting"
  log "rootfs $R, pantalla ${ANDROID_WIDTH}x${ANDROID_HEIGHT}@${ANDROID_DPI}dpi, /data en $ANDROID_DATA_MODE, red $ANDROID_NET"
  # /init es un enlace absoluto (/init -> /system/bin/init): se resuelve
  # dentro del rootfs, que es donde lo resolverá chroot.
  local init_real="$R/init" t
  if [ -L "$init_real" ]; then
    t="$(readlink "$init_real")"
    case "$t" in /*) init_real="$R$t" ;; *) init_real="$R/$t" ;; esac
  fi
  [ -x "$init_real" ] || fatal "no $R/init: the image has no Android rootfs"
  for c in unshare chroot mount; do
    command -v "$c" >/dev/null || fatal "missing $c in the base image (util-linux)"
  done

  # ── 1. kernel ──────────────────────────────────────────────────────────────
  # binderfs aparece en /proc/filesystems como "binder".
  grep -qw binder /proc/filesystems ||
    fatal "kernel without binderfs (CONFIG_ANDROID_BINDERFS): boot this image with the kernel from prototypes/android/kernel"
  grep -qw cgroup2 /proc/filesystems || fatal "kernel without cgroup2"
  [ -e /proc/pressure/memory ] || log "aviso: sin PSI (/proc/pressure/memory): lmkd irá a ciegas"
  # El comprobador completo, si el kernel expone su .config (IKCONFIG_PROC=y
  # en config-common). Solo informa: las obligatorias ya se miraron arriba.
  if [ -r /proc/config.gz ] && [ -x "$CHECK" ]; then
    zcat /proc/config.gz | "$CHECK" - || log "aviso: el comprobador de Android encontró problemas (arriba)"
  fi

  local net=()
  [ "$ANDROID_NET" = isolated ] && net=(--net)
  # ── 2. espacios de nombres ─────────────────────────────────────────────────
  # --fork: el hijo es PID 1 del espacio de PIDs nuevo; exec de --stage2 en él.
  # --propagation private: nada de lo que se monte dentro sale a la VM.
  # Red aislada por defecto: si Android (netd, EthernetTracker) tocara eth0, las
  # rutas o iptables de la VM, kling-guest dejaría de ser alcanzable y con él
  # exec, save y los reenvíos. Con egress none no se pierde nada.
  estado "booting"
  # exec: este PID pasa a ser el de unshare, y su hijo el init de Android.
  # android-sh lo encuentra así.
  echo $$ >"$STATE_DIR/unshare.pid"
  # --kill-child: si unshare muere, Android muere con él y el bucle del
  # entrypoint no acaba con dos Android a la vez.
  exec unshare --mount --pid --fork --kill-child --ipc --uts "${net[@]}" --propagation private \
    -- "$0" --stage2
}

stage2() {
  # Aquí somos PID 1 de un espacio de PIDs nuevo, con montajes privados.
  cd /
  hostname localhost 2>/dev/null || true
  # En un espacio de red nuevo "lo" nace apagado; Docker lo levanta y el
  # init.rc de AOSP también ("ifup lo"), pero cuesta nada adelantarse.
  ip link set lo up 2>/dev/null || true
  mkdir -p "$R/proc" "$R/sys" "$R/dev"

  # ── 3. pseudo-sistemas en el rootfs, como los da Docker --privileged ─────
  mount -t proc proc "$R/proc"
  mount -t sysfs -o rw,nosuid,nodev,noexec sysfs "$R/sys"
  mkdir -p "$R/sys/fs/cgroup" 2>/dev/null || true
  mount -t cgroup2 -o nsdelegate cgroup2 "$R/sys/fs/cgroup" 2>/dev/null ||
    mount -t cgroup2 cgroup2 "$R/sys/fs/cgroup" || log "aviso: no se pudo montar cgroup2"

  # /dev: tmpfs propio con copia de los nodos del invitado (-x: sin cruzar a
  # devpts u otros montajes). Es lo que hace Docker; montar el devtmpfs del
  # invitado tal cual dejaría a Android crear sus sockets y enlaces en el /dev
  # de la VM, que es de kling-guest.
  mount -t tmpfs -o mode=0755,nosuid,size=65536k tmpfs "$R/dev"
  cp -ax /dev/. "$R/dev/" 2>/dev/null || true
  mkdir -p "$R/dev/pts" "$R/dev/shm" "$R/dev/mqueue"
  mount -t devpts -o newinstance,ptmxmode=0666,mode=0620,gid=5 devpts "$R/dev/pts"
  rm -f "$R/dev/ptmx"; ln -s pts/ptmx "$R/dev/ptmx"
  mount -t tmpfs -o mode=1777,nosuid,nodev tmpfs "$R/dev/shm"
  mount -t mqueue mqueue "$R/dev/mqueue" 2>/dev/null || true

  # binderfs: el init.rc de AOSP 12+ lo monta en early-init ("mount binder
  # binder /dev/binderfs" + enlaces + chmod 0666). Si esta imagen no lo hace,
  # se hace aquí igual que documenta Redroid para kernels con binderfs.
  if ! grep -qs 'mount binder binder /dev/binderfs' "$R/system/etc/init/hw/init.rc"; then
    mkdir -p "$R/dev/binderfs"
    mount -t binder binder "$R/dev/binderfs"
    for d in binder hwbinder vndbinder; do
      [ -e "$R/dev/binderfs/$d" ] || fatal "binderfs has no $d: CONFIG_ANDROID_BINDER_DEVICES must list it"
      chmod 0666 "$R/dev/binderfs/$d"
      ln -sf "binderfs/$d" "$R/dev/$d"
    done
  fi

  # /data: en el overlay de la VM (por defecto; lo que se escribe sobrevive a
  # save/fork y cuenta contra los 512 MiB del disco escribible de kindling) o
  # en tmpfs (cuenta contra la RAM del invitado; ver README).
  mkdir -p "$R/data"
  if [ "$ANDROID_DATA_MODE" = tmpfs ]; then
    mount -t tmpfs -o "mode=0771,size=$ANDROID_DATA_SIZE" tmpfs "$R/data"
    chown 1000:1000 "$R/data"   # system:system, como en la imagen
  fi

  # ── 4. argumentos de /init ────────────────────────────────────────────────
  # Los de la imagen (ENTRYPOINT sin argv[0], típicamente "qemu=1
  # androidboot.hardware=redroid") y los nuestros. Parámetros documentados en
  # redroid-doc/README.md, "Configuration".
  local args=()
  while IFS= read -r a; do [ -n "$a" ] && args+=("$a"); done <"$ARGS_FILE"
  args+=(
    "androidboot.redroid_width=$ANDROID_WIDTH"
    "androidboot.redroid_height=$ANDROID_HEIGHT"
    "androidboot.redroid_dpi=$ANDROID_DPI"
    "androidboot.redroid_fps=$ANDROID_FPS"
    # sin GPU: SwiftShader en CPU
    "androidboot.redroid_gpu_mode=guest"
    # 6.1 no tiene ashmem
    "androidboot.use_memfd=true"
    # AOSP sin asistente de configuración que bloquee la primera pantalla
    "ro.setupwizard.mode=DISABLED"
  )
  # shellcheck disable=SC2206 # separar por espacios es lo buscado
  [ -n "${ANDROID_EXTRA_ARGS:-}" ] && args+=($ANDROID_EXTRA_ARGS)
  log "exec /init ${args[*]}"
  cd "$R"
  # Entorno limpio: el de kling-guest no tiene nada que hacer en Android. La
  # ruta de chroot se resuelve ANTES: con env -i y PATH=/system/bin, env la
  # buscaría en el /system/bin de la VM, que no existe.
  local chroot_bin
  chroot_bin="$(command -v chroot)"
  exec env -i PATH=/system/bin:/system/xbin HOSTNAME=localhost \
    "$chroot_bin" "$R" /init "${args[@]}"
}

case "${1:-}" in
  --stage2) stage2 ;;
  "") stage1 ;;
  *) echo "uso: $0" >&2; exit 2 ;;
esac
