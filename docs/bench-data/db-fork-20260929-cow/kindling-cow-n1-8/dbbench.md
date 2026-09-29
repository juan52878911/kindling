# kling-dbbench: a throwaway database per test

Started 2026-09-29T06:36:25Z, finished 2026-09-29T06:36:43Z.

**Host:** fc-test, kernel 6.17.2-1-pve, Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz, 4 CPUs, 8192 MiB RAM, nested=false, container=lxc, kling="kling v0.16.0-222-g6fae27c", docker="20.10.24+dfsg1", bench=67ee19f676fe

**Measured:** time from asking for a ready database (schema and seed) to `SELECT count(*) FROM items` returning 400000, for N copies asked at once. Modes: kindling-run. N: [1 8 32]. R = 3.

**Preflight:**
- MemAvailable 8079 MiB
- disk-path /var/lib/kindling: 4581 MiB free
- PSI some avg10 0.00 (limit 20.00)

## Cells (all rounds pooled)

| mode | N | status | ok/total | p50 ms | p95 ms | p99 ms | max ms | MemAvailable min MiB | peak RAM used MiB | disk/copy MiB | errors |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| kindling-run | 1 | ok | 3/3 | 133.2 | 2043.3 | 2043.3 | 2043.3 | 8059 | 21 | 3.6 | - |
| kindling-run | 8 | ok | 24/24 | 352.2 | 425.7 | 432.4 | 432.4 | 7947 | 133 | 3.6 | - |
| kindling-run | 32 | skipped | - | - | - | - | - | - | - | - | N=32 × 128 MiB = 4096 MiB > 70% of free disk 4581 MiB |

## Every round

| mode | N | rep | status | ok/N | p50 ms | p95 ms | p99 ms | max ms | MemAvailable before→min MiB | disk/copy MiB | PSI before/max | errors |
|---|---:|---:|---|---:|---:|---:|---:|---:|---|---:|---|---|
| kindling-run | 1 | 1 | ok | 1/1 | 2043.3 | 2043.3 | 2043.3 | 2043.3 | 8080→8059 | 3.6 | 0.0/0.0 | - |
| kindling-run | 1 | 2 | ok | 1/1 | 133.2 | 133.2 | 133.2 | 133.2 | 8079→8062 | 3.6 | 0.0/0.0 | - |
| kindling-run | 1 | 3 | ok | 1/1 | 132.5 | 132.5 | 132.5 | 132.5 | 8079→8063 | 3.6 | 0.0/0.0 | - |
| kindling-run | 8 | 1 | ok | 8/8 | 349.4 | 404.6 | 404.6 | 404.6 | 8080→7947 | 3.6 | 0.0/0.0 | - |
| kindling-run | 8 | 2 | ok | 8/8 | 340.6 | 422.1 | 422.1 | 422.1 | 8078→7947 | 3.6 | 0.0/0.0 | - |
| kindling-run | 8 | 3 | ok | 8/8 | 352.2 | 432.4 | 432.4 | 432.4 | 8079→7947 | 3.6 | 0.0/0.0 | - |
| kindling-run | 32 | - | skipped | - | - | - | - | - | - | - | - | N=32 × 128 MiB = 4096 MiB > 70% of free disk 4581 MiB |

## How to read this

- Every measured round is in the table, failed or not. Nothing was rerun or trimmed to look better.
- A round or cell with more than 1% failed copies is **DEGRADED**: its percentiles are over the copies that finished, so they are not the latency of that cell.
- A cell whose estimate does not fit is **skipped**, with the arithmetic in the errors column.
- Percentiles are nearest-rank over the full sorted sample. Cell rows pool every good copy of every round.
- Latency starts when the whole burst is released and stops when the first `count(*)` returns the expected value. It includes the tool, the wait for the database and, for docker and template, applying the schema and seed.
- Disk per copy is the change in used space of the disk-path filesystem (after sync) divided by the copies that succeeded; other writers on that filesystem add noise, and "n/a" means it was not measured.
- Nested virtualization and containers change these numbers: compare only runs whose host header matches.
