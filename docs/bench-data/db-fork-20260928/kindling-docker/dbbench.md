# kling-dbbench: a throwaway database per test

Started 2026-09-28T23:41:25Z, finished 2026-09-28T23:42:31Z.

**Host:** fc-test, kernel 6.17.2-1-pve, Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz, 4 CPUs, 8192 MiB RAM, nested=false, container=lxc, kling="kling v0.16.0-76-g7b2c2cc", docker="20.10.24+dfsg1", bench=7b2c2cc70e59-dirty

**Measured:** time from asking for a ready database (schema and seed) to `SELECT count(*) FROM items` returning 400000, for N copies asked at once. Modes: kindling-run, docker, template. N: [1 8 32]. R = 3.

**Preflight:**
- MemAvailable 8031 MiB
- disk-path /var/lib/kindling: 3800 MiB free
- PSI some avg10 0.00 (limit 20.00)

## Cells (all rounds pooled)

| mode | N | status | ok/total | p50 ms | p95 ms | p99 ms | max ms | MemAvailable min MiB | peak RAM used MiB | disk/copy MiB | errors |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| kindling-run | 1 | ok | 3/3 | 274.3 | 1794.2 | 1794.2 | 1794.2 | 8020 | 21 | 211.2 | - |
| kindling-run | 8 | ok | 24/24 | 777.1 | 1576.9 | 1577.7 | 1577.7 | 7904 | 136 | 209.7 | - |
| kindling-run | 32 | skipped | - | - | - | - | - | - | - | - | N=32 × 256 MiB = 8192 MiB > 70% of MemAvailable 8039 MiB |
| docker | 1 | ok | 3/3 | 3002.8 | 3063.4 | 3063.4 | 3063.4 | 7939 | 100 | 204.4 | - |
| docker | 8 | ok | 24/24 | 6435.1 | 6865.5 | 6941.9 | 6941.9 | 7297 | 727 | 204.4 | - |
| docker | 32 | skipped | - | - | - | - | - | - | - | - | N=32 × 200 MiB = 6400 MiB > 70% of MemAvailable 8018 MiB |
| template | 1 | skipped | - | - | - | - | - | - | - | - | setup failed: dial tcp 127.0.0.1:55499: connect: connection refused |
| template | 8 | skipped | - | - | - | - | - | - | - | - | setup failed: dial tcp 127.0.0.1:55499: connect: connection refused |
| template | 32 | skipped | - | - | - | - | - | - | - | - | setup failed: dial tcp 127.0.0.1:55499: connect: connection refused |

## Every round

| mode | N | rep | status | ok/N | p50 ms | p95 ms | p99 ms | max ms | MemAvailable before→min MiB | disk/copy MiB | PSI before/max | errors |
|---|---:|---:|---|---:|---:|---:|---:|---:|---|---:|---|---|
| kindling-run | 1 | 1 | ok | 1/1 | 1794.2 | 1794.2 | 1794.2 | 1794.2 | 8041→8020 | 211.2 | 0.0/0.0 | - |
| kindling-run | 1 | 2 | ok | 1/1 | 274.3 | 274.3 | 274.3 | 274.3 | 8043→8026 | 211.2 | 0.0/0.0 | - |
| kindling-run | 1 | 3 | ok | 1/1 | 260.7 | 260.7 | 260.7 | 260.7 | 8044→8025 | 211.2 | 0.0/0.0 | - |
| kindling-run | 8 | 1 | ok | 8/8 | 689.0 | 886.8 | 886.8 | 886.8 | 8041→7905 | 208.2 | 0.0/0.0 | - |
| kindling-run | 8 | 2 | ok | 8/8 | 532.7 | 1319.4 | 1319.4 | 1319.4 | 8040→7904 | 209.7 | 0.0/0.0 | - |
| kindling-run | 8 | 3 | ok | 8/8 | 841.1 | 1577.7 | 1577.7 | 1577.7 | 8039→7904 | 211.0 | 0.0/0.0 | - |
| kindling-run | 32 | - | skipped | - | - | - | - | - | - | - | - | N=32 × 256 MiB = 8192 MiB > 70% of MemAvailable 8039 MiB |
| docker | 1 | 1 | ok | 1/1 | 3063.4 | 3063.4 | 3063.4 | 3063.4 | 8039→7939 | 204.4 | 0.0/0.0 | - |
| docker | 1 | 2 | ok | 1/1 | 3002.8 | 3002.8 | 3002.8 | 3002.8 | 8039→7948 | 204.4 | 0.0/0.0 | - |
| docker | 1 | 3 | ok | 1/1 | 2937.9 | 2937.9 | 2937.9 | 2937.9 | 8034→7944 | 204.4 | 0.0/0.0 | - |
| docker | 8 | 1 | ok | 8/8 | 6760.8 | 6941.9 | 6941.9 | 6941.9 | 8034→7315 | 204.4 | 0.0/0.0 | - |
| docker | 8 | 2 | ok | 8/8 | 6435.1 | 6783.7 | 6783.7 | 6783.7 | 8024→7297 | 204.4 | 0.0/0.0 | - |
| docker | 8 | 3 | ok | 8/8 | 6126.7 | 6481.9 | 6481.9 | 6481.9 | 8021→7301 | 204.4 | 0.0/0.0 | - |
| docker | 32 | - | skipped | - | - | - | - | - | - | - | - | N=32 × 200 MiB = 6400 MiB > 70% of MemAvailable 8018 MiB |
| template | 1 | - | skipped | - | - | - | - | - | - | - | - | setup failed: dial tcp 127.0.0.1:55499: connect: connection refused |
| template | 8 | - | skipped | - | - | - | - | - | - | - | - | setup failed: dial tcp 127.0.0.1:55499: connect: connection refused |
| template | 32 | - | skipped | - | - | - | - | - | - | - | - | setup failed: dial tcp 127.0.0.1:55499: connect: connection refused |

## How to read this

- Every measured round is in the table, failed or not. Nothing was rerun or trimmed to look better.
- A round or cell with more than 1% failed copies is **DEGRADED**: its percentiles are over the copies that finished, so they are not the latency of that cell.
- A cell whose estimate does not fit is **skipped**, with the arithmetic in the errors column.
- Percentiles are nearest-rank over the full sorted sample. Cell rows pool every good copy of every round.
- Latency starts when the whole burst is released and stops when the first `count(*)` returns the expected value. It includes the tool, the wait for the database and, for docker and template, applying the schema and seed.
- Disk per copy is the change in used space of the disk-path filesystem (after sync) divided by the copies that succeeded; other writers on that filesystem add noise, and "n/a" means it was not measured.
- Nested virtualization and containers change these numbers: compare only runs whose host header matches.
