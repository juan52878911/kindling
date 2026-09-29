# kindling-db

Extension module for disposable Postgres databases ("kling db"): the `kling-db`
extension and the benchmark harness that answers, honestly, *how long until a
test has its own database with schema and data?*

## kling-db

`kling db up <template>` gives a ready copy of a warm Postgres template, with a
password of its own that only this host knows; `fork`, `connect`, `reset`, `rm`,
`doctor`, `audit` and `golden` complete it. `attach <agent> <copy>` shares a copy
with an agent in another microVM through its credential proxy, without the agent ever
seeing the password (Linux only for now). Model, labels, credentials and the
Linux/macOS asymmetry: [`docs/db.md`](../../docs/db.md).

```sh
cd ext/db
go build -o ~/.local/share/kling/plugins/kling-db ./cmd/kling-db
kling db up pg -name t1 && kling db connect t1 -psql
```

| package | what |
|---|---|
| `internal/klingc` | runs the `kling` binary (the seam the tests fake) |
| `internal/scram` | SCRAM-SHA-256 verifiers (RFC 5803/7677), tested with the RFC vector |
| `internal/dbstate` | the per-copy password files on the host |

## kling-dbbench

Measures, on the SAME host, the time from "I ask for a ready database" until
`SELECT count(*) FROM <seed table>` returns the expected value, for N copies
asked at once (default 1, 8, 32), R repetitions each:

| mode | what it does |
|---|---|
| `kindling-run` | `kling run -from <golden>`, then the first query |
| `kindling-fork` | `kling sandbox fork <src> -n N`, then the first query |
| `docker` | `docker run postgres:16-alpine`, wait, apply schema+seed (what Testcontainers does) |
| `template` | `CREATE DATABASE copyN TEMPLATE tpl` on an already running Postgres |

Per mode and N it reports p50/p95/p99/max, host `MemAvailable` before and at the
peak, disk per copy, and errors. Honesty rules are those of
[`docs/thaw-at-scale.md`](../../docs/thaw-at-scale.md): every round goes in the
report, more than 1% failed copies is DEGRADED, cells that do not fit are
`skipped` with the arithmetic, nothing is rerun until it looks good.

```sh
cd ext/db
go build -o kling-dbbench ./cmd/kling-dbbench          # or GOOS=linux for the lab
GOLDEN=pg-golden DBBENCH_PASSWORD=... ./scripts/bench-db-fork.sh
```

The driver script has every variable (`MODES`, `NS`, `R`, `FORK_SRC`,
`TEMPLATE_ADDR`, `DISK_PATH`...) and a trap that removes everything named `dbb_*`.
Output: `dbbench.json`, `dbbench.md` and the log, in `docs/bench-data/db-fork-<date>/`.

Requirements the lab must meet (not checked by unit tests, they use fakes):
the golden/sandbox must have Postgres listening and the seed loaded, and be
reachable from the host on `-pg-port` (macOS: label `kling.ports=5432`).
The password is read from `$DBBENCH_PASSWORD`, never from a flag.

`internal/pgmini` is the minimal, stdlib-only Postgres client used to query
(trust, cleartext and SCRAM-SHA-256; simple query protocol; no TLS).
`Config.NoCleartext` refuses the cleartext method.

## internal/doctor

The security checks behind `kling db doctor`. `doctor.Run(ctx, kling, Target{Machine|URL}, w)`
writes a readable report (rule id, severity, message, fix) and returns the number of
problems (every finding that is not `INFO`).

| rule | what |
|---|---|
| DB001-DB004 | login roles that are SUPERUSER, BYPASSRLS, CREATEROLE or members of `pg_execute_server_program` / `pg_read_server_files` / `pg_write_server_files` (the bootstrap `postgres` of a copy is only INFO: peer auth over the local socket) |
| DB010 | fail-open RLS policies: `current_setting(...) IS NULL`, `= ''`, `COALESCE(<setting>, true or <column>)` |
| DB011, DB012 | tables with `tenant_id` and RLS off, or owned by the app role without FORCE ROW LEVEL SECURITY |
| DB020, DB021 | the app role can SET ROLE to something privileged; it can SET the tenant variable itself (INFO) |
| DB030, DB031 | `password_encryption` other than SCRAM; md5 passwords |
| DB040, DB041 | `ssl=off` on a non-loopback `-url`; the doctor itself ran without TLS or without verifying the server (only with `-insecure`) |
| DB050-DB054 | copies only: clock skew over 2 s, clients inherited from the golden, app password still the golden's, `kling.db.state` not `ready`, host password file of the copy (by machine ID) missing, too open or not matching |

A machine is checked from inside (`kling exec ... su postgres -c 'psql -X -At'`, SQL on
stdin); a `-url` from the host with `pgmini` (TLS, `sslmode=verify-full` by default with the
system roots plus `sslrootcert`/`-ca-file`; SCRAM, with `-PLUS` channel binding when
offered, never a cleartext password), reading the password from `PGPASSWORD`: a URL with
a password is refused. `sslmode=disable` or `require` on a non-loopback host needs `-insecure`.

Host reachability of a copy's Postgres differs by platform: on macOS the host reaches
port 5432 only through a forward (`kling.ports`, loopback, peer-credential checked);
on Linux any host process reaches `NSIP:5432` (the netns DNATs every port). That is
why rotating the app password in every copy is mandatory and DB052 is CRITICAL.
