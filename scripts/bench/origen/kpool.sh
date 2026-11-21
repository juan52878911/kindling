#!/usr/bin/env bash
set -uo pipefail
export CONC=10,20,50 ESPERA_POOL=45 KLING_HOST=unix:///run/kt-bench/kling.sock KLING_CONFIG=/srv/kt-bench/config.json
cd /root/bench2; R=/root/res
bash /root/bench/kt-bench-up.sh >/dev/null
/srv/kt-bench/bin/kling image toolchain >/dev/null 2>&1
POOL=10 bash /root/bench2/gw-up.sh; export KTOK=$(cat ktok); sleep 40
for r in 1 2; do timeout 1800 python3 -I bench.py kpool > $R/espera-kpool-$r.log 2>&1; sleep 40; done
pkill -f "[k]ling-sandbox sbx gateway"; bash /root/bench/kt-bench-down.sh >/dev/null; echo FIN
