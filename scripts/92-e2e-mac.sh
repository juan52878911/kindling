#!/usr/bin/env bash
# Prueba de extremo a extremo del backend nativo de macOS (kling-vz) en ESTE Mac.
#
# 90-e2e.sh habla con un daemon Linux; esta levanta su propio daemon local con
# el backend vz, en una raíz de usarse y tirarse, y recorre lo que solo se ve
# con núcleo y ayudante juntos: arranque, exec, cp, shell, congelar y
# descongelar, snapshots dorados y sus réplicas por reenvío de loopback, el
# proxy al invitado, sandboxes, squeeze, resize, egress, MMDS, reinicio del
# daemon con máquinas vivas, un kling-vz muerto de un SIGKILL, y al final una
# ráfaga corta con la compuerta de arranque. Termina comprobando que no queda
# ningún proceso ni enlace de la prueba.
#
#   KLING=./kling ./92-e2e-mac.sh                       imágenes ya en $ROOT/images
#   FROM=ssh://juan@lab-arm64 ./92-e2e-mac.sh           las copia de un daemon Linux arm64
#   IMAGES_DIR=/ruta ./92-e2e-mac.sh                    las clona (cp -c) de un directorio
#   KEEP=1 ./92-e2e-mac.sh                              no limpia (para inspeccionar)
#
# Variables: KLING (binario; kling-vz junto a él o KLING_VMM), ROOT (por defecto
# una ruta a propósito más larga que sun_path), SOCK, IMG (imagen con agente de
# invitado, por defecto toolchain), BRIDGE_IMG (imagen con puente MCP en 8080,
# por defecto fetch; si falta se salta), MEM (MiB por máquina, 256), BURST (10).
#
# La salida va en inglés, como el resto de lo que ve el usuario. Un fallo no
# aborta el resto: saber que fallan tres cosas relacionadas vale más que
# enterarse de una.
set -uo pipefail

KLING="${KLING:-kling}"
case "$KLING" in */*) KLING="$(cd "$(dirname "$KLING")" && pwd)/$(basename "$KLING")";; esac
TMPBASE="${TMPDIR:-/tmp}"; TMPBASE="${TMPBASE%/}"
ROOT="${ROOT:-$TMPBASE/kling-e2e-mac/Library/Application Support/kindling-e2e-a-deliberately-long-root}"
SOCK="${SOCK:-/tmp/kling-e2e-$(id -u).sock}"
IMG="${IMG:-toolchain}"
BRIDGE_IMG="${BRIDGE_IMG:-fetch}"
MEM="${MEM:-256}"
BURST="${BURST:-10}"
KEEP="${KEEP:-0}"
FROM="${FROM:-}"
IMAGES_DIR="${IMAGES_DIR:-}"
LOG="${LOG:-$TMPBASE/kling-e2e-mac/daemon.log}"
P="e2e-$$"   # prefijo de todo lo que crea la prueba

export KLING_HOST="unix://$SOCK"
# Que la configuración del usuario (defaults.mem_mib, defaults.ttl_seconds, el
# contexto activo...) no cambie lo que se mide: una configuración vacía.
export KLING_CONFIG="${TMPBASE}/kling-e2e-mac-config-$$.json"
export KLING_MAX_PARALLEL_BOOT="${KLING_MAX_PARALLEL_BOOT:-4}"
# Carpetas compartidas vivas: el daemon de la prueba solo sirve las de aquí.
SHARES="$TMPBASE/kling-e2e-mac/shares-$$"
export KLING_SHARE_ROOTS="$SHARES"

pass=0; fail=0
ok()   { printf "  \033[32mok\033[0m    %s\n" "$1"; pass=$((pass+1)); }
bad()  { printf "  \033[31mFAIL\033[0m  %s\n        expected: %s\n        got:      %s\n" "$1" "$2" "$3"; fail=$((fail+1)); }
step() { printf "\n\033[1m%s\033[0m\n" "$1"; }
info() { printf "        %s\n" "$1"; }

# contiene busca una subcadena SIN tuberías: `cmd | grep -q` con pipefail da
# falso negativo (grep cierra, el productor muere de SIGPIPE), y `cmd | … ||
# echo x` imprime dos veces. Por eso todo se captura en variables.
contiene() { case "$1" in *"$2"*) return 0;; *) return 1;; esac; }
now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
k() { "$KLING" "$@"; }
api() { curl -s -m 30 --unix-socket "$SOCK" "$@"; }

need() { command -v "$1" >/dev/null || { echo "missing $1" >&2; exit 1; }; }
need "$KLING"; need curl; need python3; need perl; need script
[ "$(uname -s)" = Darwin ] || { echo "this test is for macOS (backend vz); on Linux use 90-e2e.sh" >&2; exit 1; }

# jq de pobre: evalúa una expresión de Python sobre el JSON de stdin (d).
pyj() { python3 -c "import json,sys
try: d=json.load(sys.stdin)
except Exception: d=None
print($1)"; }
machine_field() { k ps -a -json 2>/dev/null | pyj "next((str(m.get('$2','')) for m in (d or []) if m['name']=='$1'), '')"; }
footprint() { api http://k/procstats | pyj "next((m['pss_mib'] for m in d['machines'] if m['name']=='$1'), 0)"; }
total_fp() { api http://k/procstats | pyj "d['total_pss_mib']"; }
apple_vms() { pgrep -f com.apple.Virtualization.VirtualMachine | wc -l | tr -d ' '; }
our_vmms() { pgrep -f "kling-vz --api-sock $ROOT/" | wc -l | tr -d ' '; }
our_links() {
  local n=0 l
  for l in /tmp/kling-"$(id -u)"/*.sock; do
    [ -L "$l" ] || continue
    case "$(readlink "$l")" in "$ROOT"/*) n=$((n+1));; esac
  done
  echo $n
}
# vivos <pids...>: si alguno sigue corriendo. No vale `wait` a secas ni
# `jobs`: el daemon también es un trabajo en segundo plano de este shell.
vivos() { local p; for p in "$@"; do kill -0 "$p" 2>/dev/null && return 0; done; return 1; }
pctl() { # pctl <p> <números...>: percentil por el método del rango más cercano
  local p=$1; shift
  printf '%s\n' "$@" | sort -n | awk -v p="$p" '{a[NR]=$1} END {i=int((p/100)*NR+0.999); if(i<1)i=1; print a[i]}'
}

DPID=""
start_daemon() {
  mkdir -p "$(dirname "$LOG")"
  "$KLING" daemon -root "$ROOT" -socket "$SOCK" >>"$LOG" 2>&1 &
  DPID=$!
  local i
  for i in $(seq 1 50); do
    k info >/dev/null 2>&1 && return 0
    kill -0 "$DPID" 2>/dev/null || break
    sleep 0.2
  done
  return 1
}
stop_daemon() {
  [ -n "$DPID" ] || return 0
  kill -TERM "$DPID" 2>/dev/null
  local i; for i in $(seq 1 50); do kill -0 "$DPID" 2>/dev/null || break; sleep 0.2; done
  DPID=""
}

cleanup() {
  if [ "$KEEP" = "1" ]; then echo; echo "KEEP=1: not cleaning up. Daemon pid $DPID, root $ROOT"; return; fi
  echo; echo "cleaning up..."
  if [ -n "$DPID" ]; then
    for m in $(k ps -a -q 2>/dev/null); do k rm "$m" >/dev/null 2>&1; done
    for s in $(k snapshots 2>/dev/null | awk -v p="$P" 'index($1,p)==1 {print $1}'); do k rmi "$s" >/dev/null 2>&1; done
  fi
  stop_daemon
  pkill -KILL -f "kling-vz --api-sock $ROOT/" 2>/dev/null
  rm -f "$KLING_CONFIG"
  rm -rf "$SHARES"
}
trap cleanup EXIT

APPLE_BEFORE=$(apple_vms)

# ── 0. imágenes y daemon ──────────────────────────────────────────────────────
step "0. Setup"
mkdir -p "$ROOT/images"
info "root: $ROOT (${#ROOT} bytes; machine sockets go past the 104 of sun_path)"
if [ -n "$IMAGES_DIR" ]; then
  for f in "$IMAGES_DIR"/vmlinux "$IMAGES_DIR"/min.ext4 "$IMAGES_DIR"/*.layer.ext4 "$IMAGES_DIR"/*.recipe.json; do
    [ -f "$f" ] && [ ! -f "$ROOT/images/$(basename "$f")" ] && cp -c "$f" "$ROOT/images/"
  done
fi
if pgrep -f "kling daemon -root $ROOT" >/dev/null; then
  echo "a daemon is already running on $ROOT; stop it first" >&2; exit 1
fi
if start_daemon; then ok "daemon started (pid $DPID, log $LOG)"; else echo "daemon did not start; see $LOG" >&2; tail -5 "$LOG" >&2; exit 1; fi
if [ -n "$FROM" ]; then
  for i in "$IMG" "$BRIDGE_IMG"; do
    out=$(k images copy "$i" -from "$FROM" 2>&1) && ok "images copy $i from $FROM" || bad "images copy $i" "copied" "$out"
  done
fi
imgs=$(k images ls 2>&1)
contiene "$imgs" "$IMG " || { echo "image $IMG is not in $ROOT/images; set FROM or IMAGES_DIR" >&2; exit 1; }
HAVE_BRIDGE=0; contiene "$imgs" "$BRIDGE_IMG " && HAVE_BRIDGE=1

# Un segundo daemon sobre la misma raíz tiene que negarse: si no, cada uno
# mata los VMM del otro como huérfanos.
out=$("$KLING" daemon -root "$ROOT" -socket "$SOCK.2" 2>&1); rc=$?
[ $rc -ne 0 ] && contiene "$out" "already running" && ok "a second daemon on the same root is refused" \
  || bad "second daemon" "refused (already running)" "rc=$rc $out"
rm -f "$SOCK.2"

# ── 1. diagnóstico ────────────────────────────────────────────────────────────
step "1. kling up / info"
out=$(KLING_ROOT="$ROOT" k up -check 2>&1); rc=$?
contiene "$out" "local runtime on macOS (vz)" && ok "up -check diagnoses the Mac (rc=$rc)" || bad "up -check" "macOS (vz) diagnosis" "$out"
contiene "$out" "✓ kling-vz" && contiene "$out" "✓ vz entitlement" && ok "up -check finds kling-vz, signed" \
  || bad "up -check kling-vz" "✓ kling-vz and ✓ vz entitlement" "$out"
out=$(k info 2>&1)
be=$(printf '%s' "$out" | tr -s ' ' | grep '^backend:' || true)
contiene "$be" "vz" && ok "info: $be" || bad "info backend" "backend: vz" "$out"

# ── 2. ciclo de vida ──────────────────────────────────────────────────────────
step "2. run, exec, cp, shell, logs, freeze, thaw"
M="$P-m"
t0=$(now_ms); out=$(k run -name "$M" -image "$IMG" -mem "$MEM" -allow-exec -label kling.ports=9000 2>&1); rc=$?
if [ $rc -eq 0 ] && contiene "$out" "booted cold"; then
  vmm_ms=$(printf '%s' "$out" | grep -o '[0-9]* ms' | head -1)
  # Hasta que el agente contesta: es lo que espera el usuario de verdad.
  out=$(k exec "$M" -- true 2>&1); t1=$(now_ms)
  COLD_MS=$((t1-t0))
  ok "cold boot: VMM $vmm_ms, agent answering after $COLD_MS ms"
else
  bad "run" "booted cold" "$out"; COLD_MS=-1
fi
out=$(k exec "$M" -- sh -c 'echo out; echo err >&2; exit 3' 2>"$TMPBASE/e2e-err.$$"); rc=$?
err=$(cat "$TMPBASE/e2e-err.$$"); rm -f "$TMPBASE/e2e-err.$$"
[ "$out" = "out" ] && [ "$err" = "err" ] && [ $rc -eq 3 ] && ok "exec: stdout, stderr and exit code 3" \
  || bad "exec" "out / err / rc=3" "'$out' / '$err' / rc=$rc"
lat=()
for i in 1 2 3 4 5 6 7 8 9 10; do
  t0=$(now_ms); k exec "$M" -- true >/dev/null 2>&1; t1=$(now_ms); lat+=($((t1-t0)))
done
EXEC_P50=$(pctl 50 "${lat[@]}")
ok "exec latency (CLI round trip): p50 ${EXEC_P50} ms, max $(pctl 100 "${lat[@]}") ms"

f="$TMPBASE/e2e-cp.$$"; head -c 200000 /dev/urandom >"$f"
if k cp "$f" "$M:/tmp/blob" >/dev/null 2>&1 && k cp "$M:/tmp/blob" "$f.back" >/dev/null 2>&1 && cmp -s "$f" "$f.back"; then
  ok "cp in and out (200 KB, identical)"
else
  bad "cp" "same bytes back" "$(ls -l "$f" "$f.back" 2>&1)"
fi
rm -f "$f" "$f.back"

out=$(printf 'echo tty-$((6*7))\nexit 5\n' | script -q /dev/null "$KLING" shell "$M" 2>&1); rc=$?
out=$(printf '%s' "$out" | tr -d '\r')
contiene "$out" "tty-42" && [ $rc -eq 5 ] && ok "shell over a pty (script): output and exit code 5" \
  || bad "shell" "tty-42 and rc=5" "rc=$rc $(printf '%s' "$out" | tail -3)"

out=$(k logs "$M" -tail 50 2>&1)
contiene "$out" "listening on :8080" && ok "logs: guest console (agent listening)" || bad "logs" "the agent's line" "$(printf '%s' "$out" | tail -3)"

FP_RUN=$(footprint "$M")
out=$(k freeze "$M" 2>&1)
if contiene "$out" "warm"; then FREEZE_MS=$(printf '%s' "$out" | grep -o '[0-9]* ms' | head -1); ok "freeze -> warm ($FREEZE_MS)"; else bad "freeze" "warm" "$out"; fi
out=$(k thaw "$M" 2>&1)
if contiene "$out" "running"; then THAW_MS=$(printf '%s' "$out" | grep -o '[0-9]* ms' | head -1); ok "thaw -> running ($THAW_MS)"; else bad "thaw" "running" "$out"; fi
out=$(k exec "$M" -- cat /tmp/blob 2>/dev/null | wc -c | tr -d ' ')
[ "$out" = "200000" ] && ok "exec after thaw sees the file written before" || bad "exec after thaw" "200000 bytes" "$out"
FP_THAW=$(footprint "$M")
# El reenvío de loopback acepta siempre; esperar un puerto tiene que mirar
# DENTRO del invitado (kling-vz /kling/probe). Nadie escucha en 9000: 504.
t0=$(now_ms); out=$(api -X POST "http://k/machines/$M/guest" -d '{"port":9000,"probe_only":true,"wait_ms":1500}' -w ' %{http_code}'); t1=$(now_ms)
contiene "$out" " 504" && [ $((t1-t0)) -ge 1400 ] && ok "probe_only waits for a port nobody listens on inside the guest, then 504" \
  || bad "probe_only" "504 after ~1.5 s" "$out after $((t1-t0)) ms"

# ── 3. snapshot dorado y réplicas ────────────────────────────────────────────
step "3. commit, 3 replicas, forwards, guest proxy"
SNAP="$P-gold"
k exec "$M" -- sh -c 'echo golden > /root/mark' >/dev/null 2>&1
out=$(k commit "$M" "$SNAP" 2>&1) && ok "commit -> $SNAP" || bad "commit" "snapshot" "$out"
pids=()
for i in 1 2 3; do
  ( t0=$(now_ms); o=$(k run -from "$SNAP" -name "$P-r$i" 2>&1); r=$?; t1=$(now_ms)
    echo "$r $((t1-t0)) $o" >"$TMPBASE/e2e-r$i.$$" ) &
  pids+=($!)
done
wait "${pids[@]}"
rlat=()
for i in 1 2 3; do
  read -r r ms rest <"$TMPBASE/e2e-r$i.$$"; rm -f "$TMPBASE/e2e-r$i.$$"
  [ "$r" = 0 ] && rlat+=("$ms") || bad "run -from (replica $i)" "instantiated" "$rest"
done
[ ${#rlat[@]} -eq 3 ] && ok "3 concurrent replicas from $SNAP: $(printf '%s ' "${rlat[@]}")ms"
RUNFROM_P50=$(pctl 50 "${rlat[@]:-0}")
addrs=""
for i in 1 2 3; do
  a=$(k ps -json | pyj "next((m.get('forwards',{}).get('8080','') for m in d if m['name']=='$P-r$i'), '')")
  addrs="$addrs $a"
  h=$(curl -s -m 5 "http://$a/healthz" 2>&1)
  [ -n "$a" ] && [ "$h" = "ok" ] && ok "replica $i reachable through its own forward $a" || bad "forward r$i" "ok from http://$a/healthz" "$h"
done
nuniq=$(printf '%s\n' $addrs | sort -u | wc -l | tr -d ' ')
[ "$nuniq" = 3 ] && ok "each replica has a distinct loopback port" || bad "forwards" "3 distinct" "$addrs"
out=$(k exec "$P-r2" -- cat /root/mark 2>&1)
[ "$out" = "golden" ] && ok "replica carries the golden state" || bad "replica state" "golden" "$out"
FP_RESTORED=$(footprint "$P-r1")

# El proxy del daemon: al agente (kling-guest) y, si hay imagen con puente, a MCP.
out=$(api -X POST "http://k/machines/$P-r1/guest" -d '{"port":8080,"path":"/healthz","method":"GET"}')
st=$(printf '%s' "$out" | pyj "d and d.get('status')")
[ "$st" = 200 ] && ok "guest proxy to the agent on 8080 (200)" || bad "guest proxy" "status 200" "$out"
out=$(api -X POST "http://k/machines/$P-r1/guest" -d '{"port":9999,"path":"/","method":"GET"}' -o /dev/null -w '%{http_code}')
[ "$out" = 403 ] && ok "guest proxy refuses an undeclared port (403)" || bad "guest proxy port" "403" "$out"
if [ $HAVE_BRIDGE = 1 ]; then
  B="$P-bridge"
  if k run -name "$B" -image "$BRIDGE_IMG" -mem 512 >/dev/null 2>&1; then
    init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"e2e","version":"1"}}}'
    req=$(python3 -c 'import json,sys; print(json.dumps({"port":8080,"path":"/mcp","method":"POST","body":sys.argv[1],"headers":{"Accept":"application/json, text/event-stream","Content-Type":"application/json"},"response_headers":["Mcp-Session-Id"],"wait_ms":30000}))' "$init")
    out=$(api -X POST "http://k/machines/$B/guest" -d "$req")
    sid=$(printf '%s' "$out" | pyj "d and d.get('headers',{}).get('Mcp-Session-Id','')")
    contiene "$out" "serverInfo" && [ -n "$sid" ] && ok "guest proxy to the MCP bridge of $BRIDGE_IMG: initialize, session $sid" \
      || bad "guest proxy MCP" "initialize with a session" "$out"
    k rm "$B" >/dev/null 2>&1
  else
    bad "run $BRIDGE_IMG" "running" "failed"
  fi
else
  info "skipped the MCP bridge check: no image $BRIDGE_IMG"
fi

# ── 4. sandbox ────────────────────────────────────────────────────────────────
step "4. sandbox"
t0=$(now_ms); sb=$(k sandbox create -image "$IMG" -mem "$MEM" -name "$P-sb" -q 2>&1); rc=$?; t1=$(now_ms)
if [ $rc -eq 0 ]; then
  ok "sandbox create ($((t1-t0)) ms, agent answering)"
  out=$(k exec "$P-sb" -- uname -m 2>&1); [ "$out" = aarch64 ] && ok "sandbox exec (uname -m = aarch64)" || bad "sandbox exec" "aarch64" "$out"
  out=$(k sandbox renew "$P-sb" -ttl 20m 2>&1) && ok "sandbox renew" || bad "sandbox renew" "renewed" "$out"
  out=$(k sandbox rm "$P-sb" 2>&1); ls=$(k ps -a 2>&1)
  contiene "$ls" "$P-sb" && bad "sandbox rm" "gone" "still listed" || ok "sandbox rm"
else
  bad "sandbox create" "created" "$sb"
fi

# ── 5. squeeze y resize ──────────────────────────────────────────────────────
step "5. squeeze, resize"
# En macOS el globo solo devuelve memoria en una máquina restaurada: se aprieta
# una réplica.
before=$(footprint "$P-r1")
out=$(k squeeze "$P-r1" 2>&1)
sleep 4   # al desinflar, el framework repoblaba las páginas en ~3 s
FP_SQUEEZED=$(footprint "$P-r1")
[ "$FP_SQUEEZED" -gt 0 ] && [ "$FP_SQUEEZED" -lt $((before * 3 / 4)) ] \
  && ok "squeeze a replica: footprint $before -> $FP_SQUEEZED MiB, still there 4 s later" \
  || bad "squeeze" "footprint well below $before MiB and holding" "$FP_SQUEEZED MiB ($out)"
out=$(k exec "$P-r1" -- echo alive 2>&1); [ "$out" = alive ] && ok "squeezed replica still answers" || bad "after squeeze" "alive" "$out"

R="$P-rs"
if k run -name "$R" -image "$IMG" -mem 256 -mem-max 512 -allow-exec >/dev/null 2>&1; then
  sleep 3   # el globo inicial se fija cuando el driver del invitado aparece
  tot() { k exec "$R" -- sh -c 'free -m | awk "/^Mem:/ {print \$2-\$3+\$6}"' 2>/dev/null; } # total - usado + caché ≈ lo que le queda
  avail0=$(tot)
  out=$(k resize "$R" -mem 448 2>&1); sleep 2; avail1=$(tot)
  [ -n "$avail0" ] && [ -n "$avail1" ] && [ "$avail0" -lt 300 ] && [ "$avail1" -gt $((avail0 + 120)) ] \
    && ok "mem-max ceiling applied at boot (~$avail0 MiB usable of 512); resize 256 -> 448: ~$avail1 MiB" \
    || bad "resize" "~256 usable at boot, ~448 after" "$avail0 -> $avail1 ($out)"
  k rm "$R" >/dev/null 2>&1
else
  bad "run -mem-max" "running" "failed"
fi

# ── 6. egress y MMDS ─────────────────────────────────────────────────────────
step "6. egress, MMDS"
probe='wget -q -T 5 -O - http://example.com/ >/dev/null 2>&1 && echo NAME-OK || echo NAME-NO; wget -q -T 5 -O - http://1.1.1.1/ >/dev/null 2>&1 && echo IP-OK || echo IP-NO; wget -q -T 3 -O - http://192.168.1.1/ >/dev/null 2>&1 && echo LAN-OK || echo LAN-NO'
for e in none internet; do
  if k run -name "$P-eg-$e" -image "$IMG" -mem "$MEM" -allow-exec -egress "$e" >/dev/null 2>&1; then
    out=$(k exec -timeout 30s "$P-eg-$e" -- sh -c "$probe" 2>&1 | tr '\n' ' ')
    case "$e" in
      none) contiene "$out" "NAME-NO" && contiene "$out" "IP-NO" && contiene "$out" "LAN-NO" \
              && ok "egress none: no DNS, no IP, no LAN ($out)" || bad "egress none" "all blocked" "$out";;
      internet) contiene "$out" "NAME-OK" && contiene "$out" "LAN-NO" \
              && ok "egress internet: example.com reachable, LAN blocked ($out)" || bad "egress internet" "NAME-OK, LAN-NO" "$out";;
    esac
  else
    bad "run -egress $e" "running" "failed"
  fi
done
mm="$P-eg-none"
out=$(printf '{"secret":"s3cr3t-%s"}' "$$" | k mmds "$mm" 2>&1)
py='import urllib.request as u
t=u.urlopen(u.Request("http://169.254.169.254/latest/api/token",method="PUT",headers={"X-metadata-token-ttl-seconds":"60"}),timeout=5).read().decode()
print(u.urlopen(u.Request("http://169.254.169.254/",headers={"X-metadata-token":t,"Accept":"application/json"}),timeout=5).read().decode())'
out=$(k exec "$mm" -- python3 -c "$py" 2>&1)
contiene "$out" "s3cr3t-$$" && ok "MMDS v2 from inside, with egress none" || bad "mmds" "the secret" "$out"
out=$(k freeze "$mm" 2>&1); contiene "$out" "cannot be frozen" && ok "a machine with MMDS secrets refuses to freeze" || bad "freeze with mmds" "refused" "$out"
k rm "$P-eg-internet" >/dev/null 2>&1

# ── 6b. carpetas compartidas ─────────────────────────────────────────────────
# La copia (un ext4 construido con el mke2fs que haya) y la viva en escritura,
# servida desde APFS por el daemon sin privilegios. Ver docs/compartir.md.
step "6b. shared folders (copy, rw)"
mkdir -p "$SHARES/rw" "$SHARES/src/sub"
echo "copied" > "$SHARES/src/a.txt"; ln -s a.txt "$SHARES/src/link"; ln -s /etc/passwd "$SHARES/src/abs"
echo "host secret" > "$SHARES/secret"; ln -s ../secret "$SHARES/rw/up"; ln -s "$SHARES/secret" "$SHARES/rw/abs"
echo one > "$SHARES/rw/host.txt"
S="$P-sh"
out=$(k run -name "$S" -image "$IMG" -mem "$MEM" -allow-exec -share "$SHARES/src:/work" -share "$SHARES/rw:/w:rw" 2>&1); rc=$?
if [ $rc -eq 0 ] && contiene "$out" "booted cold"; then
  contiene "$out" "skipped abs" && ok "copy: the absolute symlink is skipped, and said" || bad "copy skip notice" "skipped abs" "$out"
  out=$(k exec "$S" -- sh -c 'cat /work/a.txt /work/link; touch /work/x 2>&1; true' 2>&1)
  contiene "$out" "copied
copied" && contiene "$out" "Read-only" && ok "copy: contents inside, read-only" || bad "copy" "copied x2 + Read-only" "$out"
  out=$(k exec "$S" -- sh -c 'cat /w/host.txt; cd /w && echo g > g && mkdir -p d/e && mv g d/e/g2 && echo more >> d/e/g2 && ln -s x y 2>&1; cat /w/up /w/abs 2>&1; true' 2>&1)
  host=$(cat "$SHARES/rw/d/e/g2" 2>&1)
  contiene "$out" "one" && [ "$host" = "g
more" ] && contiene "$out" "not permitted" && ! contiene "$out" "host secret" \
    && ok "rw: both ways, rename and mkdir reach APFS; no symlinks; host secret unreadable" \
    || bad "rw" "one / g+more on the host / not permitted / no secret" "$out // host: $host"
  echo two > "$SHARES/rw/host.txt"; sleep 1.5
  out=$(k exec "$S" -- cat /w/host.txt 2>&1)
  [ "$out" = "two" ] && ok "rw: a host edit shows up inside" || bad "host edit" "two" "$out"
  out=$(k exec -timeout 5m "$S" -- sh -c 'dd if=/dev/zero of=/w/big bs=1M count=200 conv=fsync 2>&1 | tail -1; echo 3 > /proc/sys/vm/drop_caches; dd if=/w/big of=/dev/null bs=1M 2>&1 | tail -1; rm /w/big' 2>&1)
  SH_W=$(echo "$out" | sed -n 1p | grep -o '[0-9.]*[MG]B/s' || true); SH_R=$(echo "$out" | sed -n 2p | grep -o '[0-9.]*[MG]B/s' || true)
  [ -n "$SH_W" ] && [ -n "$SH_R" ] && ok "rw: sequential write $SH_W, read $SH_R" || bad "throughput" "two dd results" "$out"
  SH_SMALL=$(k exec -timeout 5m "$S" -- python3 -c '
import os, time
d="/w/small"; os.makedirs(d); N=1000
t=time.time()
for i in range(N): open(f"{d}/f{i}","w").write("x"*1024)
c=time.time()-t; t=time.time()
for i in range(N): os.stat(f"{d}/f{i}")
s=time.time()-t; t=time.time()
for i in range(N): os.unlink(f"{d}/f{i}")
u=time.time()-t
print(f"create {N/c:.0f}/s stat {N/s:.0f}/s unlink {N/u:.0f}/s")' 2>&1)
  contiene "$SH_SMALL" "create" && ok "rw: small files: $SH_SMALL" || bad "small files" "ops/s" "$SH_SMALL"
  # Congelar y descongelar con un proceso escribiendo por un fichero abierto.
  k exec "$S" -- sh -c 'setsid python3 -c "
import time
f=open(\"/w/log\",\"a\",buffering=1)
while True:
    f.write(str(time.time())+chr(10)); time.sleep(0.1)
" >/tmp/writer.err 2>&1 </dev/null &' >/dev/null 2>&1
  sleep 2
  k freeze "$S" >/dev/null 2>&1 && k thaw "$S" >/dev/null 2>&1
  n1=$(wc -l < "$SHARES/rw/log" | tr -d ' '); sleep 2; n2=$(wc -l < "$SHARES/rw/log" | tr -d ' ')
  out=$(k exec "$S" -- cat /tmp/writer.err 2>&1)
  [ "${n2:-0}" -gt "${n1:-0}" ] && [ -z "$out" ] \
    && ok "rw: freeze -> thaw keeps the mount and the open file ($n1 -> $n2 lines)" \
    || bad "freeze/thaw with a live share" "the log grows, no errors" "$n1 -> $n2 $out"
  out=$(k commit "$S" "$P-shsnap" 2>&1)
  contiene "$out" "cannot be committed" && ok "commit with shares refused" || bad "commit with shares" "refused" "$out"
else
  bad "run -share" "booted cold" "$out"
fi
k rm "$S" >/dev/null 2>&1

# ── 7. reinicio del daemon y kill -9 ─────────────────────────────────────────
step "7. daemon restart, SIGKILL of a kling-vz"
pid_before=$(machine_field "$P-r3" pid)
stop_daemon
alive=$(our_vmms)
[ "$alive" -gt 0 ] && ok "machines outlive the daemon ($alive kling-vz running)" || bad "daemon stop" "kling-vz still running" "$alive"
start_daemon && ok "daemon restarted" || bad "daemon restart" "up" "see $LOG"
st=$(machine_field "$P-r3" state); pid_after=$(machine_field "$P-r3" pid)
out=$(k exec "$P-r3" -- cat /root/mark 2>&1)
[ "$st" = running ] && [ "$pid_before" = "$pid_after" ] && [ "$out" = golden ] \
  && ok "re-adopted after the restart (pid $pid_after) and reachable" || bad "re-adopt" "running, same pid, golden" "$st $pid_before->$pid_after '$out'"
a=$(k ps -json | pyj "next((m.get('forwards',{}).get('8080','') for m in d if m['name']=='$P-r3'), '')")
h=$(curl -s -m 5 "http://$a/healthz" 2>&1); [ "$h" = ok ] && ok "its forward $a still works" || bad "forward after restart" ok "$h"

vm_before=$(apple_vms)
victim=$(machine_field "$P-r2" pid)
kill -9 "$victim" 2>/dev/null
st=""
for i in $(seq 1 30); do st=$(machine_field "$P-r2" state); [ "$st" = failed ] && break; sleep 1; done
[ "$st" = failed ] && ok "kill -9 of its kling-vz: marked failed after ~${i} s" || bad "kill -9" "failed" "$st"
sleep 1
vm_after=$(apple_vms)
[ "$vm_after" -eq $((vm_before - 1)) ] && ok "its Apple VM process went with it ($vm_before -> $vm_after)" \
  || bad "Apple VM helper" "$((vm_before - 1))" "$vm_after"

# ── 8. ráfaga ────────────────────────────────────────────────────────────────
step "8. stress: burst of $BURST runs from the snapshot"
# Se deja sitio: lo de antes se borra y se mide lo libre.
for m in $(k ps -a -q 2>/dev/null); do k rm "$m" >/dev/null 2>&1; done
sleep 2
avail_before=$(api http://k/procstats | pyj "d['available_mib']")
t0=$(now_ms); pids=()
for i in $(seq 1 "$BURST"); do
  ( a=$(now_ms); o=$(k run -from "$SNAP" -name "$P-b$i" 2>&1); r=$?; b=$(now_ms)
    echo "$r $((b-a)) $o" >"$TMPBASE/e2e-b$i.$$" ) &
  pids+=($!)
done
peak=0
while vivos "${pids[@]}"; do tf=$(total_fp); [ "${tf:-0}" -gt $peak ] && peak=$tf; sleep 0.3; done
wait "${pids[@]}"; t1=$(now_ms)
blat=(); bfail=0
for i in $(seq 1 "$BURST"); do
  read -r r ms rest <"$TMPBASE/e2e-b$i.$$"; rm -f "$TMPBASE/e2e-b$i.$$"
  if [ "$r" = 0 ]; then blat+=("$ms"); else bfail=$((bfail+1)); info "b$i: $rest"; fi
done
tf=$(total_fp); [ "${tf:-0}" -gt $peak ] && peak=$tf
[ $bfail -eq 0 ] && ok "$BURST/$BURST started through the launch gate (KLING_MAX_PARALLEL_BOOT=$KLING_MAX_PARALLEL_BOOT) in $((t1-t0)) ms" \
  || bad "burst" "$BURST started" "$bfail failed"
BURST_P50=$(pctl 50 "${blat[@]:-0}"); BURST_P95=$(pctl 95 "${blat[@]:-0}")
ok "run -from latency: p50 $BURST_P50 ms, p95 $BURST_P95 ms"
nok=0
for i in $(seq 1 "$BURST"); do out=$(k exec "$P-b$i" -- cat /root/mark 2>&1); [ "$out" = golden ] && nok=$((nok+1)); done
[ $nok -eq "$BURST" ] && ok "all $BURST answer exec" || bad "burst exec" "$BURST" "$nok"

pids=()
for i in $(seq 1 "$BURST"); do k freeze "$P-b$i" >/dev/null 2>&1 & pids+=($!); done
wait "${pids[@]}"
tl=(); pids=()
for i in $(seq 1 "$BURST"); do
  ( a=$(now_ms); o=$(k thaw "$P-b$i" 2>&1); r=$?; b=$(now_ms); echo "$r $((b-a)) $o" >"$TMPBASE/e2e-t$i.$$" ) &
  pids+=($!)
done
while vivos "${pids[@]}"; do tf=$(total_fp); [ "${tf:-0}" -gt $peak ] && peak=$tf; sleep 0.3; done
wait "${pids[@]}"; tfail=0
for i in $(seq 1 "$BURST"); do
  read -r r ms rest <"$TMPBASE/e2e-t$i.$$"; rm -f "$TMPBASE/e2e-t$i.$$"
  [ "$r" = 0 ] && tl+=("$ms") || { tfail=$((tfail+1)); info "t$i: $rest"; }
done
[ $tfail -eq 0 ] && ok "$BURST concurrent thaws: p50 $(pctl 50 "${tl[@]:-0}") ms, p95 $(pctl 95 "${tl[@]:-0}") ms" \
  || bad "thaws" "$BURST thawed" "$tfail failed"
ok "peak footprint of the burst: $peak MiB for $BURST microVMs (a restore materializes all of its memory)"
pids=()
for m in $(k ps -a -q 2>/dev/null); do k rm "$m" >/dev/null 2>&1 & pids+=($!); done
[ ${#pids[@]} -gt 0 ] && wait "${pids[@]}"
sleep 3
tf=$(total_fp); avail_after=$(api http://k/procstats | pyj "d['available_mib']")
[ "${tf:-1}" = 0 ] && ok "memory returned: footprint 0 MiB, host available $avail_before -> $avail_after MiB" \
  || bad "memory after cleanup" "0 MiB of microVMs" "$tf"

# ── 9. nada suelto ───────────────────────────────────────────────────────────
step "9. leftovers"
k rmi "$SNAP" >/dev/null 2>&1
stop_daemon
sleep 1
n=$(our_vmms); [ "$n" = 0 ] && ok "no kling-vz of this test left" || bad "kling-vz leftovers" 0 "$n"
n=$(apple_vms); [ "$n" -le "$APPLE_BEFORE" ] && ok "no Apple VM process left ($n, $APPLE_BEFORE before the test)" || bad "Apple VM leftovers" "$APPLE_BEFORE" "$n"
n=$(our_links); [ "$n" = 0 ] && ok "no /tmp/kling-$(id -u) link into the test root left" || bad "short links" 0 "$n"

step "numbers"
info "cold boot to agent:     ${COLD_MS} ms"
info "freeze / thaw:          ${FREEZE_MS:-?} / ${THAW_MS:-?}"
info "run -from (3 at once):  p50 ${RUNFROM_P50} ms"
info "exec round trip:        p50 ${EXEC_P50} ms"
info "footprint running:      ${FP_RUN} MiB (mem ${MEM})"
info "footprint thawed:       ${FP_THAW} MiB, replica ${FP_RESTORED} MiB, squeezed ${FP_SQUEEZED} MiB"
info "burst of $BURST:           p50 ${BURST_P50} ms, p95 ${BURST_P95} ms, peak ${peak} MiB"
info "live share (rw):        write ${SH_W:-?}, read ${SH_R:-?}; ${SH_SMALL:-?}"

printf "\n\033[1m%d ok · %d failed\033[0m\n" "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
