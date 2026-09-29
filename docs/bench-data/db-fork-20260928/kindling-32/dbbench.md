# kling-dbbench: a throwaway database per test

Started 2026-09-29T00:51:23Z, finished 2026-09-29T00:52:08Z.

**Host:** fc-test, kernel 6.17.2-1-pve, Intel(R) Core(TM) i7-8700T CPU @ 2.40GHz, 4 CPUs, 8192 MiB RAM, nested=false, container=lxc, kling="kling v0.16.0-99-g952656d", docker="20.10.24+dfsg1", bench=7b2c2cc70e59-dirty

**Measured:** time from asking for a ready database (schema and seed) to `SELECT count(*) FROM items` returning 400000, for N copies asked at once. Modes: kindling-run, docker. N: [32]. R = 3.

**Preflight:**
- MemAvailable 8037 MiB
- disk-path /var/lib/kindling: 11278 MiB free
- PSI some avg10 0.00 (limit 20.00)

## Cells (all rounds pooled)

| mode | N | status | ok/total | p50 ms | p95 ms | p99 ms | max ms | MemAvailable min MiB | peak RAM used MiB | disk/copy MiB | errors |
|---|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| kindling-run | 32 | DEGRADED | 69/96 | 10866.1 | 14857.7 | 14897.9 | 14897.9 | 6682 | 649 | 208.3 | provision×27 |
| docker | 32 | skipped | - | - | - | - | - | - | - | - | N=32 × 300 MiB = 9600 MiB > 70% of free disk 214 MiB |

## Every round

| mode | N | rep | status | ok/N | p50 ms | p95 ms | p99 ms | max ms | MemAvailable before→min MiB | disk/copy MiB | PSI before/max | errors |
|---|---:|---:|---|---:|---:|---:|---:|---:|---|---:|---|---|
| kindling-run | 32 | 1 | DEGRADED | 27/32 | 14449.9 | 14891.2 | 14897.9 | 14897.9 | 8041→7572 | 208.5 | 0.0/0.9 | provision×5 |
| kindling-run | 32 | 2 | DEGRADED | 28/32 | 4801.0 | 9552.6 | 9747.6 | 9747.6 | 7678→7029 | 208.3 | 0.7/4.0 | provision×4 |
| kindling-run | 32 | 3 | DEGRADED | 14/32 | 10866.1 | 11227.2 | 11227.2 | 11227.2 | 7181→6682 | 207.1 | 1.8/1.8 | provision×18 |
| docker | 32 | - | skipped | - | - | - | - | - | - | - | - | N=32 × 300 MiB = 9600 MiB > 70% of free disk 214 MiB |

## How to read this

- Every measured round is in the table, failed or not. Nothing was rerun or trimmed to look better.
- A round or cell with more than 1% failed copies is **DEGRADED**: its percentiles are over the copies that finished, so they are not the latency of that cell.
- A cell whose estimate does not fit is **skipped**, with the arithmetic in the errors column.
- Percentiles are nearest-rank over the full sorted sample. Cell rows pool every good copy of every round.
- Latency starts when the whole burst is released and stops when the first `count(*)` returns the expected value. It includes the tool, the wait for the database and, for docker and template, applying the schema and seed.
- Disk per copy is the change in used space of the disk-path filesystem (after sync) divided by the copies that succeeded; other writers on that filesystem add noise, and "n/a" means it was not measured.
- Nested virtualization and containers change these numbers: compare only runs whose host header matches.
