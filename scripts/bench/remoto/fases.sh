#!/usr/bin/env bash
# Fases del banco en la EC2, una por sistema para que no compitan entre sí.
# Uso: fases.sh kindling|k8s|osbk8s|docker   (como root). Resultados en $KB/res/ (KB=/opt/kbench).
set -uo pipefail
F=$1; KB=${KB:-/opt/kbench}; export KB; RUNS=${RUNS:-3}; export CONC=${CONC:-10,20,50,50}
mkdir -p $KB/res; cd "$KB"
exec > >(tee -a $KB/res/fase-$F.log) 2>&1
paso(){ echo "== $(date +%T) $*"; }
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
PY=$KB/osb/bin/python
resumen(){ grep -h '^{' "$@" | $PY -c 'import json,sys
for l in sys.stdin:
  d=json.loads(l); e=d.pop("errores",[]); print(len(e),"errores",json.dumps(d))'; }

k3s_arriba(){
  systemctl is-active --quiet k3s || systemd-run --unit k3s /usr/local/bin/k3s server --disable traefik \
    --disable servicelb --disable metrics-server --write-kubeconfig-mode 644 >/dev/null
  for _ in $(seq 1 120); do k3s kubectl get nodes 2>/dev/null | grep -q " Ready" && break; sleep 2; done
  k3s kubectl get nodes
}
k3s_abajo(){
  systemctl stop k3s 2>/dev/null; pkill -f containerd-shim; sleep 3
  grep -o ' /run/k3s[^ ]*\| /var/lib/kubelet[^ ]*' /proc/mounts | sort -r | xargs -r -n1 umount -l 2>/dev/null
  ip link del cni0 2>/dev/null; ip link del flannel.1 2>/dev/null; true
}
osb_server(){ # $1 = fichero de config
  systemctl stop osb 2>/dev/null; systemctl reset-failed osb 2>/dev/null
  cp "$1" "$HOME/.sandbox.toml"
  systemd-run --unit osb -E HOME=$HOME -E OPENSANDBOX_INSECURE_SERVER=YES $KB/osb/bin/opensandbox-server >/dev/null
  for _ in $(seq 1 60); do curl -s -o /dev/null 127.0.0.1:18080/health && break; sleep 1; done
}

case $F in
kindling)
  # Barrido: PARALELO (KLING_MAX_PARALLEL_BOOT), JAILER (0/1) y CUOTAS (vacío = sin cuotas;
  # "none" explícito no existe: usa "" y, por ejemplo, "200:50" para el inquilino con cuotas).
  # Una vuelta del barrido = daemon nuevo + gateway nuevo, así cada punto parte limpio.
  PARALELO=${PARALELO:-""}; JAILER=${JAILER:-""}; CUOTAS_LISTA=${CUOTAS_LISTA:-"-"}
  for par in ${PARALELO:-def}; do for jl in ${JAILER:-def}; do for cu in $CUOTAS_LISTA; do
    etq="par${par}-jail${jl}-cuota${cu}"; etq=${etq//:/_}
    paso "daemon kindling $etq"
    [ "$par" = def ] || export KLING_MAX_PARALLEL_BOOT=$par
    [ "$jl" = def ] || export KLING_JAILER=$jl
    bash "$KB/remoto/higiene.sh" inicio
    bash "$KB/remoto/daemon-up.sh"
    export KLING_HOST=unix:///run/kt-bench/kling.sock KLING_CONFIG=/srv/kt-bench/config.json
    /srv/kt-bench/bin/kling image ls 2>/dev/null | grep -q toolchain || /srv/kt-bench/bin/kling image toolchain
    CUOTAS=$([ "$cu" = - ] && echo "" || echo "$cu") POOL=${POOL:-10} bash "$KB/remoto/gw-up.sh"
    export KTOK=$(cat "$KB/run/ktok"); sleep 20   # que se llene el pool
    for r in $(seq 1 $RUNS); do for m in k kpool; do
      paso "$m $r $etq"; timeout 1800 python3 -I "$KB/bench.py" $m > "$KB/res/kindling-$etq-$m-$r.log" 2>&1; sleep 15
    done; done
    bash "$KB/remoto/higiene.sh" rm   # kling rm -f de todo antes de parar
    pkill -f "[k]ling-sandbox sbx gateway"; bash "$KB/remoto/daemon-down.sh"
    bash "$KB/remoto/higiene.sh" verificar || { echo "banco abortado: restos de $etq" >&2; exit 1; }
    unset KLING_MAX_PARALLEL_BOOT KLING_JAILER
  done; done; done
  resumen "$KB"/res/kindling-*.log ;;
k8s)
  k3s_arriba
  k3s crictl pull docker.io/library/alpine:3.20 >/dev/null
  for r in $(seq 1 $RUNS); do paso "pod $r"; timeout 1800 python3 -I "$KB/bench_k8s.py" > $KB/res/k8s-pod-$r.log 2>&1; sleep 10; done
  resumen $KB/res/k8s-pod-*.log ;;
osbk8s)
  k3s_arriba
  if ! helm status osb-ctl >/dev/null 2>&1; then
    paso helm OSB
    helm install osb-base $KB/charts/base >/dev/null
    helm install osb-ctl $KB/charts/controller --set controller.image.repository=docker.io/opensandbox/controller \
      --set controller.image.tag=release-1.2.1 >/dev/null
    k3s kubectl create namespace opensandbox 2>/dev/null
    k3s kubectl -n opensandbox-system rollout status deploy --timeout=300s 2>&1 | tail -1
    for i in docker.io/library/alpine:3.20 docker.io/opensandbox/execd:v1.1.0 \
      sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/task-executor:v0.1.0 \
      sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/execd:v1.1.0; do k3s crictl pull $i >/dev/null || echo "fallo pull $i"; done
    k3s kubectl apply -f $KB/osbk8s/pool.yaml
  fi
  osb_server $KB/osbk8s/sandbox.toml
  limpiar(){
    k3s kubectl delete batchsandbox -n opensandbox --all --wait=true >/dev/null 2>&1
    for _ in $(seq 1 150); do
      a=$(k3s kubectl get pool bench -n opensandbox -o jsonpath='{.status.available}' 2>/dev/null)
      n=$(k3s kubectl get pods -n opensandbox --no-headers 2>/dev/null | grep -vc Running)
      [ "${a:-0}" -ge 10 ] && [ "$n" -eq 0 ] && break; sleep 2
    done; sleep 10
  }
  for r in $(seq 1 $RUNS); do for m in osb osbpool; do
    limpiar; paso "$m $r"
    SIN_PAUSA=1 timeout 1800 $PY -I "$KB/bench.py" $m > $KB/res/osbk8s-$m-$r.log 2>&1
  done; done
  limpiar; systemctl stop osb
  resumen $KB/res/osbk8s-*.log ;;
docker)
  k3s_abajo
  systemctl start docker
  for i in opensandbox/execd:v1.1.0 opensandbox/egress:v1.1.7 alpine:3.20; do docker pull -q $i >/dev/null; done
  sed -e 's/^type = "kubernetes"/type = "docker"/' -e '/^\[kubernetes\]/,/^batchsandbox_template_file/d' \
    $KB/osbk8s/sandbox.toml > $KB/osbk8s/sandbox-docker.toml
  printf '[docker]\nnetwork_mode = "bridge"\n' >> $KB/osbk8s/sandbox-docker.toml
  osb_server $KB/osbk8s/sandbox-docker.toml
  for r in $(seq 1 $RUNS); do paso "osb docker $r"
    timeout 2400 $PY -I "$KB/bench.py" osb > $KB/res/docker-osb-$r.log 2>&1
    docker ps -aq --filter label=opensandbox.io/id | xargs -r docker rm -f >/dev/null 2>&1; sleep 10
  done
  systemctl stop osb docker docker.socket
  resumen $KB/res/docker-osb-*.log ;;
esac
paso fin $F
