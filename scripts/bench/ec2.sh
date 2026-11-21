#!/usr/bin/env bash
# Banco en una EC2 anidada que se borra sola.
#
#   ec2.sh --dry-run [fase...]   enseña qué crearía y qué borraría; no llama a AWS
#   ec2.sh [fase...]             lanza, instala, mide, trae resultados y termina la instancia
#   ec2.sh --limpiar ID          borra lo que dejó un banco con ese ID (instancia, grupo, clave)
#
# fase = kindling (por defecto) | k8s | osbk8s | docker. Lanzar de verdad cuesta dinero: lo decide Juan.
#
# Doble tope contra olvidos: (1) `trap` EXIT/INT/TERM termina la instancia, borra el grupo de
# seguridad y la clave aunque el script muera a medias; (2) la instancia nace con
# InstanceInitiatedShutdownBehavior=terminate y `shutdown -h +TOPE_MIN` en el user-data, así que
# se destruye sola pase lo que pase en tu máquina. No imprime credenciales ni el token del banco.
#
# Entorno (las credenciales de AWS son las normales del aws CLI y nunca se leen ni se muestran aquí):
#   AWS_REGION      región (us-east-1)          EC2_TYPE   tipo (c8i.2xlarge)
#   EC2_CPU_OPTS    opciones de CPU             (NestedVirtualization=enabled)
#   EC2_AMI         AMI; por defecto Debian 12 vía SSM
#   EC2_DISCO_GB    disco raíz (80)             TOPE_MIN   tope duro en minutos (300)
#   BINS_DIR        carpeta con kling, kling-guest, kling-sandbox, 81-base-image.sh, lib-ext4-shrink.sh
#   SDK_DIR         SDK Python de kindling-sandbox      CHARTS_DIR  charts de OpenSandbox (solo osbk8s)
#   SALIDA          dónde se copian los resultados (./bench-out)
#   RUNS CONC SERIE EXECS POOL   parámetros del banco (ver README.md)
#   Barrido: PARALELO="2 4 8 16" (KLING_MAX_PARALLEL_BOOT), JAILER="0 1", CUOTAS_LISTA="- 200:50"
set -uo pipefail

AQUI=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REGION=${AWS_REGION:-us-east-1}
TIPO=${EC2_TYPE:-c8i.2xlarge}
CPU_OPTS=${EC2_CPU_OPTS:-NestedVirtualization=enabled}
DISCO=${EC2_DISCO_GB:-80}
TOPE=${TOPE_MIN:-300}
SALIDA=${SALIDA:-./bench-out}
KB=/opt/kbench
ID_BANCO=${ID_BANCO:-kbench-$(date +%Y%m%d-%H%M%S)-$$}
DRY=0; LIMPIAR=""
case "${1:-}" in
  --dry-run) DRY=1; shift ;;
  --limpiar) LIMPIAR=${2:?falta ID}; ID_BANCO=$LIMPIAR; shift 2 ;;
esac
FASES=("$@"); [ ${#FASES[@]} -gt 0 ] || FASES=(kindling)
for f in "${FASES[@]}"; do case $f in kindling|k8s|osbk8s|docker) ;; *) echo "fase desconocida: $f" >&2; exit 2 ;; esac; done
case $TOPE in ''|*[!0-9]*) echo "TOPE_MIN debe ser un entero" >&2; exit 2 ;; esac
[ "$TOPE" -ge 10 ] && [ "$TOPE" -le 720 ] || { echo "TOPE_MIN fuera de 10..720" >&2; exit 2; }

CLAVE=$ID_BANCO; GRUPO=$ID_BANCO
TMP=""; INST=""; SG=""
aws_() { aws --region "$REGION" --no-cli-pager "$@"; }

plan() {
  cat <<PLAN
== ec2.sh: plan ($ID_BANCO) ==
Crearía (región $REGION):
  1. par de claves        $CLAVE   (la .pem queda en un directorio temporal 0700)
  2. grupo de seguridad   $GRUPO   (SSH/22 solo desde la IP pública de quien lanza)
  3. instancia            $TIPO, disco ${DISCO} GB, cpu-options '$CPU_OPTS'
       InstanceInitiatedShutdownBehavior=terminate
       user-data: shutdown -h +$TOPE   (tope duro de $((TOPE / 60)) h $((TOPE % 60)) min)
       etiqueta: kbench=$ID_BANCO
  4. sube a $KB: scripts de scripts/bench, bins (${BINS_DIR:-¡falta BINS_DIR!}), sdk (${SDK_DIR:-¡falta SDK_DIR!})
  5. ejecuta setup.sh y fases.sh: ${FASES[*]}
     barrido: PARALELO='${PARALELO:-}' JAILER='${JAILER:-}' CUOTAS_LISTA='${CUOTAS_LISTA:--}'
  6. copia los resultados a $SALIDA
Borraría (al terminar, al fallar, en INT/TERM y, como último recurso, a las $TOPE min por apagado):
  - la instancia con etiqueta kbench=$ID_BANCO (terminate-instances y espera a 'terminated')
  - el grupo de seguridad $GRUPO
  - el par de claves $CLAVE y la .pem local
Nunca imprime credenciales ni el token del banco.
PLAN
}

limpiar() {
  local rc=$?
  trap - EXIT INT TERM
  set +e
  # Se busca por etiqueta, no solo por $INST: así --limpiar ID borra también un banco que dejó otra sesión.
  local ids
  ids=$(aws_ ec2 describe-instances --filters "Name=tag:kbench,Values=$ID_BANCO" \
        "Name=instance-state-name,Values=pending,running,stopping,stopped" --query 'Reservations[].Instances[].InstanceId' --output text 2>/dev/null)
  [ -n "$INST" ] && ids="$ids $INST"
  ids=$(echo "$ids" | tr -s '[:space:]' '\n' | sort -u | tr '\n' ' ')
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

if [ "$DRY" = 1 ]; then plan; exit 0; fi
command -v aws >/dev/null || { echo "falta el aws CLI" >&2; exit 2; }
if [ -n "$LIMPIAR" ]; then trap limpiar EXIT; exit 0; fi
: "${BINS_DIR:?define BINS_DIR}"; : "${SDK_DIR:?define SDK_DIR}"
[ -x "$BINS_DIR/kling" ] || { echo "no hay $BINS_DIR/kling" >&2; exit 2; }

plan
trap limpiar EXIT INT TERM
TMP=$(mktemp -d); chmod 700 "$TMP"
AMI=${EC2_AMI:-$(aws_ ssm get-parameter --name /aws/service/debian/release/12/latest/amd64 --query Parameter.Value --output text)} || exit 1
MI_IP=$(curl -fsS --max-time 10 https://checkip.amazonaws.com | tr -d '[:space:]') || exit 1

aws_ ec2 create-key-pair --key-name "$CLAVE" --query KeyMaterial --output text > "$TMP/k.pem" || exit 1
chmod 600 "$TMP/k.pem"
SG=$(aws_ ec2 create-security-group --group-name "$GRUPO" --description "kindling bench temporal" --query GroupId --output text) || exit 1
aws_ ec2 authorize-security-group-ingress --group-id "$SG" --protocol tcp --port 22 --cidr "$MI_IP/32" >/dev/null || exit 1
printf '#!/bin/sh\nshutdown -h +%s "tope kindling-bench"\n' "$TOPE" > "$TMP/ud.sh"
# La CPU anidada va en --cpu-options; si EC2_CPU_OPTS está vacío se omite.
CPU=(); [ -n "$CPU_OPTS" ] && CPU=(--cpu-options "$CPU_OPTS")
INST=$(aws_ ec2 run-instances --image-id "$AMI" --instance-type "$TIPO" --key-name "$CLAVE" --security-group-ids "$SG" \
  --instance-initiated-shutdown-behavior terminate "${CPU[@]}" --user-data "file://$TMP/ud.sh" \
  --block-device-mappings "DeviceName=/dev/xvda,Ebs={VolumeSize=$DISCO,VolumeType=gp3,DeleteOnTermination=true}" \
  --tag-specifications "ResourceType=instance,Tags=[{Key=kbench,Value=$ID_BANCO}]" \
  --query 'Instances[0].InstanceId' --output text) || exit 1
echo "== instancia $INST lanzada (tope duro: $TOPE min)"
aws_ ec2 wait instance-running --instance-ids "$INST" || exit 1
IP=$(aws_ ec2 describe-instances --instance-ids "$INST" --query 'Reservations[0].Instances[0].PublicIpAddress' --output text) || exit 1

SSH=(ssh -i "$TMP/k.pem" -o StrictHostKeyChecking=no -o UserKnownHostsFile="$TMP/kh" -o ConnectTimeout=10 -o ServerAliveInterval=30)
for _ in $(seq 1 40); do "${SSH[@]}" "admin@$IP" true 2>/dev/null && break; sleep 5; done
"${SSH[@]}" "admin@$IP" true || { echo "sin SSH" >&2; exit 1; }
R() { "${SSH[@]}" "admin@$IP" sudo "$@"; }

R mkdir -p "$KB/sube" "$KB/res" "$KB/run"
tar czf "$TMP/sube.tgz" -C "$AQUI" bench.py bench_k8s.py fases.py osbk8s remoto || exit 1
"${SSH[@]}" "admin@$IP" "cat > /tmp/sube.tgz" < "$TMP/sube.tgz" || exit 1
tar czf "$TMP/bins.tgz" -C "$(dirname "$BINS_DIR")" "$(basename "$BINS_DIR")" || exit 1
"${SSH[@]}" "admin@$IP" "cat > /tmp/bins.tgz" < "$TMP/bins.tgz" || exit 1
tar czf "$TMP/sdk.tgz" -C "$(dirname "$SDK_DIR")" "$(basename "$SDK_DIR")" || exit 1
"${SSH[@]}" "admin@$IP" "cat > /tmp/sdk.tgz" < "$TMP/sdk.tgz" || exit 1
R sh -c "tar xzf /tmp/sube.tgz -C $KB && mkdir -p $KB/sube/bins $KB/sube/sdk \
  && tar xzf /tmp/bins.tgz -C $KB/sube/bins --strip-components=1 && tar xzf /tmp/sdk.tgz -C $KB/sube/sdk --strip-components=1" || exit 1
if [ -n "${CHARTS_DIR:-}" ]; then
  tar czf "$TMP/ch.tgz" -C "$(dirname "$CHARTS_DIR")" "$(basename "$CHARTS_DIR")" || exit 1
  "${SSH[@]}" "admin@$IP" "cat > /tmp/ch.tgz" < "$TMP/ch.tgz" && R sh -c "mkdir -p $KB/sube/charts && tar xzf /tmp/ch.tgz -C $KB/sube/charts --strip-components=1"
fi
R bash "$KB/remoto/setup.sh" || exit 1
# Solo se reenvían variables del banco, nunca el entorno entero del que lanza.
PASO="KB=$KB"
for v in RUNS CONC SERIE EXECS POOL ESPERA_POOL PARALELO JAILER CUOTAS_LISTA; do
  [ -n "${!v:-}" ] && PASO="$PASO $v='${!v}'"
done
rc=0
for f in "${FASES[@]}"; do
  R sh -c "$PASO bash $KB/remoto/fases.sh $f" || rc=$?
done
mkdir -p "$SALIDA"
"${SSH[@]}" "admin@$IP" "sudo tar czf - -C $KB res" | tar xzf - -C "$SALIDA" && echo "== resultados en $SALIDA/res"
exit $rc
