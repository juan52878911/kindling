#!/usr/bin/env bash
# Arnés común de los spikes de la ronda 9 (r9-hugepages, r9-volcado-tmpfs, r9-pmem-dax): una microVM
# de Firecracker «a mano», sin kindling, para medir hipótesis sin tocar el producto. Se carga con
# `source`; no se ejecuta solo.
#
# Lo que monta, todo bajo $W (un directorio de trabajo que el spike borra al acabar):
#   rootfs.ext4   busybox estático + tocar (tocar.c, compilado estático aquí) + /init
#   datos.img     los «pesos»: el GGUF de un modelo de kindling si hay (KLING_ROOT/cache/von/models)
#                 o DATOS_MIB de aleatorio; el invitado lo copia a un tmpfs suyo al arrancar
# La VM carga los pesos, imprime «R9 listo» y espera en la consola: cada «t» que se le escribe por
# la entrada estándar de Firecracker (un FIFO) recorre una página de cada 4 KiB de los pesos y dice
# cuánto tardó. Tras restaurar un snapshot, la primera pasada paga los fallos de EPT del host.
#
# Necesita root, /dev/kvm, firecracker, curl, jq, gcc con libc estática, busybox estático (paquete
# busybox-static), mkfs.ext4 con -d. Lo comprueba fc_requisitos.
#
# Entorno: VMLINUX (el kernel de invitado de kindling; por defecto el de /var/lib/kindling/images o
# KLING_ROOT/images), KLING_ROOT, DATOS_MIB (1065: el GGUF de Q4 de la ronda 8), MEM_MIB (1600: la RAM
# de von-q4), VCPUS (2), FC (firecracker).

LIB_FC_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
FC=${FC:-firecracker}
DATOS_MIB=${DATOS_MIB:-1065}
MEM_MIB=${MEM_MIB:-1600}
VCPUS=${VCPUS:-2}
fc_paso() { echo "== $(date +%T) $*" >&2; }
fc_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }

fc_requisitos() { # fc_requisitos: sale con 2 y dice qué falta
  local falta=() c
  [ "$(id -u)" = 0 ] || falta+=("root")
  [ -w /dev/kvm ] || falta+=("/dev/kvm")
  for c in "$FC" curl jq gcc mkfs.ext4 perl python3; do command -v "$c" >/dev/null || falta+=("$c"); done
  fc_busybox >/dev/null || falta+=("busybox estático (busybox-static)")
  [ -n "${VMLINUX:-}" ] || VMLINUX=$(fc_vmlinux)
  [ -n "$VMLINUX" ] && [ -r "$VMLINUX" ] || falta+=("kernel de invitado (VMLINUX)")
  [ ${#falta[@]} = 0 ] || { echo "faltan: ${falta[*]}" >&2; return 2; }
}
fc_vmlinux() { # el kernel de invitado de kindling que haya
  local f
  for f in /var/lib/kindling/images/vmlinux "${KLING_ROOT:-/nonexistent}/images/vmlinux"; do
    [ -r "$f" ] && { echo "$f"; return 0; }
  done
  return 1
}
fc_busybox() {
  local b
  for b in /bin/busybox /usr/bin/busybox; do
    [ -x "$b" ] && { ldd "$b" 2>&1 || true; } | grep -q 'not a dynamic' && { echo "$b"; return 0; }
  done
  return 1
}

# fc_rootfs <dir de trabajo> [líneas extra del init antes de cargar]: rootfs.ext4 en $1.
fc_rootfs() {
  local w=$1 r=$1/rootfs bb
  bb=$(fc_busybox) || return 1
  rm -rf "$r"; mkdir -p "$r"/{bin,dev,proc,sys,tmp,mnt}
  cp "$bb" "$r/bin/busybox"
  for a in sh mount mkdir cat echo sleep; do ln -sf busybox "$r/bin/$a"; done
  gcc -O2 -static -o "$r/tocar" "$LIB_FC_DIR/tocar.c" || return 1
  {
    echo '#!/bin/sh'
    echo 'mount -t proc proc /proc; mount -t sysfs sys /sys; mount -t devtmpfs dev /dev'
    echo "mkdir -p /dev/shm; mount -t tmpfs -o size=$(( DATOS_MIB + 64 ))m tmpfs /dev/shm"
    echo "${2:-}"
    echo "${CARGA_INIT:-/tocar cargar /dev/vdb /dev/shm/pesos}"
    echo "exec /tocar bucle ${PESOS_INVITADO:-/dev/shm/pesos} </dev/console >/dev/console 2>&1"
  } > "$r/init"
  chmod 755 "$r/init"
  rm -f "$w/rootfs.ext4"
  mkfs.ext4 -q -F -d "$r" "$w/rootfs.ext4" 64M >/dev/null || return 1
}

# fc_datos <fichero>: los pesos. El GGUF real si hay uno (bytes de verdad, nada de páginas a cero que
# el volcado perforaría), si no aleatorio.
fc_datos() {
  local g
  # el mayor (el GGUF del modelo; los otros son tokenizadores o borradores)
  g=$(find "${KLING_ROOT:-/nonexistent}/cache/von/models" -maxdepth 1 -name "*.gguf" -printf "%s %p\n" 2>/dev/null | sort -rn | head -1 | cut -d" " -f2-)
  if [ -n "$g" ]; then
    head -c "$(( DATOS_MIB << 20 ))" "$g" > "$1"; echo "gguf:$(basename "$g")"
  else
    head -c "$(( DATOS_MIB << 20 ))" /dev/urandom > "$1"; echo "aleatorio"
  fi
}

# fc_api <sock> <método> <ruta> [json]: una llamada a la API de Firecracker; falla si no es 2xx.
fc_api() {
  local c
  c=$(curl -s -o "$FC_W/api.out" -w '%{http_code}' --unix-socket "$1" -X "$2" -H 'Content-Type: application/json' \
      ${4:+--data "$4"} "http://localhost$3")
  case $c in 2*) return 0 ;; esac
  echo "fc_api $2 $3 → $c: $(cat "$FC_W/api.out" 2>/dev/null)" >&2; return 1
}

# fc_lanzar <etq>: un proceso firecracker con su socket y su FIFO de consola; deja FC_PID, FC_SOCK,
# FC_LOG (su salida = la consola del invitado) y el FIFO abierto en el descriptor 7.
fc_lanzar() {
  FC_SOCK=$FC_W/$1.sock; FC_LOG=$FC_W/$1.log
  rm -f "$FC_SOCK" "$FC_W/$1.in"; mkfifo "$FC_W/$1.in"
  exec 7<>"$FC_W/$1.in"
  "$FC" --api-sock "$FC_SOCK" < "$FC_W/$1.in" > "$FC_LOG" 2>&1 &
  FC_PID=$!
  local _i
  for _ in $(seq 1 100); do [ -S "$FC_SOCK" ] && return 0; sleep 0.05; done
  echo "firecracker no abrió su socket" >&2; return 1
}
fc_parar() { [ -n "${FC_PID:-}" ] && { kill -9 "$FC_PID" 2>/dev/null; wait "$FC_PID" 2>/dev/null; }; FC_PID=""; exec 7>&-; }

# fc_arrancar <etq> [discos extra en JSON, uno por argumento]: arranca la VM de prueba y espera a «R9 listo».
fc_arrancar() {
  local etq=$1 d
  shift
  fc_lanzar "$etq" || return 1
  fc_api "$FC_SOCK" PUT /machine-config "{\"vcpu_count\":$VCPUS,\"mem_size_mib\":$MEM_MIB}" || return 1
  fc_api "$FC_SOCK" PUT /boot-source "{\"kernel_image_path\":\"$VMLINUX\",\"boot_args\":\"console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda rw init=/init ${BOOT_EXTRA:-}\"}" || return 1
  fc_api "$FC_SOCK" PUT /drives/rootfs "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$FC_W/rootfs.ext4\",\"is_root_device\":true,\"is_read_only\":false}" || return 1
  fc_api "$FC_SOCK" PUT /drives/datos "{\"drive_id\":\"datos\",\"path_on_host\":\"$FC_W/datos.img\",\"is_root_device\":false,\"is_read_only\":true}" || return 1
  for d in "$@"; do fc_api "$FC_SOCK" PUT "$(jq -r .ruta <<<"$d")" "$(jq -c .cuerpo <<<"$d")" || return 1; done
  fc_api "$FC_SOCK" PUT /actions '{"action_type":"InstanceStart"}' || return 1
  fc_esperar "R9 listo" "${PLAZO_ARRANQUE_S:-180}"
}
# fc_esperar <texto> <segundos>: espera a que la consola diga el texto (cuenta líneas nuevas).
fc_esperar() {
  local _i
  for _i in $(seq 1 $(( $2 * 10 ))); do
    grep -q "$1" "$FC_LOG" 2>/dev/null && return 0
    kill -0 "$FC_PID" 2>/dev/null || { echo "firecracker murió: $(tail -5 "$FC_LOG")" >&2; return 1; }
    sleep 0.1
  done
  echo "sin «$1» en ${2} s: $(tail -5 "$FC_LOG")" >&2; return 1
}

# fc_snapshot <mem.file> <snap.file>: pausa y volcado Full; saca {"ms","escritos_bytes"} del VMM.
fc_snapshot() {
  local t0 w0 w1
  fc_api "$FC_SOCK" PATCH /vm '{"state":"Paused"}' || return 1
  w0=$(awk '/^write_bytes/{print $2}' "/proc/$FC_PID/io"); t0=$(fc_ms)
  fc_api "$FC_SOCK" PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$2\",\"mem_file_path\":\"$1\"}" || return 1
  w1=$(awk '/^write_bytes/{print $2}' "/proc/$FC_PID/io")
  jq -nc --argjson ms "$(( $(fc_ms) - t0 ))" --argjson w "$(( w1 - w0 ))" '{ms:$ms, escritos_bytes:$w}'
}

# fc_restaurar <etq> <mem.file> <snap.file>: VMM nuevo desde el snapshot (backend File, MAP_PRIVATE,
# como kindling) y en marcha; saca los ms del load.
fc_restaurar() {
  local t0
  fc_lanzar "$1" || return 1
  t0=$(fc_ms)
  fc_api "$FC_SOCK" PUT /snapshot/load "{\"snapshot_path\":\"$3\",\"mem_backend\":{\"backend_type\":\"File\",\"backend_path\":\"$2\"},\"resume_vm\":true}" || return 1
  echo $(( $(fc_ms) - t0 ))
}

# ── contadores de KVM ────────────────────────────────────────────────────────
# Salidas de VM y páginas del mapeo de la EPT (debugfs de KVM; en la EC2 anidada, el KVM del L1).
kvm_debugfs() {
  [ -d /sys/kernel/debug/kvm ] || mount -t debugfs none /sys/kernel/debug 2>/dev/null
  [ -d /sys/kernel/debug/kvm ]
}
kvm_global() { # kvm_global <stat>: el contador global (suma de todas las VM) o null
  local f=/sys/kernel/debug/kvm/$1
  if [ -r "$f" ]; then cat "$f"; else echo null; fi
}
kvm_vm() { # kvm_vm <pid> <stat>: el contador de la VM de ese proceso o null
  local d
  for d in /sys/kernel/debug/kvm/"$1"-*; do [ -r "$d/$2" ] && { cat "$d/$2"; return; }; done
  echo null
}
# fc_tocar <n esperado>: escribe «t», espera la línea n de tocar y saca
# {"peticion_ms" (lo visto desde el host), "invitado_ms", "salidas", "ept_violaciones", "pages_4k", "pages_2m"}.
fc_tocar() {
  local n=$1 t0 e0 v0 e1 v1 l
  e0=$(kvm_global exits); v0=$(kvm_vm "$FC_PID" exits)
  t0=$(fc_ms)
  echo t >&7
  fc_esperar "R9 tocar n=$n " "${PLAZO_TOCAR_S:-120}" || return 1
  local tot=$(( $(fc_ms) - t0 ))
  e1=$(kvm_global exits); v1=$(kvm_vm "$FC_PID" exits)
  l=$(grep "R9 tocar n=$n " "$FC_LOG" | tail -1)
  jq -nc --argjson tot "$tot" --arg l "$l" --arg e0 "$e0" --arg e1 "$e1" --arg v0 "$v0" --arg v1 "$v1" \
    --arg p4 "$(kvm_vm "$FC_PID" pages_4k)" --arg p2 "$(kvm_vm "$FC_PID" pages_2m)" \
    'def n($x): ($x|tonumber? // null);
     def d($a;$b): if n($a) == null or n($b) == null then null else n($b) - n($a) end;
     {peticion_ms:$tot, invitado_ms:($l|capture("ms=(?<m>[0-9.]+)").m|tonumber),
      salidas_vm:d($v0;$v1), salidas_host:d($e0;$e1), pages_4k:n($p4), pages_2m:n($p2)}'
}
# fc_perf_kvm <segundos> <fichero>: perf kvm stat del VMM, si hay perf (opcional; en debugfs ya hay cifras).
fc_perf_kvm() {
  command -v perf >/dev/null || return 1
  timeout "$1" perf kvm stat record -p "$FC_PID" -o "$2.data" >/dev/null 2>&1
  perf kvm stat report -i "$2.data" > "$2" 2>&1
}
# disco_bytes_escritos: suma de sectores escritos de los discos enteros (para deltas por fase).
disco_bytes_escritos() {
  local d t=0 s
  for d in /sys/block/*; do
    case ${d##*/} in loop*|ram*|zram*) continue ;; esac
    s=$(awk '{print $7}' "$d/stat" 2>/dev/null); t=$(( t + ${s:-0} * 512 ))
  done
  echo "$t"
}
