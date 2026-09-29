# kling-dbbench: a throwaway database per test

Started 2026-09-29T01:31:31Z, finished 2026-09-29T01:31:52Z.

**Host:** fc-test, kernel 6.17.2-1-pve, Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz, 4 CPUs, 8192 MiB RAM, nested=false, container=lxc, kling="kling v0.16.0-124-g32d45ac", docker="20.10.24+dfsg1", bench=7b2c2cc70e59-dirty

**Measured:** time from asking for a ready database (schema and seed) to `SELECT count(*) FROM items` returning 400000, for N copies asked at once. Modes: kindling-run. N: [1 8 32]. R = 3.

**Preflight:**
- MemAvailable 8054 MiB
- disk-path /var/lib/kindling: 10593 MiB free
- PSI some avg10 0.00 (limit 20.00)

## Cells (all rounds pooled)

| mode | N | status | ok/total | p50 ms | p95 ms | p99 ms | max ms | MemAvailable min MiB | peak RAM used MiB | disk/copy MiB | errors |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| kindling-run | 1 | ok | 3/3 | 192.1 | 1744.6 | 1744.6 | 1744.6 | 8037 | 17 | 203.4 | - |
| kindling-run | 8 | ok | 24/24 | 1237.2 | 1342.6 | 1352.0 | 1352.0 | 7917 | 138 | 205.5 | - |
| kindling-run | 32 | skipped | - | - | - | - | - | - | - | - | N=32 × 192 MiB = 6144 MiB > 70% of MemAvailable 8053 MiB |

## Every round

| mode | N | rep | status | ok/N | p50 ms | p95 ms | p99 ms | max ms | MemAvailable before→min MiB | disk/copy MiB | PSI before/max | errors |
|---|---:|---:|---|---:|---:|---:|---:|---:|---|---:|---|---|
| kindling-run | 1 | 1 | ok | 1/1 | 1744.6 | 1744.6 | 1744.6 | 1744.6 | 8054→8037 | 205.1 | 0.0/0.0 | - |
| kindling-run | 1 | 2 | ok | 1/1 | 191.1 | 191.1 | 191.1 | 191.1 | 8056→8039 | 203.4 | 0.0/0.0 | - |
| kindling-run | 1 | 3 | ok | 1/1 | 192.1 | 192.1 | 192.1 | 192.1 | 8056→8040 | 203.4 | 0.0/0.0 | - |
| kindling-run | 8 | 1 | ok | 8/8 | 1181.2 | 1352.0 | 1352.0 | 1352.0 | 8057→7920 | 205.3 | 0.0/0.5 | - |
| kindling-run | 8 | 2 | ok | 8/8 | 1251.5 | 1324.5 | 1324.5 | 1324.5 | 8055→7917 | 205.5 | 0.4/0.5 | - |
| kindling-run | 8 | 3 | ok | 8/8 | 1031.8 | 1294.9 | 1294.9 | 1294.9 | 8054→7918 | 206.3 | 0.4/0.4 | - |
| kindling-run | 32 | - | skipped | - | - | - | - | - | - | - | - | N=32 × 192 MiB = 6144 MiB > 70% of MemAvailable 8053 MiB |

## How to read this

- Every measured round is in the table, failed or not. Nothing was rerun or trimmed to look better.
- A round or cell with more than 1% failed copies is **DEGRADED**: its percentiles are over the copies that finished, so they are not the latency of that cell.
- A cell whose estimate does not fit is **skipped**, with the arithmetic in the errors column.
- Percentiles are nearest-rank over the full sorted sample. Cell rows pool every good copy of every round.
- Latency starts when the whole burst is released and stops when the first `count(*)` returns the expected value. It includes the tool, the wait for the database and, for docker and template, applying the schema and seed.
- Disk per copy is the change in used space of the disk-path filesystem (after sync) divided by the copies that succeeded; other writers on that filesystem add noise, and "n/a" means it was not measured.
- Nested virtualization and containers change these numbers: compare only runs whose host header matches.
