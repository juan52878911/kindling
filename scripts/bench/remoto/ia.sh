#!/usr/bin/env bash
# Banco de IA (ronda 8: P1, P2 y P3 de analisis/ronda8-ia/plan.md §4) en el host del banco.
#
#   ia.sh preparar   (root, solo en la instancia del banco) paquetes, Firecracker y las imágenes de
#                    kindling; nada de k3s ni Docker
#   ia.sh medir      dorados de IA_MODELOS y las pruebas de IA_PRUEBAS, cada una con su plazo, con un
#                    JSON y un .txt por prueba en $KB/res y el resumen de ia.py en res/resumen-ia.txt
#
# En la EC2 (lo llaman ec2.sh y fases.sh ia) levanta el daemon privado kt-bench y hace higiene entre
# variantes (`pkill -9 -x firecracker`). En un host Linux cualquiera con kindling (el lab):
#
#   USAR_DAEMON=1 KB=$HOME/kb IA_MODELOS=von-smol IA_PRUEBAS="p1 p2" bash scripts/bench/remoto/ia.sh medir
#
# usa el daemon al que apunte kling (KLING_HOST o su contexto) y NO mata procesos ni para nada.
#
# Entorno: KB (/opt/kbench), IA_PRUEBAS ("p1 p2", p3 si hay $KB/sube/chispa), IA_MODELOS (lista de
# «nombre=id:quant» para `kling ai model add` o «nombre» de un dorado que ya existe; por defecto los
# Qwen2.5-1.5B Q4_K_M y Q8_0), IA_CPUS (4), RUNS (10), REPLICAS (4), CYCLES (20), MEM_RONDAS (2),
# CLIENTES, CARGA_S, PLAZO_P1_MIN (60), PLAZO_P2_MIN (45), PLAZO_P3_MIN (45), y para P3 el material en
# $KB/sube/chispa: imagen.tar (imagen OCI de chispa-server con modelos y config horneados), peticion.json,
# warmup.jsonl y, opcional, directo.sh (el mismo binario en el host, escuchando en 127.0.0.1:18090).
set -uo pipefail
KB=${KB:-/opt/kbench}; export KB
AQUI=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
RES=$KB/res
paso() { echo "== $(date +%T) $*"; }
bajar() { curl -fsSL --retry 3 -o "$2" "$1"; }
ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }

preparar() {
  [ "$(id -u)" = 0 ] || { echo "preparar va como root" >&2; exit 2; }
  mkdir -p "$KB"; cd "$KB" || exit 1
  exec > >(tee -a "$KB/setup-ia.log") 2>&1
  paso apt
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq jq perl curl zstd python3 iptables ipset conntrack e2fsprogs xfsprogs >/dev/null || exit 1
  paso firecracker
  local F=v1.17.0
  bajar "https://github.com/firecracker-microvm/firecracker/releases/download/$F/firecracker-$F-x86_64.tgz" fc.tgz || exit 1
  bajar "https://github.com/firecracker-microvm/firecracker/releases/download/$F/firecracker-$F-x86_64.tgz.sha256.txt" fc.sha || exit 1
  [ "$(sha256sum fc.tgz | cut -d' ' -f1)" = "$(cut -d' ' -f1 fc.sha)" ] || { echo "sha firecracker NO coincide"; exit 1; }
  tar xzf fc.tgz
  install -m755 "release-$F-x86_64/firecracker-$F-x86_64" /usr/local/bin/firecracker
  install -m755 "release-$F-x86_64/jailer-$F-x86_64" /usr/local/bin/jailer
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

medir() {
  mkdir -p "$RES"
  exec > >(tee -a "$RES/medir-ia.log") 2>&1
  local HIG=0
  if [ "${USAR_DAEMON:-0}" = 1 ]; then
    K=${KLING:-kling}
  else
    [ "$(id -u)" = 0 ] || { echo "sin USAR_DAEMON=1, medir levanta kt-bench y va como root" >&2; exit 2; }
    bash "$KB/remoto/higiene.sh" inicio
    bash "$KB/remoto/daemon-up.sh" || exit 1
    export KLING_HOST=unix:///run/kt-bench/kling.sock KLING_CONFIG=/srv/kt-bench/config.json
    K=/srv/kt-bench/bin/kling; HIG=1
  fi
  VB=$(primero "$KB/96-von-bench.sh" "$AQUI/../../96-von-bench.sh") || { echo "no encuentro 96-von-bench.sh" >&2; exit 2; }
  IAPY=$(primero "$KB/ia.py" "$AQUI/../ia.py") || IAPY=""
  IAB=$(primero "$KB/bins/ia-bench" "$KB/sube/bins/ia-bench" "$(command -v ia-bench 2>/dev/null || echo /nonexistent)") || IAB=""
  [ -n "$IAB" ] || echo "AVISO: sin ia-bench: se omiten remove, ociosa por gateway y carga" >&2
  export KLING=$K IA_BENCH=$IAB HIGIENE=$HIG
  command -v jq >/dev/null && command -v perl >/dev/null || { echo "faltan jq o perl" >&2; exit 2; }

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
      timeout 3600 "$K" ai model add "$n" -model "${idq%%:*}" -quant "${idq#*:}" -cpus "${IA_CPUS:-4}"; rc=$?
      jq -nc --arg n "$n" --arg m "$idq" --argjson ms "$(( $(ms) - t0 ))" --argjson rc "$rc" \
        '{dorado:$n, modelo:$m, fabricar_ms:$ms, rc:$rc}' > "$RES/dorado-$n.json"
      [ "$rc" = 0 ] || { echo "falló kling ai model add $n" >&2; continue; }
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
        paso "P1 $n (plazo ${cada} s)"; entre
        RUNS=${RUNS:-10} REPLICAS=1 SECCIONES="restaurar primera tps" JSON_OUT="$RES/ia-p1-$n.json" \
          timeout "$cada" bash "$VB" "$n" > "$RES/ia-p1-$n.txt" 2>&1 || echo "P1 $n: rc=$? (¿plazo?)" >&2
      done ;;
    p2)
      paso "P2 $ultimo (plazo ${PLAZO_P2_MIN:-45} min)"; entre
      RUNS=${RUNS:-10} REPLICAS=${REPLICAS:-4} CYCLES=${CYCLES:-20} MEM_RONDAS=${MEM_RONDAS:-2} SECCIONES="memoria ociosa" \
        JSON_OUT="$RES/ia-p2-$ultimo.json" timeout "$(( ${PLAZO_P2_MIN:-45} * 60 ))" bash "$VB" "$ultimo" \
        > "$RES/ia-p2-$ultimo.txt" 2>&1 || echo "P2: rc=$? (¿plazo?)" >&2 ;;
    p3)
      [ -f "$CH/imagen.tar" ] && [ -f "$CH/peticion.json" ] || { echo "P3: falta $CH/imagen.tar o peticion.json" >&2; continue; }
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
    *) echo "prueba desconocida: $p" >&2 ;;
    esac
  done
  entre
  if [ -n "$IAPY" ] && command -v python3 >/dev/null; then
    python3 -I "$IAPY" "$RES"/ia-p*.json | tee "$RES/resumen-ia.txt"
  fi
  paso fin medir
}

case "${1:-}" in
preparar) preparar ;;
medir) medir ;;
*) echo "uso: ia.sh preparar|medir" >&2; exit 2 ;;
esac
