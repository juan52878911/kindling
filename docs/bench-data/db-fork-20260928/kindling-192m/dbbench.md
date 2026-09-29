# kling-dbbench: a throwaway database per test

Started 2026-09-29T01:32:43Z, finished 2026-09-29T01:33:34Z.

**Host:** fc-test, kernel 6.17.2-1-pve, Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz, 4 CPUs, 8192 MiB RAM, nested=false, container=lxc, kling="kling v0.16.0-124-g32d45ac", docker="20.10.24+dfsg1", bench=7b2c2cc70e59-dirty

**Measured:** time from asking for a ready database (schema and seed) to `SELECT count(*) FROM items` returning 400000, for N copies asked at once. Modes: kindling-run. N: [32]. R = 3.

**Preflight:**
- MemAvailable 8055 MiB
- disk-path /var/lib/kindling: 10593 MiB free
- PSI some avg10 0.00 (limit 20.00)

## Cells (all rounds pooled)

| mode | N | status | ok/total | p50 ms | p95 ms | p99 ms | max ms | MemAvailable min MiB | peak RAM used MiB | disk/copy MiB | errors |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| kindling-run | 32 | ok | 96/96 | 9521.5 | 15757.1 | 16172.1 | 16172.1 | 7457 | 598 | 210.9 | - |

## Every round

| mode | N | rep | status | ok/N | p50 ms | p95 ms | p99 ms | max ms | MemAvailable before→min MiB | disk/copy MiB | PSI before/max | errors |
|---|---:|---:|---|---:|---:|---:|---:|---:|---|---:|---|---|
| kindling-run | 32 | 1 | ok | 32/32 | 5957.4 | 6609.8 | 6609.9 | 6609.9 | 8055→7457 | 210.6 | 0.0/2.6 | - |
| kindling-run | 32 | 2 | ok | 32/32 | 9855.8 | 11967.3 | 12088.3 | 12088.3 | 8052→7459 | 211.1 | 1.4/1.4 | - |
| kindling-run | 32 | 3 | ok | 32/32 | 15028.2 | 16171.8 | 16172.1 | 16172.1 | 8050→7464 | 210.9 | 0.4/0.4 | - |

## How to read this

- Every measured round is in the table, failed or not. Nothing was rerun or trimmed to look better.
- A round or cell with more than 1% failed copies is **DEGRADED**: its percentiles are over the copies that finished, so they are not the latency of that cell.
- A cell whose estimate does not fit is **skipped**, with the arithmetic in the errors column.
- Percentiles are nearest-rank over the full sorted sample. Cell rows pool every good copy of every round.
- Latency starts when the whole burst is released and stops when the first `count(*)` returns the expected value. It includes the tool, the wait for the database and, for docker and template, applying the schema and seed.
- Disk per copy is the change in used space of the disk-path filesystem (after sync) divided by the copies that succeeded; other writers on that filesystem add noise, and "n/a" means it was not measured.
- Nested virtualization and containers change these numbers: compare only runs whose host header matches.
