#!/usr/bin/env bash
# Gateway de kindling-sandbox en 127.0.0.1:18099 sobre el daemon privado kt-bench.
# Variables: KB (/opt/kbench), POOL (tamaño del pool de la plantilla bench-pool, 10),
# CUOTAS (vacío = inquilino sin cuotas; p. ej. "100:20" = max_sandboxes:max_por_plantilla).
# El token se genera aquí (0600) y no se imprime nunca.
set -euo pipefail
export KLING_HOST=unix:///run/kt-bench/kling.sock KLING_CONFIG=/srv/kt-bench/config.json
KB=${KB:-/opt/kbench}; B=$KB/bins/kling-sandbox; D=$KB/run
mkdir -p "$D"
[ -f "$D/ktok" ] || (umask 077; head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$D/ktok")
pkill -f "[k]ling-sandbox sbx gateway -listen 127.0.0.1:18099" || true; sleep 0.5
printf '{"name":"bench","image":"toolchain","pool":0,"mem_mib":256,"steps":[{"cmd":["true"]}]}' > "$D/t1.json"
printf '{"name":"bench-pool","image":"toolchain","pool":%s,"mem_mib":256,"steps":[{"cmd":["true"]}]}' "${POOL:-10}" > "$D/t2.json"
$B sbx template apply -f "$D/t1.json" | tail -1
$B sbx template apply -f "$D/t2.json" | tail -1
# Formato de KLING_SANDBOX_TENANTS: id:token[:max_sandboxes[:max_por_plantilla...]]
T="bench:$(cat "$D/ktok")"
[ -n "${CUOTAS:-}" ] && T="$T:${CUOTAS}"
KLING_SANDBOX_TENANTS="$T" setsid nohup "$B" sbx gateway -listen 127.0.0.1:18099 -image toolchain > "$D/gw.log" 2>&1 < /dev/null &
sleep 2; curl -s 127.0.0.1:18099/healthz; echo
