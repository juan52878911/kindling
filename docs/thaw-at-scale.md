# Thaw at scale

What happens when many MCP sessions arrive at once at services that are **frozen**
(scaled to zero), and what it costs the host. [`despertar.md`](despertar.md) answers
"how long does ONE frozen replica take to answer" (27 ms, phase by phase); this page
answers the follow-up question from the r/mcp thread: *and with 50 services and 200
concurrent agents?*

> **Status: pending lab run.** The method, the tools and the table are ready; the
> numbers below are empty until the lead runs the matrix on the lab host. Nothing on
> this page is a claim yet.

## What is measured

One **cell** is: `M` concurrent MCP sessions, spread round-robin over `N` services, each
session doing the full client dance through the real gateway:

1. `initialize` (the gateway places the session: adopt a running instance, thaw a
   frozen one, restore a new replica from the golden snapshot, or answer 503 at the
   replica cap),
2. `notifications/initialized`,
3. one `tools/call` (the `echo` tool),
4. `CALLS` more `tools/call` (steady state, default 5),
5. `DELETE` of the session.

All `M` sessions wait on a start barrier, so the burst is a burst and not a ramp.

Per cell, [`kling-mcpbench`](../ext/mcp/cmd/kling-mcpbench) reports:

| Figure | Meaning |
|---|---|
| **TTFR** p50/p95/p99/max | `initialize` sent → first tool result received. The headline number: what an agent opening a session on a cold service waits. |
| initialize, first call, steady, delete | The same, per phase. Steady is the hot path, with nothing to wake. |
| ok / errors | Sessions that completed every step, and failures by `phase:code` (`init:http_503`, `call:timeout`...). |
| live peak, machines | Max microVMs of the measured services alive at once, and distinct machines seen (primaries + replicas). |
| PSS peak | Sum of PSS of those microVMs (`kling_machine_pss_mib`), MiB. PSS, not RSS: copies of the same snapshot share pages. |
| min available | Lowest host `MemAvailable` during the cell. |
| PSI max | Highest `/proc/pressure/memory` `some avg10`. The daemon refuses new machines above `KLING_MAX_MEM_PRESSURE` (20 by default). |
| t→0 | After the round, time until no microVM of the measured services is alive (the gateway froze them all). Frozen cells only. |

Percentiles are nearest-rank over the full sorted sample (no histogram buckets, no
interpolation): every percentile is a latency that actually happened. Host samples come
from the daemon's `/metrics` every 250 ms and from `/proc/pressure/memory`.

Every JSON report carries a host header: kernel, CPU model and count, memory, whether the
host is itself a VM (nested virtualization), whether it runs in a container (LXC CT),
kling and Firecracker versions, the git SHA of the bench binary, and the daemon's PSI
limit.

## The matrix

| Axis | Values (default) | Notes |
|---|---|---|
| `N` services | 1, 10, 50 | `tb-0..N-1` (norep) and `tb-r0..N-1` (rep), all from one image `tb-echo`: the example stdio server ([`examples/mcp/stdio-server`](../examples/mcp/stdio-server)), static, behind `kling-bridge`. |
| `M` sessions | 1, 10, 50, 200 | Total, round-robin over the services (with `M < N`, only `M` services are touched). |
| mode | frozen, warm | **frozen**: a priming round creates the instances (and replicas), the gateway freezes them all after `-idle`, then the measured round thaws them. **warm**: priming round and, without waiting, the measured round. |
| replicas | norep, rep | Chosen by memory, because that is what decides sessions per instance (64 MiB per session, 192 reserved, max 32). **norep**: 1024 MiB → 13 sessions per instance. **rep**: 256 MiB → 1 session per instance, so every extra session on a service is a replica. |
| repetitions | R = 3 | Every repetition is its own row. |

The gateway runs with `-max-replicas 16` (the new default, see below), so a rep cell with
more than 16 sessions on one service **expects** 503s. Those rows are kept, not hidden:
that is exactly what the cap is for.

## Honesty rules

- **Every measured run goes in the table**, including failed and degraded ones. No
  reruns until it looks good; no outlier removal.
- A run with **more than 1 % failed sessions is DEGRADED**. Its latency figures are over
  the sessions that completed, so they must not be quoted as the cell's latency.
- A cell whose estimate (instances × configured memory) exceeds **70 % of available
  RAM**, or whose frozen memory files would not fit on disk, **is not run**. It appears
  as `skipped` with the arithmetic, so a missing number is visibly missing and not
  silently absent.
- Between cells the driver waits for zero live `tb-` microVMs and PSI < 5, so one cell's
  backlog does not bleed into the next. If the host does not calm down in 5 minutes the
  run goes on and says so in its log.
- The raw JSON of every run and the gateway log are kept next to the table
  (`docs/bench-data/thaw-scale-<date>/`).
- The host header travels with the numbers. Nested virtualization and an LXC container
  both change the result; a number without them is not comparable.

## The replica cap (safety fix that came with this)

Before this work the MCP gateway had **no replica cap**: `MaxReplicas` was 0 (unlimited),
so 200 simultaneous sessions against one 256 MiB service tried to create ~200
microVMs, and the only brake was the host running out of memory. `kling mcp serve` now
takes `-max-replicas N` (default **16** per service, 0 = unlimited as before). Sessions
beyond the cap get `503 ... all replicas full`, which a client can retry, and the
service is **not** marked unhealthy for it (it is full, not broken). 16 replicas of a
2 GiB service are still 512 concurrent sessions.

## Reproduce

The lab host has no Go. Cross-compile on a workstation, copy, run:

```sh
# On the workstation, from the repo root (amd64 lab; use GOARCH=arm64 for an arm64 host)
export GOOS=linux GOARCH=amd64 CGO_ENABLED=0
go build -o /tmp/tb/stdio-server ./examples/mcp/stdio-server
(cd ext/mcp && go build -o /tmp/tb/kling-mcpbench ./cmd/kling-mcpbench \
             && go build -o /tmp/tb/kling-bridge  ./cmd/kling-bridge \
             && go build -o /tmp/tb/kling-mcp     ./cmd/kling-mcp)
cp ext/mcp/scripts/95-thaw-scale.sh ext/mcp/scripts/80-mcp-image.sh /tmp/tb/
scp -r /tmp/tb juan@<lab>:~/tb

# On the lab host (user with passwordless sudo, next to the running daemon)
cd ~/tb && KLING_MCP=./kling-mcp ./95-thaw-scale.sh
```

`kling-bridge` must be the one the image was built for; if the lab already has one
installed (`make bridge`), `BRIDGE=/path/to/it` works too. The run needs the base image
(`min` by default, `BASE_IMAGE=` to change it) and free disk for the golden snapshots
(`N × memory` per replica variant, plus 3 GiB); the preflight checks both and refuses
otherwise.

A short smoke pass first is a good idea:

```sh
NS="1 10" MS="1 10" R=1 KLING_MCP=./kling-mcp ./95-thaw-scale.sh
```

Useful knobs (all environment variables, see the script header): `NS`, `MS`, `MODES`,
`REPS`, `R`, `CALLS`, `IDLE` (gateway `-idle`, 10 s), `SETTLE` (max wait for t→0,
120 s), `MAXREP` (16), `MEM_NOREP` (1024M), `MEM_REP` (256M), `KEEP=1` (leave
everything in place). The driver starts its **own** gateway on `127.0.0.1:18180` with a
one-off token passed by environment, so it does not touch the production gateway or its
token; it removes every `tb-` machine, template and the `tb-echo` image on exit and
checks that the host is back to its baseline.

To run a single cell by hand against an existing gateway:

```sh
KLING_GATEWAY_TOKEN=... ./kling-mcpbench -gateway http://127.0.0.1:8080 \
    -services tb-0..9 -sessions 50 -calls 5 -settle 60s -label manual -json out.json
```

## Results

**Pending lab run.** Host header, table and raw data will go here.

Host: _pending_

| cell | N | M | ok | TTFR p50 | p95 | p99 | max | steady p50 | p99 | live peak | machines | PSS peak MiB | min avail MiB | PSI max | t→0 s | status |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| _pending lab run_ | | | | | | | | | | | | | | | | |

Latencies in ms. Raw JSON: `docs/bench-data/thaw-scale-<date>/`.
