#!/bin/bash
# Prueba de humo de kling-vz en un Mac real, sin el daemon: habla con el
# ayudante por su socket con curl y repite las secuencias que usa el núcleo.
#
#   GUEST_DIR=/ruta/con/vmlinux,min.ext4,overlay-64.ext4,*.layer.ext4 scripts/smoke.sh
#
# Variables: KLING_VZ (binario, por defecto ./bin/kling-vz), LAYER (capa de
# servicio, por defecto everything.layer.ext4), WORK (directorio de trabajo,
# por defecto uno nuevo bajo $TMPDIR). Nunca escribe en GUEST_DIR: todo lo
# escribible se clona con `cp -c` (clonefile de APFS) al directorio de trabajo.
#
# Recorre: arranque en frío -> puente alcanzable por el puerto reenviado ->
# MMDS desde el invitado -> egress none -> pausa + snapshot con el
# overlay reapuntado (como Commit) -> SIGTERM -> ayudante nuevo, load con
# resume_vm=false + PATCH del overlay + Resumed -> alcanzable otra vez ->
# globo -> egress internet -> tercer ayudante con resume_vm=true y egress allowlist.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
KLING_VZ=${KLING_VZ:-$ROOT/bin/kling-vz}
: "${GUEST_DIR:?set GUEST_DIR to the directory with vmlinux, min.ext4, overlay-64.ext4 and the layer images}"
LAYER=${LAYER:-$GUEST_DIR/everything.layer.ext4}
WORK=${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/kling-vz-smoke.XXXXXX")}
MEM=${MEM:-256}
GUEST_PORT=8080

for f in "$KLING_VZ" "$GUEST_DIR/vmlinux" "$GUEST_DIR/min.ext4" "$GUEST_DIR/overlay-64.ext4" "$LAYER"; do
	[ -e "$f" ] || { echo "missing: $f" >&2; exit 1; }
done
mkdir -p "$WORK"
cd "$WORK"
echo "work dir: $WORK"

now_ms() { perl -MTime::HiRes=time -e 'printf "%d\n", time*1000'; }
fail() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "  ok  $*"; }

PIDS=()
cleanup() {
	for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -9 "$p" 2>/dev/null || true; done
}
trap cleanup EXIT

# api SOCK METHOD PATH [BODY] -> imprime el cuerpo; falla si el código no es 2xx.
# El socket es relativo al directorio de trabajo: sun_path solo admite 104
# bytes y $TMPDIR en macOS ya se come casi todos.
api() {
	local sock=$1 method=$2 path=$3 body=${4:-}
	local out code
	out=$(curl -sS --unix-socket "$sock" -X "$method" "http://localhost$path" \
		-H 'Content-Type: application/json' ${body:+-d "$body"} -w '\n%{http_code}')
	code=${out##*$'\n'}
	out=${out%$'\n'*}
	[[ $code == 2* ]] || fail "$method $path -> $code $out"
	printf '%s' "$out"
}

start_helper() { # nombre -> deja PID en $HPID
	local name=$1
	rm -f "$name.sock"
	"$KLING_VZ" --api-sock "$name.sock" >"$name.log" 2>&1 &
	HPID=$!
	PIDS+=("$HPID")
	for _ in $(seq 1 100); do
		curl -s --unix-socket "$name.sock" http://localhost/ >/dev/null 2>&1 && return 0
		sleep 0.02
	done
	fail "$name: API socket did not come up"
}

forward() { # sock -> dirección 127.0.0.1:N del puerto del invitado
	api "$1" PUT /kling/forwards "{\"ports\":[$GUEST_PORT]}" |
		perl -ne "print \$1 if /\"$GUEST_PORT\":\"([^\"]+)\"/"
}

wait_healthz() { # addr -> ms hasta que /healthz contesta
	local t0 t1
	t0=$(now_ms)
	for _ in $(seq 1 1500); do
		if curl -s -m 1 "http://$1/healthz" >/dev/null 2>&1; then
			t1=$(now_ms)
			echo $((t1 - t0))
			return 0
		fi
		sleep 0.02
	done
	return 1
}

# gexec addr cmd... -> salida del comando en el invitado (via /exec del agente)
gexec() {
	local addr=$1
	shift
	local json
	json=$(perl -e 'print "[", join(",", map { s/(["\\])/\\$1/g; s/\n/\\n/g; s/\t/\\t/g; "\"$_\"" } @ARGV), "]"' "$@")
	curl -s -m 60 "http://$addr/exec" -d "{\"cmd\":$json}"
}

footprint() { api "$1" GET /kling/stats | perl -ne 'print $1 if /"footprint_mib":(\d+)/'; }

BOOT_ARGS="console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/sbin/overlay-init ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off kling.exec=1 kling.layer=/dev/vdc"

echo "== 1. cold boot (helper A)"
cp -c "$GUEST_DIR/overlay-64.ext4" overlay-a.ext4
start_helper a
A=a.sock
api $A GET /kling/info >/dev/null
api $A PUT /boot-source "{\"kernel_image_path\":\"$GUEST_DIR/vmlinux\",\"boot_args\":\"$BOOT_ARGS\"}"
api $A PUT /drives/rootfs "{\"drive_id\":\"rootfs\",\"path_on_host\":\"$GUEST_DIR/min.ext4\",\"is_root_device\":true,\"is_read_only\":true}"
api $A PUT /drives/overlay "{\"drive_id\":\"overlay\",\"path_on_host\":\"$WORK/overlay-a.ext4\",\"is_root_device\":false,\"is_read_only\":false}"
api $A PUT /drives/layer "{\"drive_id\":\"layer\",\"path_on_host\":\"$LAYER\",\"is_root_device\":false,\"is_read_only\":true}"
api $A PUT /network-interfaces/eth0 '{"iface_id":"eth0","host_dev_name":"tap0","guest_mac":"06:00:AC:10:00:02"}'
api $A PUT /mmds/config '{"version":"V2","ipv4_address":"169.254.169.254","network_interfaces":["eth0"]}'
api $A PUT /entropy '{}'
api $A PUT /machine-config "{\"vcpu_count\":1,\"mem_size_mib\":$MEM}"
api $A PUT /balloon '{"amount_mib":0,"deflate_on_oom":true,"stats_polling_interval_s":1}'
api $A PUT /kling/network '{"egress":"none"}'
T0=$(now_ms)
api $A PUT /actions '{"action_type":"InstanceStart"}'
T_START=$(($(now_ms) - T0))
ADDR_A=$(forward $A)
[ -n "$ADDR_A" ] || fail "no forward for $GUEST_PORT"
[ "$(forward $A)" = "$ADDR_A" ] || fail "repeating a forward returned a different address"
W=$(wait_healthz "$ADDR_A") || fail "guest agent not reachable through $ADDR_A"
BOOT_MS=$((T_START + W))
ok "InstanceStart ${T_START} ms, guest agent on $ADDR_A after ${BOOT_MS} ms"
sleep 2
FP_BOOT=$(footprint $A)
ok "footprint ${FP_BOOT} MiB"

echo "== 2. MMDS from the guest"
api $A PUT /mmds '{"env":{"SMOKE":"mmds-ok"},"sessions":{}}'
OUT=$(gexec "$ADDR_A" node -e '
fetch("http://169.254.169.254/latest/api/token",{method:"PUT",headers:{"X-metadata-token-ttl-seconds":"60"}})
 .then(r=>r.text()).then(t=>fetch("http://169.254.169.254/",{headers:{"X-metadata-token":t,Accept:"application/json"}}))
 .then(r=>r.text()).then(console.log).catch(e=>console.log("ERR "+e.message))')
[[ $OUT == *mmds-ok* ]] || fail "MMDS not readable from the guest: $OUT"
OUT=$(gexec "$ADDR_A" wget -q -T 3 -O - http://169.254.169.254/)
[[ $OUT == *mmds-ok* ]] && fail "MMDS answered without a token: $OUT"
ok "token flow works and reads without a token are rejected"

echo "== 3. egress none"
OUT=$(gexec "$ADDR_A" sh -c 'wget -q -T 5 -O - http://1.1.1.1/ >/dev/null 2>&1 && echo LEAK; nslookup example.com 2>&1 | grep -q REFUSED && echo DNS-REFUSED; true')
[[ $OUT == *LEAK* ]] && fail "egress none let the guest reach 1.1.1.1"
[[ $OUT == *DNS-REFUSED* ]] || fail "egress none: DNS was not refused: $OUT"
ok "outbound TCP blocked, DNS refused"

echo "== 4. pause + snapshot with the overlay repointed (Commit)"
T0=$(now_ms)
api $A PATCH /vm '{"state":"Paused"}'
T_PAUSE=$(($(now_ms) - T0))
cp -c overlay-a.ext4 overlay-gold.ext4
api $A PATCH /drives/overlay "{\"drive_id\":\"overlay\",\"path_on_host\":\"$WORK/overlay-gold.ext4\"}"
T0=$(now_ms)
api $A PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$WORK/snap.json\",\"mem_file_path\":\"$WORK/state.vzs\"}"
SNAP_MS=$(($(now_ms) - T0))
api $A PATCH /drives/overlay "{\"drive_id\":\"overlay\",\"path_on_host\":\"$WORK/overlay-a.ext4\"}"
api $A PATCH /vm '{"state":"Resumed"}'
grep -q overlay-gold.ext4 snap.json || fail "snapshot does not record the patched overlay"
grep -q machine_identifier snap.json || fail "snapshot has no machine identifier"
curl -s -m 3 "http://$ADDR_A/healthz" >/dev/null || fail "template unreachable after resuming"
STATE_MIB=$(($(stat -f %z state.vzs) / 1048576))
ok "pause ${T_PAUSE} ms, snapshot ${SNAP_MS} ms, state file ${STATE_MIB} MiB"

echo "== 5. SIGTERM helper A"
kill -TERM "$HPID"
for _ in $(seq 1 100); do kill -0 "$HPID" 2>/dev/null || break; sleep 0.05; done
kill -0 "$HPID" 2>/dev/null && fail "helper A still alive after SIGTERM"
wait "$HPID" 2>/dev/null && ok "helper A exited cleanly" || fail "helper A exit code $?"
[ -e a.sock ] && fail "helper A left its socket behind"

echo "== 6. restore in a new helper: load(resume=false) + PATCH overlay + Resumed"
cp -c overlay-gold.ext4 overlay-b.ext4
start_helper b
B=b.sock
api $B PUT /kling/network '{"egress":"internet"}'
T0=$(now_ms)
api $B PUT /snapshot/load "{\"snapshot_path\":\"$WORK/snap.json\",\"mem_backend\":{\"backend_path\":\"$WORK/state.vzs\",\"backend_type\":\"File\"},\"resume_vm\":false}"
T_LOAD=$(($(now_ms) - T0))
api $B PATCH /drives/overlay "{\"drive_id\":\"overlay\",\"path_on_host\":\"$WORK/overlay-b.ext4\"}"
T0=$(now_ms)
api $B PATCH /vm '{"state":"Resumed"}'
T_RESUME=$(($(now_ms) - T0))
ADDR_B=$(forward $B)
W=$(wait_healthz "$ADDR_B") || fail "restored guest not reachable through $ADDR_B"
RESTORE_MS=$((T_LOAD + T_RESUME))
ok "load ${T_LOAD} ms + restore/resume ${T_RESUME} ms; agent answered ${W} ms later"
sleep 2
FP_RESTORE=$(footprint $B)
ok "footprint after restore ${FP_RESTORE} MiB"

echo "== 7. balloon on the restored machine"
# Tras restaurar, el framework ha comprometido toda la RAM del invitado: es
# donde el globo devuelve memoria al host (en un arranque en frío las páginas
# libres nunca se tocaron y no hay nada que devolver).
api $B PATCH /balloon '{"amount_mib":128}'
STATS=$(api $B GET /balloon/statistics)
[[ $STATS == *'"target_mib":128'* ]] || fail "balloon stats: $STATS"
sleep 3
FP_BALLOON=$(footprint $B)
api $B PATCH /balloon '{"amount_mib":0}'
curl -s -m 3 "http://$ADDR_B/healthz" >/dev/null || fail "guest unreachable after the balloon"
ok "inflated 128 of ${MEM} MiB: footprint ${FP_RESTORE} -> ${FP_BALLOON} MiB; deflated, guest still answers"

echo "== 8. egress internet"
OUT=$(gexec "$ADDR_B" sh -c 'wget -q -T 10 -O - http://example.com/ 2>&1 | head -c 200; echo; wget -q -T 3 -O - http://192.168.1.1/ >/dev/null 2>&1 && echo PRIVATE-LEAK; true')
[[ $OUT == *"Example Domain"* ]] || fail "egress internet: example.com not reachable: $OUT"
[[ $OUT == *PRIVATE-LEAK* ]] && fail "egress internet reached a private address"
ok "example.com reachable, private ranges blocked"
kill -TERM "$HPID"
wait "$HPID" 2>/dev/null || fail "helper B exit code $?"

echo "== 9. thaw-style load(resume=true) in a third helper, egress allowlist"
start_helper c
C=c.sock
api $C PUT /kling/network '{"egress":"allowlist","allow_domains":["example.com"]}'
T0=$(now_ms)
api $C PUT /snapshot/load "{\"snapshot_path\":\"$WORK/snap.json\",\"mem_backend\":{\"backend_path\":\"$WORK/state.vzs\",\"backend_type\":\"File\"},\"resume_vm\":true}"
THAW_MS=$(($(now_ms) - T0))
ADDR_C=$(forward $C)
W=$(wait_healthz "$ADDR_C") || fail "thawed guest not reachable"
OUT=$(gexec "$ADDR_C" sh -c 'wget -q -T 10 -O - http://example.com/ 2>&1 | head -c 200; echo; nslookup google.com 2>&1 | grep -q REFUSED && echo OTHER-REFUSED; wget -q -T 5 -O - http://1.1.1.1/ >/dev/null 2>&1 && echo IP-LEAK; true')
[[ $OUT == *"Example Domain"* ]] || fail "allowlist: example.com not reachable: $OUT"
[[ $OUT == *OTHER-REFUSED* ]] || fail "allowlist: other domains resolved: $OUT"
[[ $OUT == *IP-LEAK* ]] && fail "allowlist: direct IP reachable"
ok "load(resume=true) ${THAW_MS} ms, agent ${W} ms later; allowlist lets example.com through only"
kill -TERM "$HPID"
wait "$HPID" 2>/dev/null || fail "helper C exit code $?"

cat <<EOF

kling-vz smoke: PASS
  cold boot to guest agent   ${BOOT_MS} ms   (InstanceStart ${T_START} ms)
  pause                      ${T_PAUSE} ms
  snapshot/create            ${SNAP_MS} ms   (state file ${STATE_MIB} MiB)
  restore (load+resume)      ${RESTORE_MS} ms
  footprint running          ${FP_BOOT} MiB
  footprint restored         ${FP_RESTORE} MiB
  footprint restored+balloon ${FP_BALLOON} MiB   (128 of ${MEM} MiB inflated)
EOF
