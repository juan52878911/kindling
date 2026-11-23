#!/usr/bin/env bash
# Banco en una EC2 que se borra sola.
#
#   ec2.sh --dry-run [fase...]   enseña el plan, el coste y las llamadas a aws que haría, sin hacerlas
#   ec2.sh [--si] [fase...]      lanza, instala, mide, trae resultados y termina la instancia
#   ec2.sh --limpiar ID          borra lo que dejó un banco con ese ID (instancia, grupo, clave)
#   ec2.sh --limpiar-todo [--dry-run]   termina TODO lo etiquetado kbench en la región (instancias,
#                                peticiones spot, discos sueltos, grupos y claves)
#
# fase = kindling (por defecto) | k8s | osbk8s | docker | ia. Lanzar de verdad cuesta dinero: lo decide
# Juan. Antes de lanzar imprime el coste estimado y pide confirmación en la terminal; --si la da por dada.
#
# Doble tope contra olvidos: (1) `trap` EXIT/INT/TERM termina la instancia, borra el grupo de
# seguridad y la clave aunque el script muera a medias; (2) la instancia nace con
# InstanceInitiatedShutdownBehavior=terminate y `shutdown -h +TOPE_MIN` en el user-data, así que
# se destruye sola pase lo que pase en tu máquina. No imprime credenciales ni el token del banco.
#
# Fase ia (banco de IA, ronda 8: P1, P2 y P3 de analisis/ronda8-ia/plan.md §4). Va sola. Por defecto
# instancia SPOT de un solo uso (se termina si AWS la reclama) con precio máximo = precio de la tabla ×
# EC2_SPOT_FACTOR, disco gp3 de 40 GB que se borra con ella, tope de 150 min y un plazo por prueba
# (PLAZO_P1_MIN, PLAZO_P2_MIN, PLAZO_P3_MIN, dentro de remoto/ia.sh). Tope de gasto: el PEOR caso de esta
# instancia (precio máximo × tope de minutos + disco + IPv4) más lo ya apuntado en GASTO_LIBRO no puede
# pasar de PRESUPUESTO_USD; si pasa, se niega antes de llamar a aws. Cada lanzamiento apunta su peor
# caso en el libro (cota superior: lo real es menor). Recetas:
#   P1 anidado + P2:  BINS_DIR=… scripts/bench/ec2.sh --dry-run ia     (c8i.4xlarge)
#   P1 metal:         EC2_TYPE=c7i.metal-24xl IA_PRUEBAS=p1 TOPE_MIN=90 BINS_DIR=… scripts/bench/ec2.sh ia
#   P3:               CHISPA_DIR=… (imagen.tar, peticion.json, warmup.jsonl[, directo.sh]) añade p3
#
# Entorno (las credenciales de AWS son las normales del aws CLI y nunca se leen ni se muestran aquí):
#   AWS_REGION      región (us-east-1)          EC2_TYPE   tipo (c8i.2xlarge; ia: c8i.4xlarge)
#   EC2_CPU_OPTS    opciones de CPU             (NestedVirtualization=enabled; vacío o .metal: ninguna)
#   EC2_AMI         AMI; por defecto Debian 12 vía SSM
#   EC2_DISCO_GB    disco raíz (80; ia: 40)     TOPE_MIN   tope duro en minutos (300; ia: 150)
#   EC2_SPOT        1 = spot (ia: 1; resto: 0)  EC2_SPOT_FACTOR  precio máximo / precio de tabla (1.5)
#   EC2_PRECIO_USD_H  precio por hora si el tipo no está en la tabla (obligatorio en ia)
#   PRESUPUESTO_USD tope de gasto total (10)    GASTO_LIBRO  libro de gasto (~/.cache/kindling-bench/gasto.tsv)
#   BINS_DIR        carpeta con kling, kling-guest, kling-sandbox, 81-base-image.sh, lib-ext4-shrink.sh
#                   (ia: también ia-bench linux/amd64; si falta, se compila aquí con go)
#   SDK_DIR         SDK Python de kindling-sandbox (no en ia)    CHARTS_DIR  charts de OpenSandbox (osbk8s)
#   CHISPA_DIR      material de P3 (solo ia)
#   SALIDA          dónde se copian los resultados (./bench-out)
#   RUNS CONC SERIE EXECS POOL   parámetros del banco (ver README.md)
#   ia: IA_PRUEBAS ("p1 p2", + p3 con CHISPA_DIR), IA_MODELOS, REPLICAS, CYCLES, CLIENTES, CARGA_S
#   Barrido: PARALELO="2 4 8 16" (KLING_MAX_PARALLEL_BOOT), JAILER="0 1", CUOTAS_LISTA="- 200:50"
set -uo pipefail

AQUI=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
exec 3>&1   # las trazas de --dry-run salen por aquí aunque la llamada vaya dentro de $(…)
DRY=0; LIMPIAR=""; LIMPIAR_TODO=0; SI=0
while [ $# -gt 0 ]; do
  case $1 in
    --dry-run) DRY=1; shift ;;
    --si|--yes) SI=1; shift ;;
    --limpiar) LIMPIAR=${2:?falta ID}; shift 2 ;;
    --limpiar-todo) LIMPIAR_TODO=1; shift ;;
    --) shift; break ;;
    -*) echo "opción desconocida: $1" >&2; exit 2 ;;
    *) break ;;
  esac
done
FASES=("$@"); [ ${#FASES[@]} -gt 0 ] || FASES=(kindling)
IA=0
for f in "${FASES[@]}"; do case $f in kindling|k8s|osbk8s|docker) ;; ia) IA=1 ;; *) echo "fase desconocida: $f" >&2; exit 2 ;; esac; done
if [ "$IA" = 1 ] && [ ${#FASES[@]} -gt 1 ]; then echo "la fase ia va sola (instala otra cosa y mide otra cosa)" >&2; exit 2; fi

REGION=${AWS_REGION:-us-east-1}
if [ "$IA" = 1 ]; then
  TIPO=${EC2_TYPE:-c8i.4xlarge}; DISCO=${EC2_DISCO_GB:-40}; TOPE=${TOPE_MIN:-150}; SPOT=${EC2_SPOT:-1}
else
  TIPO=${EC2_TYPE:-c8i.2xlarge}; DISCO=${EC2_DISCO_GB:-80}; TOPE=${TOPE_MIN:-300}; SPOT=${EC2_SPOT:-0}
fi
# Solo las familias 8i (no metal) admiten virtualización anidada; un .metal no la necesita y AWS
# rechaza la opción en los tipos que no la tienen (g4dn, c7i…).
case $TIPO in *.metal*) CPU_DEF="" ;; c8i.*|m8i.*|r8i.*) CPU_DEF=NestedVirtualization=enabled ;; *) CPU_DEF="" ;; esac
CPU_OPTS=${EC2_CPU_OPTS-$CPU_DEF}
SALIDA=${SALIDA:-./bench-out}
KB=/opt/kbench
ID_BANCO=${ID_BANCO:-kbench-$(date +%Y%m%d-%H%M%S)-$$}
[ -n "$LIMPIAR" ] && ID_BANCO=$LIMPIAR
FASE_ETQ=${FASES[*]}; FASE_ETQ=${FASE_ETQ// /+}
case $TOPE in ''|*[!0-9]*) echo "TOPE_MIN debe ser un entero" >&2; exit 2 ;; esac
[ "$TOPE" -ge 10 ] && [ "$TOPE" -le 720 ] || { echo "TOPE_MIN fuera de 10..720" >&2; exit 2; }
case $DISCO in ''|*[!0-9]*) echo "EC2_DISCO_GB debe ser un entero" >&2; exit 2 ;; esac

# Pruebas de la fase ia y su plazo (min) dentro de remoto/ia.sh; la preparación se cuenta aparte.
IA_PRUEBAS=${IA_PRUEBAS:-p1 p2${CHISPA_DIR:+ p3}}
PLAZO_P1_MIN=${PLAZO_P1_MIN:-60}; PLAZO_P2_MIN=${PLAZO_P2_MIN:-45}; PLAZO_P3_MIN=${PLAZO_P3_MIN:-45}
PREPARAR_MIN=20

CLAVE=$ID_BANCO; GRUPO=$ID_BANCO
TMP=""; INST=""; SG=""
ETIQUETAS="{Key=kbench,Value=$ID_BANCO},{Key=kbench-fase,Value=$FASE_ETQ}"

aws_() {
  if [ "$DRY" = 1 ]; then
    # En seco: la llamada se imprime tal cual se haría y se contesta con un valor de mentira.
    # (Ningún argumento lleva comilla simple: basta con envolver los que tienen otros signos.)
    { printf "+ aws --region %s --no-cli-pager" "$REGION"
      for a in "$@"; do case $a in *[!A-Za-z0-9_./:=@,+-]*) printf " '%s'" "$a" ;; *) printf " %s" "$a" ;; esac; done
      echo; } >&3
    case "$1 $2" in
      "ssm get-parameter") echo ami-0dryrun ;;
      "ec2 create-security-group") echo sg-0dryrun ;;
      "ec2 run-instances") echo i-0dryrun ;;
      "ec2 create-key-pair") echo "(clave en seco)" ;;
    esac
    return 0
  fi
  aws --region "$REGION" --no-cli-pager "$@"
}

# ── coste ─────────────────────────────────────────────────────────────────────
# Precios SPOT en us-east-1 (USD/h) apuntados el 2026-11-23; se confirman antes de lanzar y se
# sobrescriben con EC2_PRECIO_USD_H. El tope real lo pone MaxPrice: AWS nunca cobra más por hora.
precio_tabla() {
  case $1 in
    c8i.4xlarge) echo 0.19 ;;
    c7i.metal-24xl) echo 0.91 ;;
    g4dn.xlarge) echo 0.21 ;;
  esac
}
PRECIO=${EC2_PRECIO_USD_H:-}
[ -n "$PRECIO" ] || { [ "$SPOT" = 1 ] && PRECIO=$(precio_tabla "$TIPO"); }
FACTOR=${EC2_SPOT_FACTOR:-1.5}
PRESUPUESTO=${PRESUPUESTO_USD:-10}
LIBRO=${GASTO_LIBRO:-$HOME/.cache/kindling-bench/gasto.tsv}
num() { case $1 in ''|*[!0-9.]*|*.*.*) return 1 ;; esac; }
GASTADO=0
[ -f "$LIBRO" ] && GASTADO=$(awk -F'\t' '{s+=$4} END {printf "%.2f", s+0}' "$LIBRO")
MAXP=""; PEOR=""; ESPERADO=""; MIN_ESP=$TOPE
if [ "$IA" = 1 ]; then
  MIN_ESP=$PREPARAR_MIN
  for p in $IA_PRUEBAS; do
    case $p in p1) MIN_ESP=$((MIN_ESP + PLAZO_P1_MIN)) ;; p2) MIN_ESP=$((MIN_ESP + PLAZO_P2_MIN)) ;;
      p3) MIN_ESP=$((MIN_ESP + PLAZO_P3_MIN)) ;; *) echo "prueba desconocida en IA_PRUEBAS: $p" >&2; exit 2 ;; esac
  done
  [ "$MIN_ESP" -le "$TOPE" ] || MIN_ESP=$TOPE
fi
if [ -n "$PRECIO" ]; then
  num "$PRECIO" && num "$FACTOR" && num "$PRESUPUESTO" || { echo "precio, factor o presupuesto no numéricos" >&2; exit 2; }
  if [ "$SPOT" = 1 ]; then MAXP=$(awk -v p="$PRECIO" -v f="$FACTOR" 'BEGIN {printf "%.4f", p*f}'); else MAXP=$PRECIO; fi
  # Disco gp3 a 0,08 USD/GB-mes (730 h) e IPv4 pública a 0,005 USD/h.
  PEOR=$(awk -v p="$MAXP" -v m="$TOPE" -v d="$DISCO" 'BEGIN {h=m/60; printf "%.2f", p*h + d*0.08/730*h + 0.005*h}')
  ESPERADO=$(awk -v p="$PRECIO" -v m="$MIN_ESP" -v d="$DISCO" 'BEGIN {h=m/60; printf "%.2f", p*h + d*0.08/730*h + 0.005*h}')
elif [ "$IA" = 1 ]; then
  echo "no hay precio de $TIPO en la tabla (spot=$SPOT): define EC2_PRECIO_USD_H" >&2; exit 2
fi

coste() {
  if [ -z "$PRECIO" ]; then
    echo "Coste: precio de $TIPO desconocido (EC2_PRECIO_USD_H); sin tope de gasto en esta fase."
    return
  fi
  local modo="bajo demanda"; [ "$SPOT" = 1 ] && modo="spot de un solo uso, precio máximo $MAXP USD/h"
  cat <<COSTE
Coste estimado ($TIPO, $modo):
  esperado   $ESPERADO USD   ($PRECIO USD/h × $MIN_ESP min + disco gp3 ${DISCO} GB + IPv4)
  peor caso  $PEOR USD   (precio máximo × tope de $TOPE min; la instancia se apaga y termina sola)
  libro      $GASTADO USD ya apuntados en $LIBRO; presupuesto $PRESUPUESTO USD
COSTE
}
if [ -n "$PEOR" ] && [ -z "$LIMPIAR" ] && [ "$LIMPIAR_TODO" = 0 ]; then
  if awk -v g="$GASTADO" -v p="$PEOR" -v t="$PRESUPUESTO" 'BEGIN {exit !(g + p > t)}'; then
    coste
    echo "se niega: $GASTADO + $PEOR USD supera el presupuesto de $PRESUPUESTO USD (PRESUPUESTO_USD, GASTO_LIBRO; baja TOPE_MIN)" >&2
    exit 3
  fi
fi

plan() {
  local fase="setup.sh y fases.sh: ${FASES[*]}"
  local sube="scripts de scripts/bench, bins (${BINS_DIR:-¡falta BINS_DIR!}), sdk (${SDK_DIR:-¡falta SDK_DIR!})"
  if [ "$IA" = 1 ]; then
    fase="remoto/ia.sh preparar y fases.sh ia: $IA_PRUEBAS (plazos p1 ${PLAZO_P1_MIN} / p2 ${PLAZO_P2_MIN} / p3 ${PLAZO_P3_MIN} min)"
    sube="scripts de scripts/bench, 96-von-bench.sh, bins (${BINS_DIR:-¡falta BINS_DIR!}) con ia-bench${CHISPA_DIR:+, chispa ($CHISPA_DIR)}"
  fi
  cat <<PLAN
== ec2.sh: plan ($ID_BANCO) ==
Crearía (región $REGION):
  1. par de claves        $CLAVE   (la .pem queda en un directorio temporal 0700)
  2. grupo de seguridad   $GRUPO   (SSH/22 solo desde la IP pública de quien lanza)
  3. instancia            $TIPO, disco gp3 ${DISCO} GB (se borra con ella), cpu-options '$CPU_OPTS', spot=$SPOT
       InstanceInitiatedShutdownBehavior=terminate
       user-data: shutdown -h +$TOPE   (tope duro de $((TOPE / 60)) h $((TOPE % 60)) min)
       etiquetas: kbench=$ID_BANCO kbench-fase=$FASE_ETQ (instancia, disco, clave y grupo)
  4. sube a $KB: $sube
  5. ejecuta $fase
     barrido: PARALELO='${PARALELO:-}' JAILER='${JAILER:-}' CUOTAS_LISTA='${CUOTAS_LISTA:--}'
  6. copia los resultados a $SALIDA
Borraría (al terminar, al fallar, en INT/TERM y, como último recurso, a las $TOPE min por apagado):
  - la instancia con etiqueta kbench=$ID_BANCO (terminate-instances y espera a 'terminated')
  - el grupo de seguridad $GRUPO
  - el par de claves $CLAVE y la .pem local
Si algo queda: ec2.sh --limpiar $ID_BANCO, o ec2.sh --limpiar-todo (todo lo etiquetado kbench).
Nunca imprime credenciales ni el token del banco.
PLAN
  coste
}

ids_texto() { tr -s '[:space:]' '\n' | grep -v '^None$' | sort -u | tr '\n' ' '; }

limpiar() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  # Se busca por etiqueta, no solo por $INST: así --limpiar ID borra también un banco que dejó otra sesión.
  local ids
  ids=$(aws_ ec2 describe-instances --filters "Name=tag:kbench,Values=$ID_BANCO" \
        "Name=instance-state-name,Values=pending,running,stopping,stopped" --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null)
  [ -n "$INST" ] && ids="$ids $INST"
  ids=$(echo "$ids" | ids_texto)
  if [ -n "${ids// /}" ]; then
    echo "== terminando instancia(s): $ids"
    # shellcheck disable=SC2086
    aws_ ec2 terminate-instances --instance-ids $ids >/dev/null
    # shellcheck disable=SC2086
    aws_ ec2 wait instance-terminated --instance-ids $ids
  fi
  local sg=$SG
  [ -n "$sg" ] || sg=$(aws_ ec2 describe-security-groups --filters "Name=group-name,Values=$GRUPO" --query 'SecurityGroups[0].GroupId' --output text 2>/dev/null)
  if [ -n "$sg" ] && [ "$sg" != None ]; then
    for _ in 1 2 3 4 5 6; do aws_ ec2 delete-security-group --group-id "$sg" 2>/dev/null && break; sleep 10; done
  fi
  aws_ ec2 delete-key-pair --key-name "$CLAVE" >/dev/null 2>&1
  [ -n "$TMP" ] && rm -rf "$TMP"
  echo "== limpio: instancia, grupo de seguridad y clave de $ID_BANCO"
  exit "$rc"
}

# limpiar_todo termina todo lo que lleve la clave de etiqueta kbench en la región, sea del banco que
# sea, y borra los grupos y claves kbench-* (también los de antes de etiquetarlos).
limpiar_todo() {
  set +e
  local ids f
  f=(--filters "Name=tag-key,Values=kbench")
  ids=$(aws_ ec2 describe-instances "${f[@]}" "Name=instance-state-name,Values=pending,running,stopping,stopped" \
        --query 'Reservations[].Instances[].InstanceId' --output text | ids_texto)
  if [ -n "${ids// /}" ]; then
    echo "== terminando instancia(s): $ids"
    # shellcheck disable=SC2086
    aws_ ec2 terminate-instances --instance-ids $ids >/dev/null
    # shellcheck disable=SC2086
    aws_ ec2 wait instance-terminated --instance-ids $ids
  fi
  ids=$(aws_ ec2 describe-spot-instance-requests "${f[@]}" "Name=state,Values=open,active" \
        --query 'SpotInstanceRequests[].SpotInstanceRequestId' --output text | ids_texto)
  # shellcheck disable=SC2086
  [ -n "${ids// /}" ] && aws_ ec2 cancel-spot-instance-requests --spot-instance-request-ids $ids >/dev/null
  for v in $(aws_ ec2 describe-volumes "${f[@]}" "Name=status,Values=available" --query 'Volumes[].VolumeId' --output text | ids_texto); do
    echo "== borrando disco suelto $v"; aws_ ec2 delete-volume --volume-id "$v"
  done
  local sgs
  sgs=$( { aws_ ec2 describe-security-groups "${f[@]}" --query 'SecurityGroups[].GroupId' --output text
           aws_ ec2 describe-security-groups --filters "Name=group-name,Values=kbench-*" --query 'SecurityGroups[].GroupId' --output text; } | ids_texto)
  for sg in $sgs; do
    for _ in 1 2 3 4 5 6; do aws_ ec2 delete-security-group --group-id "$sg" 2>/dev/null && break; sleep 10; done
  done
  for k in $( { aws_ ec2 describe-key-pairs "${f[@]}" --query 'KeyPairs[].KeyName' --output text
                aws_ ec2 describe-key-pairs --filters "Name=key-name,Values=kbench-*" --query 'KeyPairs[].KeyName' --output text; } | ids_texto); do
    aws_ ec2 delete-key-pair --key-name "$k" >/dev/null
  done
  echo "== limpio: todo lo etiquetado kbench en $REGION"
}

# lanzar crea clave, grupo e instancia. En seco solo imprime las llamadas.
lanzar() {
  AMI=${EC2_AMI:-$(aws_ ssm get-parameter --name /aws/service/debian/release/12/latest/amd64 --query Parameter.Value --output text)} || exit 1
  if [ "$DRY" = 1 ]; then MI_IP=203.0.113.10; else MI_IP=$(curl -fsS --max-time 10 https://checkip.amazonaws.com | tr -d '[:space:]') || exit 1; fi
  aws_ ec2 create-key-pair --key-name "$CLAVE" --tag-specifications "ResourceType=key-pair,Tags=[$ETIQUETAS]" \
    --query KeyMaterial --output text > "$TMP/k.pem" || exit 1
  chmod 600 "$TMP/k.pem"
  SG=$(aws_ ec2 create-security-group --group-name "$GRUPO" --description "kindling bench temporal" \
    --tag-specifications "ResourceType=security-group,Tags=[$ETIQUETAS]" --query GroupId --output text) || exit 1
  aws_ ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port 22 --cidr "$MI_IP/32" >/dev/null || exit 1
  printf '#!/bin/sh\nshutdown -h +%s "tope kindling-bench"\n' "$TOPE" > "$TMP/ud.sh"
  # La CPU anidada va en --cpu-options; si EC2_CPU_OPTS está vacío se omite.
  local CPU=() MERCADO=() TAGS=(--tag-specifications "ResourceType=instance,Tags=[$ETIQUETAS]" "ResourceType=volume,Tags=[$ETIQUETAS]")
  [ -n "$CPU_OPTS" ] && CPU=(--cpu-options "$CPU_OPTS")
  if [ "$SPOT" = 1 ]; then
    # one-time + terminate: si AWS reclama la capacidad, la instancia se termina (no se para ni vuelve).
    MERCADO=(--instance-market-options "MarketType=spot,SpotOptions={MaxPrice=$MAXP,SpotInstanceType=one-time,InstanceInterruptionBehavior=terminate}")
    TAGS+=("ResourceType=spot-instances-request,Tags=[$ETIQUETAS]")
  fi
  INST=$(aws_ ec2 run-instances --image-id "$AMI" --instance-type "$TIPO" --key-name "$CLAVE" --security-group-ids "$SG" \
    --instance-initiated-shutdown-behavior terminate ${CPU[@]+"${CPU[@]}"} ${MERCADO[@]+"${MERCADO[@]}"} --user-data "file://$TMP/ud.sh" \
    --block-device-mappings "DeviceName=/dev/xvda,Ebs={VolumeSize=$DISCO,VolumeType=gp3,DeleteOnTermination=true}" \
    "${TAGS[@]}" --query 'Instances[0].InstanceId' --output text) || exit 1
}

if [ "$LIMPIAR_TODO" = 1 ]; then
  [ "$DRY" = 1 ] || command -v aws >/dev/null || { echo "falta el aws CLI" >&2; exit 2; }
  limpiar_todo; exit 0
fi
if [ -n "$LIMPIAR" ]; then
  [ "$DRY" = 1 ] || command -v aws >/dev/null || { echo "falta el aws CLI" >&2; exit 2; }
  limpiar
fi

if [ "$DRY" = 1 ]; then
  plan
  echo "== llamadas que haría (en seco; no se ejecuta ninguna) =="
  TMP=$(mktemp -d); chmod 700 "$TMP"
  [ "$IA" = 1 ] && [ ! -x "${BINS_DIR:-/nonexistent}/ia-bench" ] && \
    echo "+ (cd scripts/.. && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o \$TMP/ia-bench ./scripts/ia-bench)"
  lanzar
  echo "+ apunta $PEOR USD en $LIBRO"
  echo "+ ssh admin@<ip>: sube scripts y bins a $KB"
  if [ "$IA" = 1 ]; then
    echo "+ ssh admin@<ip>: sudo bash $KB/remoto/ia.sh preparar"
    echo "+ ssh admin@<ip>: sudo KB=$KB IA_PRUEBAS='$IA_PRUEBAS' bash $KB/remoto/fases.sh ia"
  else
    echo "+ ssh admin@<ip>: sudo bash $KB/remoto/setup.sh"
    echo "+ ssh admin@<ip>: sudo bash $KB/remoto/fases.sh ${FASES[*]}"
  fi
  echo "+ copia $KB/res a $SALIDA/res"
  limpiar
fi

command -v aws >/dev/null || { echo "falta el aws CLI" >&2; exit 2; }
: "${BINS_DIR:?define BINS_DIR}"
[ -x "$BINS_DIR/kling" ] || { echo "no hay $BINS_DIR/kling" >&2; exit 2; }
[ "$IA" = 1 ] || : "${SDK_DIR:?define SDK_DIR}"
if [ -n "${CHISPA_DIR:-}" ]; then [ -f "$CHISPA_DIR/imagen.tar" ] || { echo "falta $CHISPA_DIR/imagen.tar" >&2; exit 2; }; fi

plan
# Confirmación antes de la primera llamada a aws: lanzar cuesta dinero.
if [ "$SI" != 1 ]; then
  if [ ! -t 0 ]; then echo "sin terminal para confirmar el gasto: repite con --si si de verdad quieres lanzar" >&2; exit 2; fi
  printf '¿Lanzar %s (peor caso %s USD)? Escribe «si» para seguir: ' "$TIPO" "${PEOR:-?}"
  read -r resp
  case $resp in si|sí|yes) ;; *) echo "no se lanza nada" >&2; exit 2 ;; esac
fi
TMP=$(mktemp -d); chmod 700 "$TMP"
IA_BENCH=""
if [ "$IA" = 1 ]; then
  if [ -x "$BINS_DIR/ia-bench" ]; then IA_BENCH=$BINS_DIR/ia-bench
  else
    command -v go >/dev/null || { echo "falta $BINS_DIR/ia-bench y no hay go para compilarlo" >&2; rm -rf "$TMP"; exit 2; }
    (cd "$AQUI/../.." && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$TMP/ia-bench" ./scripts/ia-bench) || { rm -rf "$TMP"; exit 1; }
    IA_BENCH=$TMP/ia-bench
  fi
fi
trap limpiar EXIT INT TERM
lanzar
if [ -n "$PEOR" ]; then
  mkdir -p "$(dirname "$LIBRO")" && printf '%s\t%s\t%s\t%s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$ID_BANCO" "$TIPO" "$PEOR" >> "$LIBRO"
fi
echo "== instancia $INST lanzada (tope duro: $TOPE min)"
aws_ ec2 wait instance-running --instance-ids "$INST" || exit 1
IP=$(aws_ ec2 describe-instances --instance-ids "$INST" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text) || exit 1

SSH=(ssh -i "$TMP/k.pem" -o StrictHostKeyChecking=no -o UserKnownHostsFile="$TMP/kh" -o ConnectTimeout=10 -o ServerAliveInterval=30)
for _ in $(seq 1 40); do "${SSH[@]}" "admin@$IP" true 2>/dev/null && break; sleep 5; done
"${SSH[@]}" "admin@$IP" true || { echo "sin SSH" >&2; exit 1; }
# ssh junta sus argumentos con espacios y la shell remota los vuelve a partir: sin
# entrecomillar, `R sh -c "a && b"` llegaba como `sh -c a && b`. printf %q los protege.
R() { "${SSH[@]}" "admin@$IP" "sudo $(printf '%q ' "$@")"; }
sube() { # sube <fichero local> <ruta remota>
  "${SSH[@]}" "admin@$IP" "cat > $2" < "$1"
}

R mkdir -p "$KB/sube" "$KB/res" "$KB/run"
tar czf "$TMP/sube.tgz" -C "$AQUI" bench.py bench_k8s.py fases.py ia.py osbk8s remoto -C "$AQUI/.." 96-von-bench.sh || exit 1
sube "$TMP/sube.tgz" /tmp/sube.tgz || exit 1
tar czf "$TMP/bins.tgz" -C "$(dirname "$BINS_DIR")" "$(basename "$BINS_DIR")" || exit 1
sube "$TMP/bins.tgz" /tmp/bins.tgz || exit 1
R sh -c "tar xzf /tmp/sube.tgz -C $KB && mkdir -p $KB/sube/bins && tar xzf /tmp/bins.tgz -C $KB/sube/bins --strip-components=1" || exit 1
if [ "$IA" = 1 ]; then
  sube "$IA_BENCH" /tmp/ia-bench && R install -m755 /tmp/ia-bench "$KB/sube/bins/ia-bench" || exit 1
  if [ -n "${CHISPA_DIR:-}" ]; then
    tar czf "$TMP/ch.tgz" -C "$(dirname "$CHISPA_DIR")" "$(basename "$CHISPA_DIR")" || exit 1
    sube "$TMP/ch.tgz" /tmp/chispa.tgz && R sh -c "mkdir -p $KB/sube/chispa && tar xzf /tmp/chispa.tgz -C $KB/sube/chispa --strip-components=1" || exit 1
  fi
  R bash "$KB/remoto/ia.sh" preparar || exit 1
else
  tar czf "$TMP/sdk.tgz" -C "$(dirname "$SDK_DIR")" "$(basename "$SDK_DIR")" || exit 1
  sube "$TMP/sdk.tgz" /tmp/sdk.tgz || exit 1
  R sh -c "mkdir -p $KB/sube/sdk && tar xzf /tmp/sdk.tgz -C $KB/sube/sdk --strip-components=1" || exit 1
  if [ -n "${CHARTS_DIR:-}" ]; then
    tar czf "$TMP/ch.tgz" -C "$(dirname "$CHARTS_DIR")" "$(basename "$CHARTS_DIR")" || exit 1
    sube "$TMP/ch.tgz" /tmp/ch.tgz && R sh -c "mkdir -p $KB/sube/charts && tar xzf /tmp/ch.tgz -C $KB/sube/charts --strip-components=1"
  fi
  R bash "$KB/remoto/setup.sh" || exit 1
fi
# Solo se reenvían variables del banco, nunca el entorno entero del que lanza.
PASO="KB=$KB"
for v in RUNS CONC SERIE EXECS POOL ESPERA_POOL PARALELO JAILER CUOTAS_LISTA \
         IA_PRUEBAS IA_MODELOS REPLICAS CYCLES CLIENTES CARGA_S PLAZO_P1_MIN PLAZO_P2_MIN PLAZO_P3_MIN; do
  [ -n "${!v:-}" ] && PASO="$PASO $v='${!v}'"
done
rc=0
for f in "${FASES[@]}"; do
  R sh -c "$PASO bash $KB/remoto/fases.sh $f" || rc=$?
done
mkdir -p "$SALIDA"
"${SSH[@]}" "admin@$IP" "sudo tar czf - -C $KB res" | tar xzf - -C "$SALIDA" && echo "== resultados en $SALIDA/res"
if [ "$IA" = 1 ] && command -v python3 >/dev/null; then
  python3 -I "$AQUI/ia.py" "$SALIDA"/res/ia-*.json
fi
exit $rc
