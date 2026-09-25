#!/usr/bin/env bash
# Frozen -> first response, phase by phase (docs/despertar.md).
#
# Runs ON the Linux host, next to the daemon (it talks to its Unix socket and
# dials the guests by IP). Two measurements:
#
#   daemon   freeze/thaw one machine through the daemon API and time, from the
#            client, the thaw call and the first HTTP request to the guest;
#            prints the daemon's own phase breakdown (api.Machine.Wake).
#   gateway  a `kling ai serve` in front of a Chispa task with backend microvm:
#            wait for its reaper to freeze the replica (-idle), time one
#            /v1/classify from the client, and read the per-phase histograms
#            (kling_ai_wake_phase_seconds) from the gateway's /metrics.
#   paused   like gateway, with -paused-mib so the reaper pauses instead of
#            freezing (the paused tier); prints the replica's RSS while paused.
#
#   sudo SNAP=lat-chispa ./99-thaw-bench.sh daemon
#   sudo SNAP=lat-chispa N=20 ./99-thaw-bench.sh gateway
#
# Env: SNAP (golden snapshot of a Chispa task, required), N (iterations, 10),
# KLING (kling binary), SOCK (daemon socket, /run/kling.sock), PORT (guest
# port, 8000), TEXT (text to classify), IDLE (gateway -idle, 3s), KEEP=1 (do
# not remove the lat- machines at the end).
set -uo pipefail

MODE="${1:-daemon}"
SNAP="${SNAP:?set SNAP to the golden snapshot of a Chispa task (kling chispa deploy)}"
N="${N:-10}"
KLING="${KLING:-kling}"
SOCK="${SOCK:-/run/kling.sock}"
PORT="${PORT:-8000}"
TEXT="${TEXT:-turn on the kitchen lights}"
IDLE="${IDLE:-3s}"
KEEP="${KEEP:-0}"
WORK="$(mktemp -d /tmp/lat-bench.XXXXXX)"
GWPID=""

cleanup() {
  [ -n "$GWPID" ] && kill "$GWPID" 2>/dev/null && wait "$GWPID" 2>/dev/null
  if [ "$KEEP" != "1" ]; then
    # Each line is "<id> <name>"; only our own lat- machines are removed.
    for m in $("$KLING" ps -a 2>/dev/null | awk '$2 ~ /^lat-(bench|gw)/ {print $1}'); do
      "$KLING" rm "$m" >/dev/null 2>&1
    done
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

# The whole measurement is Python: one process, monotonic clock, no fork per
# sample. Output is plain text tables.
bench_py() {
  python3 - "$@" <<'PY'
import http.client, json, os, socket, statistics, sys, time

mode, sock, snap, n, port, text = sys.argv[1], sys.argv[2], sys.argv[3], int(sys.argv[4]), int(sys.argv[5]), sys.argv[6]
gwsock = sys.argv[7] if len(sys.argv) > 7 else ""

class UnixConn(http.client.HTTPConnection):
    def __init__(self, path, timeout=120):
        super().__init__("localhost", timeout=timeout)
        self.path_ = path
    def connect(self):
        s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        s.settimeout(self.timeout)
        s.connect(self.path_)
        self.sock = s

def call(path, method, url, body=None):
    c = UnixConn(path)
    hdr = {"Content-Type": "application/json"} if body is not None else {}
    c.request(method, url, body=json.dumps(body) if body is not None else None, headers=hdr)
    r = c.getresponse()
    data = r.read()
    c.close()
    if r.status >= 300:
        raise RuntimeError(f"{method} {url}: {r.status} {data[:300]!r}")
    return json.loads(data) if data else None

def guest(ip):
    c = http.client.HTTPConnection(ip, port, timeout=10)
    c.request("POST", "/v1/classify", body=json.dumps({"text": text}), headers={"Content-Type": "application/json"})
    r = c.getresponse(); r.read(); c.close()
    return r.status

def pct(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1))))]

def row(name, xs):
    if not xs:
        return
    print(f"  {name:<16} p50 {pct(xs,50):8.2f}  mean {statistics.mean(xs):8.2f}  min {min(xs):8.2f}  max {max(xs):8.2f}")

PH = ["wait_ms", "check_ms", "net_ms", "spawn_ms", "socket_ms", "load_ms", "resync_ms", "cgroup_ms", "finish_ms", "total_ms"]

def wait_state(ref, state, timeout=60):
    t = time.time()
    while time.time() - t < timeout:
        m = call(sock, "GET", "/machines/" + ref)
        if m["state"] == state:
            return m
        time.sleep(0.2)
    raise RuntimeError(f"{ref} did not reach {state}")

if mode == "daemon":
    name = "lat-bench"
    try:
        call(sock, "DELETE", "/machines/" + name)
    except Exception:
        pass
    m = call(sock, "POST", "/machines", {"name": name, "from": snap})
    ip = m["ip"]
    # A fresh restore may take a moment to listen; warm the path once.
    for _ in range(100):
        try:
            guest(ip)
            break
        except OSError:
            time.sleep(0.05)
    thaw, first, e2e, phases = [], [], [], {k: [] for k in PH}
    tier = "?"
    for i in range(n):
        call(sock, "POST", f"/machines/{name}/freeze")
        time.sleep(0.5)
        t0 = time.perf_counter()
        m = call(sock, "POST", f"/machines/{name}/thaw")
        t1 = time.perf_counter()
        st = guest(ip)
        t2 = time.perf_counter()
        if st != 200:
            print(f"  iteration {i}: guest answered {st}", file=sys.stderr)
        thaw.append((t1 - t0) * 1000); first.append((t2 - t1) * 1000); e2e.append((t2 - t0) * 1000)
        w = m.get("wake") or {}
        tier = w.get("tier", tier)
        for k in PH:
            phases[k].append(float(w.get(k, 0)))
    print(f"daemon: {n} freeze -> thaw -> first request cycles of {name} (from {snap}), tier {tier}, ms")
    print(" client view")
    row("thaw call", thaw); row("first request", first); row("thaw + first", e2e)
    print(" daemon phases (api.Machine.Wake)")
    for k in PH:
        row(k[:-3], phases[k])
    call(sock, "DELETE", "/machines/" + name)
    sys.exit(0)

# gateway / paused: a kling ai serve is already listening on gwsock.
def metrics():
    c = UnixConn(gwsock); c.request("GET", "/metrics"); r = c.getresponse(); body = r.read().decode(); c.close()
    out = {}
    for line in body.splitlines():
        if not line.startswith("kling_ai_wake_phase_seconds_"):
            continue
        name, val = line.rsplit(" ", 1)
        kind = name.split("{")[0].rsplit("_", 1)[1]
        if kind not in ("sum", "count"):
            continue
        labels = dict(p.split("=", 1) for p in name.split("{", 1)[1].rstrip("}").split(","))
        k = (labels["how"].strip('"'), labels["phase"].strip('"'))
        out.setdefault(k, {})[kind] = float(val)
    return out

def classify():
    t0 = time.perf_counter()
    r = call(gwsock, "POST", "/v1/classify", {"task": "lat", "text": text})
    return (time.perf_counter() - t0) * 1000, r

def replicas():
    ms = call(sock, "GET", "/machines")
    return [m for m in ms if (m.get("labels") or {}).get("ai.gateway") == "lat"]

lat, _ = classify()
print(f"{mode}: first call (cold start from the golden snapshot) {lat:.1f} ms")
before = metrics()
e2e, states = [], []
for i in range(n):
    # Idle long enough for the reaper to put the replica down.
    t = time.time()
    while True:
        rs = replicas()
        if rs and all(m["state"] != "running" for m in rs):
            break
        if time.time() - t > 120:
            raise RuntimeError("the reaper never put the replica down")
        time.sleep(0.2)
    states.append(rs[0]["state"])
    time.sleep(0.3)
    ms, r = classify()
    e2e.append(ms)
after = metrics()
if mode == "paused":
    # What a paused replica keeps: its VMM's memory (the cost of the tier).
    t = time.time()
    while time.time() - t < 60:
        rs = [m for m in replicas() if m["state"] == "paused"]
        if rs:
            break
        time.sleep(0.2)
    for m in rs:
        pid = m.get("pid", 0)
        kv = {}
        for f in (f"/proc/{pid}/status", f"/proc/{pid}/smaps_rollup"):
            try:
                for line in open(f):
                    k, _, v = line.partition(":")
                    kv[k] = v.strip()
            except OSError:
                pass
        print(f"{mode}: paused replica {m['name']} (mem_mib {m['mem_mib']}): RSS {kv.get('VmRSS','?')} "
              f"(anon {kv.get('RssAnon','?')}, file {kv.get('RssFile','?')}), PSS {kv.get('Pss','?')}")
print(f"{mode}: {n} cycles, replica state before each call: {sorted(set(states))}; ms")
print(" client view (HTTP to kling ai serve)")
row("down -> decision", e2e)
print(" gateway phases (mean of kling_ai_wake_phase_seconds over the cycles)")
hows = sorted({h for (h, _) in after})
for how in hows:
    for phase in ["list", "renew", "wake", "ready", "first_request", "total",
                  "daemon_wait", "daemon_check", "daemon_net", "daemon_spawn", "daemon_socket", "daemon_load",
                  "daemon_resync", "daemon_cgroup", "daemon_finish", "daemon_total"]:
        a, b = after.get((how, phase), {}), before.get((how, phase), {})
        cnt = a.get("count", 0) - b.get("count", 0)
        if cnt <= 0:
            continue
        mean = (a.get("sum", 0) - b.get("sum", 0)) / cnt * 1000
        print(f"  {how:<8} {phase:<16} {mean:8.2f}  (n={int(cnt)})")
PY
}

case "$MODE" in
daemon)
  bench_py daemon "$SOCK" "$SNAP" "$N" "$PORT" "$TEXT"
  ;;
gateway|paused)
  GWSOCK="$WORK/ai.sock"
  cat >"$WORK/ai.json" <<EOF
{"models": {"lat": {"kind": "chispa", "backend": "microvm", "snapshot": "$SNAP", "max_replicas": 1}},
 "tasks": {"lat": {"chispa": "lat"}}}
EOF
  # gateway measures the frozen tier (no pausing); paused, the paused tier.
  extra=(-paused-mib 0)
  [ "$MODE" = "paused" ] && extra=(-paused-mib "${PAUSED_MIB:-256}")
  "$KLING" ai serve -config "$WORK/ai.json" -socket "$GWSOCK" -id lat -name-prefix lat-gw- \
    -idle "$IDLE" "${extra[@]}" >"$WORK/gw.log" 2>&1 &
  GWPID=$!
  for _ in $(seq 50); do [ -S "$GWSOCK" ] && break; sleep 0.1; done
  [ -S "$GWSOCK" ] || { echo "kling ai serve did not start:"; cat "$WORK/gw.log"; exit 1; }
  bench_py "$MODE" "$SOCK" "$SNAP" "$N" "$PORT" "$TEXT" "$GWSOCK"
  rc=$?
  echo " gateway log (last wake-ups):"
  grep "replica ready" "$WORK/gw.log" | tail -3 | sed 's/^/  /'
  exit $rc
  ;;
*)
  echo "usage: $0 daemon|gateway|paused" >&2
  exit 2
  ;;
esac
