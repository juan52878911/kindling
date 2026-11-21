#!/usr/bin/env bash
# Gateway de kindling-sandbox en 127.0.0.1:18099 sobre el daemon privado kt-bench.
set -euo pipefail
export KLING_HOST=unix:///run/kt-bench/kling.sock KLING_CONFIG=/srv/kt-bench/config.json
B=/root/bench/bins/kling-sandbox; D=/root/bench2
[ -f $D/ktok ] || (umask 077; head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > $D/ktok)
pkill -f "[k]ling-sandbox sbx gateway -listen 127.0.0.1:18099" || true; sleep 0.5
printf '{"name":"bench","image":"toolchain","pool":0,"mem_mib":256,"steps":[{"cmd":["true"]}]}' > $D/t1.json
printf '{"name":"bench-pool","image":"toolchain","pool":%s,"mem_mib":256,"steps":[{"cmd":["true"]}]}' "${POOL:-10}" > $D/t2.json
$B sbx template apply -f $D/t1.json | tail -1
$B sbx template apply -f $D/t2.json | tail -1
KLING_SANDBOX_TENANTS="bench:$(cat $D/ktok)" setsid nohup $B sbx gateway -listen 127.0.0.1:18099 -image toolchain > $D/gw.log 2>&1 < /dev/null &
sleep 2; curl -s 127.0.0.1:18099/healthz; echo
