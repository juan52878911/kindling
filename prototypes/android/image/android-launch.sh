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
#      si ANDROID_NET=isolated; uno con nombre y un veth si ANDROID_NET=veth,
#      el valor por defecto) con unshare, y dentro:
#   3. monta en el rootfs /proc, /sys (rw), cgroup2, un /dev en tmpfs con los
#      nodos del invitado, devpts, /dev/shm, mqueue y, si el init.rc de la
#      imagen no lo hace, binderfs;
#   4. pivot_root al rootfs y exec de /init con los argumentos de la imagen más
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
# Red veth (ver red_veth). Una /30 privada que no choca con la de kindling
# (172.16.0.0/30 en vz).
NETNS=kandroid
HOSTIF=kandroid0
VETH_HOST="${ANDROID_VETH_HOST:-10.88.0.1}"
VETH_ANDROID="${ANDROID_VETH_ANDROID:-10.88.0.2}"
ANDROID_PORTS="${ANDROID_PORTS:-5555 5900}"

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
  # dentro del rootfs, que es donde lo resolverá la raíz de Android.
  local init_real="$R/init" t
  if [ -L "$init_real" ]; then
    t="$(readlink "$init_real")"
    case "$t" in /*) init_real="$R$t" ;; *) init_real="$R/$t" ;; esac
  fi
  [ -x "$init_real" ] || fatal "no $R/init: the image has no Android rootfs"
  for c in unshare pivot_root mount umount; do
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

  # ── 2. espacios de nombres ─────────────────────────────────────────────────
  # --fork: el hijo es PID 1 del espacio de PIDs nuevo; exec de --stage2 en él.
  # --propagation private: nada de lo que se monte dentro sale a la VM.
  #
  # La red de Android NUNCA es la de la VM salvo que se pida (ANDROID_NET=shared):
  # netd reescribe las reglas de enrutamiento del espacio donde vive (borra la
  # regla "main" y añade "from all unreachable"), y con la red compartida la VM
  # se quedó sin rutas: los reenvíos de adb/vnc dejaron de contestar (medido,
  # docs/telefono.md). Con veth, Android tiene su eth0 y la VM enruta con NAT;
  # con isolated, solo "lo".
  local pre=() net=()
  case "$ANDROID_NET" in
    veth)
      if red_veth; then
        pre=(nsenter "--net=/run/netns/$NETNS" --)
      else
        log "aviso: sin enlace veth (arriba); Android arranca con la red aislada (solo lo)"
        net=(--net)
      fi ;;
    isolated) net=(--net) ;;
    shared) log "aviso: red compartida: netd tocará las rutas de la VM (docs/telefono.md)" ;;
  esac
  estado "booting"
  # exec: este PID pasa a ser el de unshare (nsenter hace exec sin fork), y su
  # hijo el init de Android. android-sh lo encuentra así.
  echo $$ >"$STATE_DIR/unshare.pid"
  # --kill-child: si unshare muere, Android muere con él y el bucle del
  # entrypoint no acaba con dos Android a la vez.
  exec "${pre[@]}" unshare --mount --pid --fork --kill-child --ipc --uts "${net[@]}" --propagation private \
    -- "$0" --stage2
}

# red_veth: el enlace privado Android↔VM, como la red "bridge" de Docker que
# Redroid espera. Un espacio de red con nombre (/run/netns/$NETNS) con eth0
# dentro (un extremo del veth) y $HOSTIF fuera; la VM hace de router:
#   - DNAT de los puertos de ANDROID_PORTS que llegan por el eth0 de la VM (los
#     reenvíos de kindling: 127.0.0.1:<p> del Mac → VM:5555 → Android:5555);
#   - MASQUERADE de lo que sale de Android: sale con la IP de la VM, así que la
#     política de egress de kindling (none/internet/allowlist) sigue mandando;
#   - Android no puede hablar con la VM (kling-guest :8080) ni con MMDS
#     (169.254.169.254): los secretos de la VM no son del teléfono.
# Idempotente: el bucle del entrypoint relanza el lanzador si Android muere.
# Tras restaurar un dorado no hay que rehacer nada: todo vive en la RAM.
red_veth() {
  local ipt p dns
  ipt="$(command -v iptables-legacy || command -v iptables || true)"
  [ -n "$ipt" ] || { log "aviso: no iptables in the base image (build-image.sh BASE_PKGS)"; return 1; }
  ip link del "$HOSTIF" 2>/dev/null || true
  ip netns del "$NETNS" 2>/dev/null || true
  ip netns add "$NETNS" || return 1
  if ! ip link add "$HOSTIF" type veth peer name eth0 netns "$NETNS"; then
    log "aviso: cannot create a veth pair (kernel without CONFIG_VETH? see kernel/config-android)"
    ip netns del "$NETNS" 2>/dev/null || true
    return 1
  fi
  ip addr add "$VETH_HOST/30" dev "$HOSTIF"
  ip link set "$HOSTIF" up
  ip -n "$NETNS" link set lo up
  ip -n "$NETNS" addr add "$VETH_ANDROID/30" dev eth0
  ip -n "$NETNS" link set eth0 up
  ip -n "$NETNS" route add default via "$VETH_HOST"
  echo 1 >/proc/sys/net/ipv4/ip_forward
  # Cadenas propias: se vacían y se rellenan en cada arranque.
  "$ipt" -t nat -N KANDROID_PRE 2>/dev/null || "$ipt" -t nat -F KANDROID_PRE
  "$ipt" -t nat -N KANDROID_POST 2>/dev/null || "$ipt" -t nat -F KANDROID_POST
  "$ipt" -N KANDROID_IN 2>/dev/null || "$ipt" -F KANDROID_IN
  "$ipt" -N KANDROID_FWD 2>/dev/null || "$ipt" -F KANDROID_FWD
  "$ipt" -t nat -C PREROUTING -j KANDROID_PRE 2>/dev/null || "$ipt" -t nat -A PREROUTING -j KANDROID_PRE
  "$ipt" -t nat -C POSTROUTING -j KANDROID_POST 2>/dev/null || "$ipt" -t nat -A POSTROUTING -j KANDROID_POST
  "$ipt" -C INPUT -j KANDROID_IN 2>/dev/null || "$ipt" -I INPUT -j KANDROID_IN
  "$ipt" -C FORWARD -j KANDROID_FWD 2>/dev/null || "$ipt" -I FORWARD -j KANDROID_FWD
  for p in $ANDROID_PORTS; do
    "$ipt" -t nat -A KANDROID_PRE -i eth0 -p tcp --dport "$p" -j DNAT --to-destination "$VETH_ANDROID:$p"
  done
  "$ipt" -t nat -A KANDROID_POST -s "$VETH_ANDROID/32" -o eth0 -j MASQUERADE
  "$ipt" -A KANDROID_IN -i "$HOSTIF" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT
  "$ipt" -A KANDROID_IN -i "$HOSTIF" -j DROP
  "$ipt" -A KANDROID_FWD -i "$HOSTIF" -d 169.254.0.0/16 -j DROP
  # DNS: el mismo servidor que usa la VM; Redroid lo lee de estos argumentos.
  dns="$(sed -n 's/^nameserver[[:space:]]*\([0-9.]*\).*/\1/p' /etc/resolv.conf 2>/dev/null | head -n 1)"
  # (stage2 vuelve a leer android.conf, así que van en una variable aparte.)
  [ -n "$dns" ] && KANDROID_NET_ARGS="androidboot.redroid_net_ndns=1 androidboot.redroid_net_dns1=$dns"
  export KANDROID_NET_ARGS
  log "red veth: Android $VETH_ANDROID/30 (eth0) <-> VM $VETH_HOST ($HOSTIF), puertos $ANDROID_PORTS, dns ${dns:-?}"
}

stage2() {
  # Aquí somos PID 1 de un espacio de PIDs nuevo, con montajes privados.
  cd /
  hostname localhost 2>/dev/null || true
  # En un espacio de red nuevo "lo" nace apagado; Docker lo levanta y el
  # init.rc de AOSP también ("ifup lo"), pero cuesta nada adelantarse.
  ip link set lo up 2>/dev/null || true
  # La raíz de Android tiene que ser un punto de montaje, como la que da
  # Docker: el init de AOSP (SetupMountNamespaces) hace mount(NULL, "/",
  # MS_REC|MS_SHARED) y con / como simple directorio recibe EINVAL
  # y aborta ("Failed to remount / as 104000"). Medido en la fase 0.
  mount --bind "$R" "$R"
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
    # Un ext4 en un fichero de un tmpfs, no un tmpfs directo: Android necesita
    # xattrs "user.*" en /data (serial del usuario 0) y tmpfs no los tiene
    # hasta Linux 6.6. discard devuelve al tmpfs lo que Android borra.
    local ram=/run/kindling-android/data
    mkdir -p "$ram"
    mount -t tmpfs -o "mode=0700,size=$ANDROID_DATA_SIZE" tmpfs "$ram"
    cp --sparse=always /usr/local/lib/kindling-android/data.ext4 "$ram/data.ext4"
    mount -o loop,discard,noatime "$ram/data.ext4" "$R/data"
    chmod 0771 "$R/data"
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
  # shellcheck disable=SC2206
  [ -n "${KANDROID_NET_ARGS:-}" ] && args+=($KANDROID_NET_ARGS)
  log "exec /init ${args[*]}"
  # pivot_root y no chroot, como Docker. Con chroot la raíz del montaje sigue
  # siendo la de la VM y el kernel nombra los ficheros de Android como
  # /android/...: zygote compara las rutas de sus descriptores con una lista
  # blanca y aborta ("Not allowlisted: /android/apex/.../core-oj.jar"), y con
  # él netd (onrestart). Medido en la fase 0.
  #
  # La raíz de la VM queda en /$vieja durante un instante y se suelta con el
  # umount de Debian. Tras el pivot su ELF buscaría el cargador en /lib de
  # Android (y /bin/umount sería el de toybox, cuyo linker64 está en un APEX aún
  # sin montar), así que se llama a través del cargador de glibc de la raíz vieja.
  # Desde ahí solo builtins de bash hasta el exec de /init.
  local vieja=.kindling-vm
  # (x86_64: el cargador de glibc tiene otro nombre. uname se mira antes del
  # pivot, cuando aún es el de Debian.)
  local ld="/$vieja/lib/ld-linux-aarch64.so.1" tri=aarch64-linux-gnu
  if [ "$(uname -m)" = x86_64 ]; then ld="/$vieja/lib64/ld-linux-x86-64.so.2"; tri=x86_64-linux-gnu; fi
  cd "$R"
  mkdir -p "$vieja"
  pivot_root . "$vieja"
  cd /
  "$ld" --library-path "/$vieja/lib/$tri" "/$vieja/bin/umount" -l "/$vieja"
  # Entorno limpio: el de kling-guest no tiene nada que hacer en Android.
  local v
  for v in $(compgen -e); do unset "$v" 2>/dev/null || true; done
  export PATH=/system/bin:/system/xbin HOSTNAME=localhost
  exec /init "${args[@]}"
}

case "${1:-}" in
  --stage2) stage2 ;;
  "") stage1 ;;
  *) echo "uso: $0" >&2; exit 2 ;;
esac
