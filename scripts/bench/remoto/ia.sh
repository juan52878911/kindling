#!/usr/bin/env bash
# Banco de IA (ronda 8: P1, P2 y P3 de analisis/ronda8-ia/plan.md §4; ronda 9: datos de la carga de
# modelos y spikes de analisis/ronda9-carga/plan-r9.md, Z7) en el host del banco.
#
#   ia.sh preparar   (root, solo en la instancia del banco) paquetes, Firecracker y las imágenes de
#                    kindling; con KB_NVME=1, el disco NVMe local formateado y montado en /srv/kt-bench;
#                    nada de k3s ni Docker
#   ia.sh medir      dorados de IA_MODELOS y las pruebas de IA_PRUEBAS, cada una con su plazo, con un
#                    JSON y un .txt por prueba en $KB/res y el resumen de ia.py en res/resumen-ia.txt
#   ia.sh fabricar <n> <desc> <orden…>
#                    (lo llama chispa-p3.sh, con RES, COR y JUNIT de medir) fabrica un dorado con la
#                    misma vigilancia y el mismo registro que los de medir
#   ia.sh vigilar <prefijo> <dir>
#                    (lo lanza medir, y chispa-p3.sh) mientras viva la plantilla <prefijo>…-golden-…,
#                    guarda cada VIGILAR_S s su service.log, su consola, el /proc/meminfo del invitado
#                    (con exec) y el RSS de su VMM; hasta que lo maten
#
# En la EC2 (lo llaman ec2.sh y fases.sh ia) levanta el daemon privado kt-bench y hace higiene entre
# variantes (`pkill -9 -x firecracker`). En un host Linux cualquiera con kindling (el lab):
#
#   USAR_DAEMON=1 KB=$HOME/kb IA_MODELOS=von-smol IA_PRUEBAS="p1 p2" bash scripts/bench/remoto/ia.sh medir
#
# usa el daemon al que apunte kling (KLING_HOST o su contexto) y NO mata procesos ni para nada.
#
# Qué deja además de los JSON (C9-12, C9-20), todo en $RES y descrito en $RES/corrida.json (esquema
# ia-corrida/1, lo comprueba ia.py: sin él la corrida se rechaza):
#   journal-<unidad>.log       el journal del daemon de toda la corrida (JOURNAL_UNIT; kt-bench en la EC2;
#                              con USAR_DAEMON hay que darla o la corrida sale sin journal)
#   dorado-<n>/                por dorado fabricado: salida de `kling ai model add`, su trozo de journal,
#                              service.log y consola del invitado, meminfo.jsonl y vmm.jsonl (picos)
#   corrida.json               desglose del commit (de la línea «commit X -> n: …» del daemon), picos de
#                              memoria, reinicios del servicio, bytes de /proc/diskstats por fase y spikes
#
# Entorno: KB (/opt/kbench), IA_PRUEBAS ("p1 p2", p3 si hay $KB/sube/chispa; y los spikes s-huge,
# s-pmem, s-volcado de prototypes/r9-*), IA_MODELOS (lista de «nombre=id:quant» para `kling ai model add`
# o «nombre» de un dorado que ya existe; por defecto los Qwen2.5-1.5B Q4_K_M y Q8_0), IA_CPUS (4), RUNS
# (10), REPLICAS (4), CYCLES (20), MEM_RONDAS (2), CLIENTES, CARGA_S, PLAZO_P1_MIN (60), PLAZO_P2_MIN
# (45), PLAZO_P3_MIN (45), PLAZO_SPIKE_MIN (25), VIGILAR_S (5; 0 = no vigilar las plantillas),
# JOURNAL_UNIT, y para P3 el material en $KB/sube/chispa: imagen.tar (imagen OCI de chispa-server con
# modelos y config horneados), peticion.json, warmup.jsonl y, opcional, directo.sh (el mismo binario en
# el host, escuchando en 127.0.0.1:18090). Con IA_PRUEBAS=p3 y sin ese material, P3 construye chispa en
# la instancia con chispa-p3.sh (plazo de la preparación PLAZO_P3_PREP_MIN, 30; sus variables P3_* y
# CHISPA_COMMIT, en su cabecera).
set -uo pipefail
KB=${KB:-/opt/kbench}; export KB
AQUI=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RES=${RES:-$KB/res}
paso() { echo "== $(date +%T) $*"; }
bajar() { curl -fsSL --retry 3 -o "$2" "$1"; }
ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }

# nvme_local: el primer disco NVMe de instancia (instance store) sin particiones ni montajes.
nvme_local() {
  local d
  for d in /dev/nvme*n1; do
    [ -b "$d" ] || continue
    grep -qi 'instance storage' "/sys/block/${d#/dev/}/device/model" 2>/dev/null || continue
    lsblk -nro MOUNTPOINT "$d" | grep -q . && continue
    echo "$d"; return 0
  done
  return 1
}

preparar() {
  [ "$(id -u)" = 0 ] || { echo "preparar va como root" >&2; exit 2; }
  mkdir -p "$KB"; cd "$KB" || exit 1
  exec > >(tee -a "$KB/setup-ia.log") 2>&1
  paso apt
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  # gcc, libc6-dev y busybox-static: el invitado de prueba de los spikes (prototypes/r9-*).
  apt-get install -y -qq jq perl curl zstd python3 iptables ipset conntrack e2fsprogs xfsprogs debootstrap \
    gcc libc6-dev busybox-static >/dev/null || exit 1
  if [ "${KB_NVME:-0}" = 1 ]; then
    # Fila de disco rápido: la raíz del daemon del banco en el NVMe local, con el mismo sistema de
    # ficheros que la raíz (que la única diferencia sea el disco, no reflink sí/no).
    paso "disco NVMe local para /srv/kt-bench"
    local dev fs
    dev=$(nvme_local) || { echo "KB_NVME=1 y la instancia no tiene NVMe local libre" >&2; exit 1; }
    fs=${KB_NVME_FS:-$(findmnt -no FSTYPE /)}
    case $fs in ext4) mkfs.ext4 -q -F "$dev" ;; xfs) mkfs.xfs -q -f "$dev" ;; *) echo "sistema de ficheros $fs sin receta" >&2; exit 1 ;; esac || exit 1
    mkdir -p /srv/kt-bench && mount "$dev" /srv/kt-bench || exit 1
    echo "$dev $fs" > "$KB/nvme"
  fi
  paso firecracker
  local F=v1.17.0
  bajar "https://github.com/firecracker-microvm/firecracker/releases/download/$F/firecracker-$F-x86_64.tgz" fc.tgz || exit 1
  bajar "https://github.com/firecracker-microvm/firecracker/releases/download/$F/firecracker-$F-x86_64.tgz.sha256.txt" fc.sha || exit 1
  [ "$(sha256sum fc.tgz | cut -d' ' -f1)" = "$(cut -d' ' -f1 fc.sha)" ] || { echo "sha firecracker NO coincide"; exit 1; }
  tar xzf fc.tgz
  install -m755 "release-$F-x86_64/firecracker-$F-x86_64" /usr/local/bin/firecracker
  install -m755 "release-$F-x86_64/jailer-$F-x86_64" /usr/local/bin/jailer
  # La especificación de la API de esta versión: el spike de pmem mira en ella si hay virtio-pmem.
  for f in "release-$F-x86_64"/*.yaml; do [ -e "$f" ] && install -m644 "$f" "$KB/firecracker-api.yaml"; done
  paso imagenes kindling
  local R="https://github.com/juan52878911/kindling/releases/download/${KB_RELEASE:-v1.0.0-rc.2}"
  mkdir -p /var/lib/kindling/images
  bajar "$R/vmlinux-amd64.zst" v.zst && zstd -dqf v.zst -o /var/lib/kindling/images/vmlinux || exit 1
  bajar "$R/min-amd64.ext4.zst" m.zst && zstd -dqf m.zst -o /var/lib/kindling/images/min.ext4 || exit 1
  rm -rf "$KB/bins"; cp -r "$KB/sube/bins" "$KB/bins" || exit 1
  paso fin preparar
}

# primero devuelve la primera ruta ejecutable o legible de la lista.
primero() { for f in "$@"; do [ -e "$f" ] && { echo "$f"; return 0; }; done; return 1; }

# ── vigilar una plantilla (C9-12, C9-13, C9-20) ──────────────────────────────
# El fabricante borra la plantilla al terminar, también cuando falla: lo del invitado hay que
# recogerlo mientras vive. Cada pasada sobrescribe service.log y consola.log (de forma atómica) y
# añade una línea a meminfo.jsonl y vmm.jsonl, así queda lo último que se vio y el pico.
vigilar() {
  local pre=${1:?prefijo} dir=${2:?dir} K=${KLING:-kling} m pid t
  mkdir -p "$dir"
  while :; do
    m=$("$K" ps -json 2>/dev/null | jq -r --arg p "$pre" '[.[]? | select((.name|startswith($p)) and (.name|contains("-golden-"))) | .name][0] // empty')
    if [ -n "$m" ]; then
      echo "$m" > "$dir/plantilla"
      t=$(date +%s)
      if "$K" logs -service -tail 0 "$m" > "$dir/.s" 2>/dev/null && [ -s "$dir/.s" ]; then mv "$dir/.s" "$dir/service.log"
      elif "$K" exec "$m" -- tail -c 262144 /var/log/service.log > "$dir/.s" 2>/dev/null && [ -s "$dir/.s" ]; then mv "$dir/.s" "$dir/service.log"
      else echo "ni kling logs -service ni kling exec dieron el service.log de $m" > "$dir/service.log.motivo"; fi
      "$K" logs -tail 0 "$m" > "$dir/.c" 2>/dev/null && mv "$dir/.c" "$dir/consola.log"
      "$K" exec "$m" -- cat /proc/meminfo 2>/dev/null | awk -v t="$t" '
        /^(MemTotal|MemAvailable|AnonPages|Shmem|Cached):/ {sub(":", "", $1); v[$1]=int($2/1024)}
        END {if (length(v)) printf "{\"t\":%d,\"total_mib\":%d,\"disponible_mib\":%d,\"usada_mib\":%d,\"anon_mib\":%d,\"shmem_mib\":%d,\"cached_mib\":%d}\n",
             t, v["MemTotal"], v["MemAvailable"], v["MemTotal"]-v["MemAvailable"], v["AnonPages"], v["Shmem"], v["Cached"]}' >> "$dir/meminfo.jsonl"
      pid=$("$K" top -json 2>/dev/null | jq -r --arg n "$m" '[.machines[]? | select(.name==$n) | .pid][0] // empty')
      if [ -n "$pid" ] && [ -r "/proc/$pid/status" ]; then
        awk -v t="$t" '/^VmRSS:/{r=$2} /^VmHWM:/{h=$2} END {printf "{\"t\":%d,\"rss_mib\":%d,\"hwm_mib\":%d}\n", t, r/1024, h/1024}' "/proc/$pid/status" >> "$dir/vmm.jsonl"
      fi
    fi
    sleep "${VIGILAR_S:-5}"
  done
}

# ── /proc/diskstats por fase ─────────────────────────────────────────────────
# disco_foto <fichero>: «dispositivo sectores_leídos sectores_escritos entero» de los discos que importan;
# entero = 1 si es un disco entero (está en /sys/block y no es loop): solo esos suman en el total.
disco_foto() {
  if [ -r /proc/diskstats ]; then
    local d enteros=""
    for d in /sys/block/*; do case ${d##*/} in loop*|ram*|zram*) ;; *) enteros="$enteros ${d##*/}" ;; esac; done
    awk -v e="$enteros " '$3 !~ /^(ram|zram)/ {print $3, $6, $10, (index(" " e, " " $3 " ") ? 1 : 0)}' /proc/diskstats > "$1"
  else : > "$1"; fi
}
# (%.0f y no %d: mawk, el awk de Debian, satura %d en 2^31-1 y daba 2 GiB en vez de lo escrito.)
# disco_delta <antes> <después> → {"dispositivos":{dev:{leidos_bytes,escritos_bytes}}, "total":{…}}: cada
# dispositivo que se movió (particiones y loop incluidos) y el total de los discos enteros.
disco_delta() {
  awk 'NR==FNR {r[$1]=$2; w[$1]=$3; next}
       ($1 in r) {dr=($2-r[$1])*512; dw=($3-w[$1])*512; if (dr>0 || dw>0) {
         d[$1]=sprintf("\"%s\":{\"leidos_bytes\":%.0f,\"escritos_bytes\":%.0f}", $1, dr, dw)
         if ($4 == 1) {tr+=dr; tw+=dw}}}
       END {s=""; for (k in d) {s = s (s=="" ? "" : ",") d[k]}
            printf "{\"dispositivos\":{%s},\"total\":{\"leidos_bytes\":%.0f,\"escritos_bytes\":%.0f}}\n", s, tr, tw}' "$1" "$2"
}
FASE_T0=0
fase_ini() { disco_foto "$COR/disco-antes"; FASE_T0=$(date +%s); }
fase_fin() { # fase_fin <nombre>: apunta la fase en corrida (fases.jsonl)
  disco_foto "$COR/disco-despues"
  jq -c --arg f "$1" --argjson s "$(( $(date +%s) - FASE_T0 ))" '{fase:$f, segundos:$s} + .' \
    <<<"$(disco_delta "$COR/disco-antes" "$COR/disco-despues")" >> "$COR/fases.jsonl"
}

# ── journal y desglose del commit ────────────────────────────────────────────
journal() { # journal <desde epoch> <fichero>: el journal del daemon desde entonces
  [ -n "$JUNIT" ] || return 1
  if [ -n "${JOURNAL_FILE:-}" ]; then cp "$JOURNAL_FILE" "$2"; return 0; fi   # pruebas en seco
  journalctl -u "$JUNIT" --no-pager -o short-precise --since "@$1" > "$2" 2>/dev/null
}
# desglose <n> <fichero journal> → {"overlay":80,…,"total":27285} de la última línea «commit … -> n…: »
desglose() {
  grep -E "commit [^ ]+ -> $1(-v[0-9]+)?: " "$2" 2>/dev/null | tail -1 | perl -ne '
    s/^.* -> \S+: //; my @p; while (/([a-z_]+) (\d+) ms/g) { push @p, "\"$1\":$2" }
    print "{" . join(",", @p) . "}\n" if @p'
}
# dorado_fin <n> <modelo> <rc> <ms> <t0 epoch>: dorado-<n>/ y su línea en corrida (dorados.jsonl)
dorado_fin() {
  local n=$1 dir="$RES/dorado-$1" c=null motivo="" pg=null pv=null reinicios=0 sl=null mi=null
  journal "$5" "$dir/journal.log" || true
  [ -s "$dir/journal.log" ] && c=$(desglose "$n" "$dir/journal.log") && [ -n "$c" ] || c=null
  if [ -s "$dir/service.log" ]; then
    sl="\"dorado-$n/service.log\""
    reinicios=$(grep -c 'service exited with ' "$dir/service.log")
  else motivo=$(cat "$dir/service.log.motivo" 2>/dev/null || echo "la plantilla no llegó a verse viva (VIGILAR_S=${VIGILAR_S:-5})"); fi
  [ -s "$dir/meminfo.jsonl" ] && { mi="\"dorado-$n/meminfo.jsonl\""; pg=$(jq -s 'map(.usada_mib) | max' "$dir/meminfo.jsonl"); }
  [ -s "$dir/vmm.jsonl" ] && pv=$(jq -s 'map(.hwm_mib) | max' "$dir/vmm.jsonl")
  jq -nc --arg n "$n" --arg m "$2" --argjson rc "$3" --argjson ms "$4" --argjson c "$c" --argjson sl "$sl" --arg mo "$motivo" \
    --argjson mi "$mi" --argjson pg "${pg:-null}" --argjson pv "${pv:-null}" --argjson re "$reinicios" \
    --argjson d "$(disco_delta "$COR/disco-antes" "$COR/disco-despues" | jq -c .total)" \
    '{dorado:$n, modelo:$m, rc:$rc, fabricar_ms:$ms, commit:$c, service_log:$sl,
      service_log_motivo:(if $mo == "" then null else $mo end), meminfo:$mi, pico_invitado_mib:$pg,
      pico_vmm_mib:$pv, reinicios_servicio:$re, disco:$d}' >> "$COR/dorados.jsonl"
}

# fabricar <n> <descripción> <orden…>: corre la orden que fabrica el dorado n vigilando su plantilla,
# con su fase de disco, su trozo de journal y su línea en la corrida. Devuelve el rc de la orden. La usan
# medir y chispa-p3.sh (`ia.sh fabricar`, con RES, COR y JUNIT en el entorno).
fabricar() {
  local n=$1 desc=$2 vpid="" t0 t0s rc
  shift 2
  mkdir -p "$RES/dorado-$n" "$COR"
  if [ "${VIGILAR_S:-5}" != 0 ]; then vigilar "$n-" "$RES/dorado-$n" & vpid=$!; fi
  fase_ini; t0=$(ms); t0s=$(date +%s)
  "$@" 2>&1 | tee "$RES/dorado-$n/fabricar.log"; rc=${PIPESTATUS[0]}
  [ -n "$vpid" ] && { kill "$vpid" 2>/dev/null; wait "$vpid" 2>/dev/null; }
  fase_fin "dorado $n"
  dorado_fin "$n" "$desc" "$rc" "$(( $(ms) - t0 ))" "$t0s"
  return "$rc"
}

# corrida_json: junta las piezas en $RES/corrida.json (también si medir sale a medias).
corrida_json() {
  [ -n "${COR:-}" ] && [ -d "$COR" ] || return 0
  local j='{"omitido":"sin JOURNAL_UNIT (con USAR_DAEMON=1 hay que decir la unidad de systemd del daemon)"}' f l
  if [ -n "$JUNIT" ]; then
    f="journal-$JUNIT.log"
    if journal "$T_INICIO" "$RES/$f" && [ -s "$RES/$f" ]; then
      l=$(wc -l < "$RES/$f" | tr -d ' ')
      j=$(jq -nc --arg u "$JUNIT" --arg f "$f" --argjson l "$l" '{unidad:$u, fichero:$f, lineas:$l}')
    else j=$(jq -nc --arg u "$JUNIT" '{omitido:("journalctl -u " + $u + " no devolvió nada")}'); fi
  fi
  local raiz=${KLING_ROOT:-/} dev fs tipo=gp3
  dev=$(df -P "$raiz" 2>/dev/null | awk 'NR==2 {print $1}'); fs=$(df -PT "$raiz" 2>/dev/null | awk 'NR==2 {print $2}')
  [ -f "$KB/nvme" ] && tipo="nvme local"
  jq -n --arg i "$(date -u -d "@$T_INICIO" +%FT%TZ 2>/dev/null || date -u -r "$T_INICIO" +%FT%TZ)" --arg fin "$(date -u +%FT%TZ)" \
    --arg u "$(uname -srm)" --arg dev "${dev:-?}" --arg fs "${fs:-?}" --arg r "$raiz" --arg t "${EC2_DISCO_DESC:-$tipo}" \
    --argjson j "$j" --slurpfile d <(cat "$COR/dorados.jsonl" 2>/dev/null) \
    --slurpfile fa <(cat "$COR/fases.jsonl" 2>/dev/null) --slurpfile sp <(cat "$COR/spikes.jsonl" 2>/dev/null) \
    '{esquema:"ia-corrida/1", inicio:$i, fin:$fin, host:{uname:$u, disco:{dispositivo:$dev, fs:$fs, raiz:$r, tipo:$t}},
      journal:$j, dorados:$d, fases:$fa, spikes:$sp}' > "$RES/corrida.json"
}

medir() {
  mkdir -p "$RES"
  exec > >(tee -a "$RES/medir-ia.log") 2>&1
  local HIG=0
  T_INICIO=$(date +%s)
  if [ "${USAR_DAEMON:-0}" = 1 ]; then
    K=${KLING:-kling}
    JUNIT=${JOURNAL_UNIT:-}
  else
    [ "$(id -u)" = 0 ] || { echo "sin USAR_DAEMON=1, medir levanta kt-bench y va como root" >&2; exit 2; }
    bash "$KB/remoto/higiene.sh" inicio
    bash "$KB/remoto/daemon-up.sh" || { echo "daemon-up.sh falló (rc=$?)" >&2; exit 1; }
    export KLING_HOST=unix:///run/kt-bench/kling.sock KLING_CONFIG=/srv/kt-bench/config.json
    # Raíz de datos del daemon del banco: 96-von-bench.sh calienta de aquí los ficheros del dorado.
    export KLING_ROOT=${KLING_ROOT:-/srv/kt-bench/root}
    K=/srv/kt-bench/bin/kling; HIG=1
    JUNIT=${JOURNAL_UNIT:-kt-bench}
  fi
  COR=$RES/.corrida; rm -rf "$COR"; mkdir -p "$COR"; export COR JUNIT RES
  trap corrida_json EXIT
  VB=$(primero "$KB/96-von-bench.sh" "$AQUI/../../96-von-bench.sh") || { echo "no encuentro 96-von-bench.sh" >&2; exit 2; }
  IAPY=$(primero "$KB/ia.py" "$AQUI/../ia.py") || IAPY=""
  IAB=$(primero "$KB/bins/ia-bench" "$KB/sube/bins/ia-bench" "$(command -v ia-bench 2>/dev/null || echo /nonexistent)") || IAB=""
  [ -n "$IAB" ] || echo "AVISO: sin ia-bench: se omiten remove, ociosa por gateway, arranque y carga" >&2
  export KLING=$K IA_BENCH=$IAB HIGIENE=$HIG
  if ! command -v jq >/dev/null || ! command -v perl >/dev/null; then echo "faltan jq o perl" >&2; exit 2; fi

  local CH=$KB/sube/chispa
  local pruebas=${IA_PRUEBAS:-p1 p2}
  [ -z "${IA_PRUEBAS:-}" ] && [ -f "$CH/imagen.tar" ] && pruebas="$pruebas p3"
  local modelos=${IA_MODELOS:-von-q4=qwen2.5-1.5b-instruct:q4_k_m von-q8=qwen2.5-1.5b-instruct:q8_0}
  local dorados="" e n idq t0 rc
  for e in $modelos; do
    n=${e%%=*}
    if [ "$n" != "$e" ] && ! "$K" snapshots -json | jq -e --arg s "$n" 'any(.[]; .name==$s)' >/dev/null; then
      idq=${e#*=}
      paso "dorado $n (${idq%%:*} ${idq#*:})"
      t0=$(ms)
      fabricar "$n" "$idq" timeout 3600 "$K" ai model add "$n" -model "${idq%%:*}" -quant "${idq#*:}" -cpus "${IA_CPUS:-4}"; rc=$?
      jq -nc --arg n "$n" --arg m "$idq" --argjson ms "$(( $(ms) - t0 ))" --argjson rc "$rc" \
        '{dorado:$n, modelo:$m, fabricar_ms:$ms, rc:$rc}' > "$RES/dorado-$n.json"
      [ "$rc" = 0 ] || { echo "falló kling ai model add $n (service.log y meminfo en $RES/dorado-$n)" >&2; continue; }
    else
      jq -nc --arg n "$n" '{dorado:$n, preexistente:true}' >> "$COR/dorados.jsonl"
    fi
    dorados="$dorados $n"
  done
  # shellcheck disable=SC2086
  set -- $dorados
  [ $# -gt 0 ] || { echo "sin dorados que medir" >&2; exit 1; }
  local ultimo=${!#}

  entre() { # higiene entre pruebas: nada vivo del banco antes de la siguiente
    [ "$HIG" = 1 ] || return 0
    bash "$KB/remoto/higiene.sh" rm; pkill -9 -x firecracker >/dev/null 2>&1; sleep 1
  }
  for p in $pruebas; do
    case $p in
    p1)
      local cada=$(( ${PLAZO_P1_MIN:-60} * 60 / $# )); [ "$cada" -ge 60 ] || cada=60
      for n in "$@"; do
        paso "P1 $n (plazo ${cada} s)"; entre; fase_ini
        RUNS=${RUNS:-10} REPLICAS=1 SECCIONES="restaurar primera arranque tps" JSON_OUT="$RES/ia-p1-$n.json" \
          timeout "$cada" bash "$VB" "$n" > "$RES/ia-p1-$n.txt" 2>&1 || echo "P1 $n: rc=$? (¿plazo?)" >&2
        fase_fin "P1 $n"
      done ;;
    p2)
      paso "P2 $ultimo (plazo ${PLAZO_P2_MIN:-45} min)"; entre; fase_ini
      RUNS=${RUNS:-10} REPLICAS=${REPLICAS:-4} CYCLES=${CYCLES:-20} MEM_RONDAS=${MEM_RONDAS:-2} SECCIONES="memoria ociosa" \
        JSON_OUT="$RES/ia-p2-$ultimo.json" timeout "$(( ${PLAZO_P2_MIN:-45} * 60 ))" bash "$VB" "$ultimo" \
        > "$RES/ia-p2-$ultimo.txt" 2>&1 || echo "P2: rc=$? (¿plazo?)" >&2
      fase_fin "P2 $ultimo" ;;
    p3)
      if [ ! -f "$CH/imagen.tar" ]; then
        # Sin material subido: chispa se construye aquí (chispa-p3.sh: clona el commit fijo, imagen
        # imagen-ort, e5 en ONNX, banco, dorados con y sin claves) y se mide contra el binario directo.
        paso "P3 chispa construida en la instancia (preparar ${PLAZO_P3_PREP_MIN:-30} min, medir ${PLAZO_P3_MIN:-45} min)"; entre
        fase_ini
        timeout "$(( ${PLAZO_P3_PREP_MIN:-30} * 60 ))" bash "$AQUI/chispa-p3.sh" preparar \
          || { echo "P3: falló chispa-p3.sh preparar (rc=$?; ver $KB/p3/preparar.log)" >&2; fase_fin "P3 preparar"; continue; }
        fase_fin "P3 preparar"; fase_ini
        RES=$RES timeout "$(( ${PLAZO_P3_MIN:-45} * 60 ))" bash "$AQUI/chispa-p3.sh" medir \
          || echo "P3: chispa-p3.sh medir rc=$? (¿plazo?)" >&2
        fase_fin "P3 medir"
        continue
      fi
      [ -f "$CH/peticion.json" ] || { echo "P3: falta $CH/peticion.json" >&2; continue; }
      paso "P3 chispa (plazo ${PLAZO_P3_MIN:-45} min)"; entre
      "$K" image import -archive "$CH/imagen.tar" -name chispa-bench -replace || { echo "P3: falló image import" >&2; continue; }
      t0=$(ms)
      local wu=(); [ -f "$CH/warmup.jsonl" ] && wu=(-warmup "$CH/warmup.jsonl")
      timeout 1800 "$K" ai model deploy chispa -image chispa-bench -port 8090 -health /healthz -contract chispa-decide \
        -mem "${CHISPA_MEM:-1024}" -cpus "${IA_CPUS:-4}" ${wu[@]+"${wu[@]}"}; rc=$?
      jq -nc --argjson ms "$(( $(ms) - t0 ))" --argjson rc "$rc" '{dorado:"chispa", fabricar_ms:$ms, rc:$rc}' \
        > "$RES/dorado-chispa.json"
      [ "$rc" = 0 ] || { echo "P3: falló kling ai model deploy (¿falta Z5 integrada?)" >&2; continue; }
      local dor=${CHISPA_DORADO:-$("$K" snapshots -json | jq -r '[.[]|select(.name|startswith("chispa"))]|sort_by(.created_at)|last|.name // empty')}
      [ -n "$dor" ] || { echo "P3: no encuentro el dorado de chispa (CHISPA_DORADO)" >&2; continue; }
      local dpid="" dir=""
      if [ -f "$CH/directo.sh" ]; then
        bash "$CH/directo.sh" > "$RES/ia-p3-directo.log" 2>&1 & dpid=$!
        for _ in $(seq 1 60); do curl -fs -o /dev/null http://127.0.0.1:18090/healthz && break; sleep 1; done
        dir=http://127.0.0.1:18090/v1/decide
      fi
      CONTRATO=chispa-decide REQ_BODY="@$CH/peticion.json" MODELO=chispa COLD=1 LOAD_DIRECT_URL=$dir \
        RUNS=${RUNS:-10} REPLICAS=${REPLICAS:-4} SECCIONES="frio restaurar primera memoria carga" \
        JSON_OUT="$RES/ia-p3-$dor.json" timeout "$(( ${PLAZO_P3_MIN:-45} * 60 ))" bash "$VB" "$dor" \
        > "$RES/ia-p3-$dor.txt" 2>&1 || echo "P3: rc=$? (¿plazo?)" >&2
      [ -n "$dpid" ] && kill "$dpid" 2>/dev/null ;;
    s-huge|s-pmem|s-volcado)
      # Spikes de la ronda 9 (prototypes/r9-*): miden fuera del producto, con su propio Firecracker.
      local sd sp
      case $p in s-huge) sd=r9-hugepages ;; s-pmem) sd=r9-pmem-dax ;; s-volcado) sd=r9-volcado-tmpfs ;; esac
      sp=$(primero "$KB/prototypes/$sd/spike.sh" "$AQUI/../../../prototypes/$sd/spike.sh") \
        || { echo "$p: no encuentro prototypes/$sd/spike.sh" >&2; continue; }
      paso "spike $p (plazo ${PLAZO_SPIKE_MIN:-25} min)"; entre; fase_ini
      mkdir -p "$RES/spike-$sd"
      DORADO=$ultimo timeout "$(( ${PLAZO_SPIKE_MIN:-25} * 60 ))" bash "$sp" "$RES/spike-$sd" > "$RES/spike-$sd/salida.log" 2>&1; rc=$?
      fase_fin "spike $p"
      jq -nc --arg s "$p" --arg r "spike-$sd/resultado.json" --argjson rc "$rc" '{spike:$s, rc:$rc, resultado:$r}' >> "$COR/spikes.jsonl"
      [ "$rc" = 0 ] || echo "$p: rc=$rc (ver $RES/spike-$sd/salida.log)" >&2 ;;
    *) echo "prueba desconocida: $p" >&2 ;;
    esac
  done
  entre
  corrida_json
  if [ -n "$IAPY" ] && command -v python3 >/dev/null; then
    python3 -I "$IAPY" "$RES"/ia-p*.json 2>&1 | tee "$RES/resumen-ia.txt"
  fi
  paso fin medir
}

case "${1:-}" in
preparar) preparar ;;
medir) medir ;;
vigilar) shift; vigilar "$@" ;;
fabricar)
  shift
  : "${RES:?}" "${COR:?}"; JUNIT=${JUNIT:-}
  fabricar "$@" ;;
*) echo "uso: ia.sh preparar|medir|vigilar <prefijo> <dir>|fabricar <n> <desc> <orden…>" >&2; exit 2 ;;
esac
