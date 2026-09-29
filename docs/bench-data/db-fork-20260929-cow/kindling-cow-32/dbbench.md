# kling-dbbench: a throwaway database per test

Started 2026-09-29T06:37:01Z, finished 2026-09-29T06:37:18Z.

**Host:** fc-test, kernel 6.17.2-1-pve, Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz, 4 CPUs, 8192 MiB RAM, nested=false, container=lxc, kling="kling v0.16.0-222-g6fae27c", docker="20.10.24+dfsg1", bench=67ee19f676fe

**Measured:** time from asking for a ready database (schema and seed) to `SELECT count(*) FROM items` returning 400000, for N copies asked at once. Modes: kindling-run. N: [32]. R = 3.

**Preflight:**
- MemAvailable 8068 MiB
- disk-path /var/lib/kindling: 4581 MiB free
- PSI some avg10 0.08 (limit 20.00)

## Cells (all rounds pooled)

| mode | N | status | ok/total | p50 ms | p95 ms | p99 ms | max ms | MemAvailable min MiB | peak RAM used MiB | disk/copy MiB | errors |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| kindling-run | 32 | ok | 96/96 | 1236.8 | 1753.9 | 1805.5 | 1805.5 | 7519 | 554 | 3.6 | - |

## Every round

| mode | N | rep | status | ok/N | p50 ms | p95 ms | p99 ms | max ms | MemAvailable before→min MiB | disk/copy MiB | PSI before/max | errors |
|---|---:|---:|---|---:|---:|---:|---:|---:|---|---:|---|---|
| kindling-run | 32 | 1 | ok | 32/32 | 1173.3 | 1675.9 | 1710.7 | 1710.7 | 8078→7524 | 3.6 | 0.1/0.1 | - |
| kindling-run | 32 | 2 | ok | 32/32 | 1320.8 | 1799.4 | 1805.5 | 1805.5 | 8073→7519 | 3.6 | 0.1/0.1 | - |
| kindling-run | 32 | 3 | ok | 32/32 | 1270.1 | 1743.7 | 1753.9 | 1753.9 | 8071→7520 | 3.6 | 0.0/0.0 | - |

## How to read this

- Every measured round is in the table, failed or not. Nothing was rerun or trimmed to look better.
- A round or cell with more than 1% failed copies is **DEGRADED**: its percentiles are over the copies that finished, so they are not the latency of that cell.
- A cell whose estimate does not fit is **skipped**, with the arithmetic in the errors column.
- Percentiles are nearest-rank over the full sorted sample. Cell rows pool every good copy of every round.
- Latency starts when the whole burst is released and stops when the first `count(*)` returns the expected value. It includes the tool, the wait for the database and, for docker and template, applying the schema and seed.
- Disk per copy is the change in used space of the disk-path filesystem (after sync) divided by the copies that succeeded; other writers on that filesystem add noise, and "n/a" means it was not measured.
- Nested virtualization and containers change these numbers: compare only runs whose host header matches.
