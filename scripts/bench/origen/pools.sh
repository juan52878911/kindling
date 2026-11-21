#!/usr/bin/env bash
# Repite kpool y osbpool dejando 45 s de relleno antes de cada ráfaga.
set -uo pipefail
export CONC=10,20,50 ESPERA_POOL=45 KUBECONFIG=/etc/rancher/k3s/k3s.yaml
cd /root/bench2; R=/root/res
bash /root/bench/kt-bench-up.sh >/dev/null
POOL=10 bash /root/bench2/gw-up.sh >/dev/null; export KTOK=$(cat ktok); sleep 40
for r in 1 2; do timeout 1800 python3 -I bench.py kpool > $R/espera-kpool-$r.log 2>&1; sleep 40; done
pkill -f "[k]ling-sandbox sbx gateway"; bash /root/bench/kt-bench-down.sh >/dev/null
cp /root/osbk8s/sandbox.toml /root/.sandbox.toml; systemctl reset-failed osb 2>/dev/null
systemd-run --unit osb -E HOME=/root -E OPENSANDBOX_INSECURE_SERVER=YES /root/osb/bin/opensandbox-server >/dev/null; sleep 8
for r in 1 2; do
  k3s kubectl delete batchsandbox -n opensandbox --all --wait=true >/dev/null 2>&1; sleep 45
  SIN_PAUSA=1 timeout 1800 /root/osb/bin/python -I bench.py osbpool > $R/espera-osbpool-$r.log 2>&1
done
systemctl stop osb; echo FIN
