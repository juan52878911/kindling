#!/usr/bin/env bash
# Corre bench.py osb/osbpool en k3s dejando el espacio de nombres limpio entre corridas.
export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
cd /root/bench2
limpiar() {
  k3s kubectl delete batchsandbox -n opensandbox --all --wait=true >/dev/null 2>&1
  for _ in $(seq 1 90); do
    a=$(k3s kubectl get pool bench -n opensandbox -o jsonpath='{.status.available}' 2>/dev/null)
    n=$(k3s kubectl get pods -n opensandbox --no-headers 2>/dev/null | grep -vc Running)
    [ "${a:-0}" -ge 10 ] && [ "$n" -eq 0 ] && break; sleep 2
  done
  sleep 10
}
for r in 1 2; do for m in osb osbpool; do
  limpiar
  SIN_PAUSA=1 timeout 1500 /root/osb/bin/python -I bench.py $m > vm-$m-$r.log 2>&1
done; done
limpiar
for f in vm-osb-*.log vm-osbpool-*.log; do echo "== $f"; grep '^{' $f | /root/osb/bin/python -c 'import json,sys; d=json.load(sys.stdin); e=d.pop("errores",[]); print(len(e), json.dumps(d))' || tail -2 $f; done
