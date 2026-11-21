# Benchmarks

Every performance number that appears in the [full guide](guide.md) (formerly the README, and the equivalent
ones in [`guia.md`](guia.md)), with the hardware and rough date it was
measured on, and the script that reproduces it. Where no dedicated script exists, the
table says so — those numbers come from a one-off run of `kling` itself, shown for
context, not as a benchmark claim.

Figures measured before the boot-latency work (A1–A4 below) and flagged STALE have been
**withdrawn** from the tables: they are replaced by the section "kindling vs OpenSandbox 1.2.1"
at the end, measured with the F0 protocol. Cold-boot, snapshot-restore and thaw figures of the old lab rig
are no longer quoted.

## The boot-latency changes (A1–A4)

| Node | Change | Where |
|---|---|---|
| A1 | Kernel command line gains `quiet`; on amd64 also disables i8042 (PS/2) emulation | `internal/machine/manager.go`, `bootArgs`/`archBootArg` |
| A2 | A booting or restoring machine runs with every vCPU at a full core (capped at the host's cores) until its image's ready probe passes (or the guest agent answers, if the image declares no probe), capped at 60 s (`KLING_READY_BOOST`) plus the `HEALTHCHECK`'s start period when the image declares one (up to 120 s more), then drops to its configured `cpu_pct`; an explicit `-cpu-pct` gets no boost. `KLING_READY_BOOST=0` restores the earlier boost: `max(cpu_pct, 100)` until the agent answers, capped at 10 s | `internal/machine/arranque_cpu.go` |
| A3 | SSH remote dialing reuses a `ControlMaster` socket (60 s persist) instead of opening a fresh connection per call | `internal/transport/dial.go` |
| A4 | The daemon polls the guest agent every 5–10 ms during boot (was a coarser interval) and records a per-phase latency breakdown | part of the N8 node, `internal/machine` |

A1 and A2 apply to the Firecracker (Linux) backend only — A1's i8042 flag is amd64-only,
and A2's boost is implemented with a Linux cgroup, so it has no effect on the `vz`
backend (macOS). A4 is backend-agnostic. A3 affects only the CLI's SSH transport, not
in-VM latency.

Any number below that measures a cold boot, a restore/thaw, or an end-to-end action that
includes one of those, is a candidate to have moved after A1/A2/A4; those rows were withdrawn.
Numbers that measure something else entirely (density in RAM, disk-per-service, hot-path
latency with no boot involved) are unaffected and are flagged **unaffected**.

## Hardware and dates

Two separate rigs produced these numbers; git history (`git log -S "<the exact figure>"
-- README.md`) is the source for the "measured around" dates, since the repo does not
keep a benchmarks log of its own before this file.

- **Lab rig**: Proxmox host, Intel i7-8700T, Firecracker v1.16.1 running **nested**
  inside a VM, kernel 6.1.177, 800 MB Ubuntu 24.04 rootfs. This is what
  `scripts/40-bench-boot.sh` runs against, and what most of the "At a glance" and
  "Measured numbers" figures come from.
- **Mac rig**: Apple Silicon (M-series), `kling-vz` backend (Virtualization.framework),
  used for the handful of numbers in the AI-gateway/Chispa sections that say "on a Mac".

## README "At a glance" table

| Claim | Value | Hardware | Measured around | Script | Status |
|---|---|---|---|---|---|
| Tool call, hot | 9 ms | Lab rig | 2026-08-13 | none — ad hoc, not scripted | unaffected (no boot on the hot path) |
| RAM of a `frozen` machine | 0 | Lab rig | — | — (definitional: a frozen machine is a file, not a process) | unaffected |
| Density: 142 microVMs in 3.9 GB | 3.9 GB / 142 | Lab rig | 2026-09-01 | none — see `docs/densidad-zram.md` for the zram density methodology | unaffected |
| 10 instances from one golden snapshot | +68 MiB total (12× denser than cold boots) | Lab rig | 2026-08-13 | none — ad hoc `kling top` reading after 10 `kling run -from` | unaffected |
| Disk for a service, layered images | 1300 MiB → 433 MiB (7-service fleet) | Lab rig | — | `docs/three-layers.md` methodology | unaffected |
| 20 concurrent calls, same service | p50 4.66 s (was 44 s before v0.4) | Lab rig | — | gateway load test, not scripted here | unaffected |

## README "Measured numbers" table

| Claim | Value | Hardware | Measured around | Script | Status |
|---|---|---|---|---|---|
| Snapshot creation | 305 ms | Lab rig | 2026-08-13 | `scripts/40-bench-boot.sh` | unaffected (Snapshot/dump path, not boot args or CPU cap) |

## README "Measured with 8 instances" block

| Claim | Value | Hardware | Measured around | Script | Status |
|---|---|---|---|---|---|
| RAM added for 8 more instances | 113 MiB total (14 MiB/instance) | Lab rig | 2026-08-13 | none — ad hoc `kling top`, not scripted | unaffected |

## README "Sandboxes for code agents" section

| Claim | Value | Hardware | Measured around | Script | Status |
|---|---|---|---|---|---|

## README "Topology report" example numbers

The `thaw 26ms/28ms/41ms` and `boot 46ms` figures in the `kling topo` example output are
illustrative sample output from one run, not a benchmark claim reproduced by a script —
the topology command prints whatever the live timings were when the screenshot was taken.
They move for the same reasons as the "Measured numbers" table above and are not tracked
here as separate rows.

## README "Chispa" and "CI failure triage" sections (Mac rig, unaffected by A1/A2)

| Claim | Value | Hardware | Measured around | Script | Status |
|---|---|---|---|---|---|
| Commit classification accuracy, Chispa alone | 0.640 in 6 µs (861 held-out commits) | N/A (CPU-only classifier, no microVM) | — | `docs/CHISPA-EVAL.md` methodology | unaffected |
| Warm generation, Mac | 9 ms | Mac rig | — | `docs/ai-gateway.md` methodology | unaffected (A1/A2 are Linux/Firecracker-only; A4 could shift agent-detection latency slightly on the frozen-replica path below) |
| Frozen replica wake, Mac | ~1.5 s | Mac rig | — | `docs/ai-gateway.md` methodology | possibly stale (A4) — worth a re-check on the vz backend, low priority |
| CI log triage, per log through the gateway | 25 ms (Chispa ~1 µs/line) | N/A (CPU-only) | — | `docs/CI-TRIAGE-EVAL.md` methodology | unaffected |

## Energy on the Mac

`scripts/94-energia-mac.sh` (no sudo; adds `powermetrics` when `sudo -n` works) starts
its own daemon on a throwaway root and samples `top -l N -c d -stats
pid,command,power,idlew,csw`, filtered to the test's processes (the daemon, each
`kling-hv`/`kling-vz` and, with vz, Apple's helper), in three scenarios: the daemon
alone, three idle machines (redis 7, 256 MiB, like an idle three-service compose) and
the same three frozen. POWER is macOS's energy impact (Activity Monitor's Energy
column, an index, not watts); IDLEW and CSW are per second. The first `top` sample is
dropped. JSON per run in `docs/bench-data/energia-mac-<vmm>-<label>-<date>.json`.

M4 (Mac16,1, macOS 26.5.1), AC power, kling-hv, other agents' benches running on the same
Mac (the scenarios took the `/tmp/mac-banco.lock` but processes outside the test still
add noise), 3-minute scenarios, 36 samples of 5 s each, one run per setting
(2026-10-28). Mean (p50) of the sum over the test's processes:

| scenario | per-thread QoS on (default) | `KLING_HV_QOS=0` |
|---|---|---|
| daemon alone | POWER 0.11 (0.1) · 0.01 idle wakeups/s · 23 (14) csw/s | POWER 0.07 (0.1) · 0.01 · 15 (10) |
| 3 idle machines | POWER **1.35 (1.5)** · **3.9 (4.8)** idle wakeups/s · 169 (167) csw/s | POWER **1.85 (1.8)** · **7.6 (8.0)** · 213 (205) |
| the 3 frozen | POWER 0.11 (0.0) · 0.05 · 17 (2.4) csw/s | POWER 0.11 (0.1) · 0.04 · 17 (4.2) |

Per `kling-hv` with three idle machines: POWER 0.42 with QoS, 0.59 without. With the Mac
this busy the QoS on the vCPU threads comes out ahead (about a quarter less energy
impact and half the idle wakeups), unlike the earlier per-thread measurement in
[backend-hv.md](backend-hv.md#qos-de-los-hilos), which saw no saving: it stays **on by
default**. One run per setting: repeat both back to back before quoting the gap
precisely. Frozen machines cost what the daemon alone costs: there is no VMM left.

Reproduce:

```
make build vz
KLING=./kling VMM=hv IMAGES_DIR=<dir with vmlinux, min.ext4, redis-7-alpine.ext4> \
  DUR=600 LABEL=qos1 ./scripts/94-energia-mac.sh
KLING_HV_QOS=0 KLING=./kling VMM=hv IMAGES_DIR=... DUR=600 LABEL=qos0 ./scripts/94-energia-mac.sh
```

## What this file does not cover

- **VON (small local LLMs)** numbers live in [`docs/von.md`](von.md) and
  [`docs/von-cpu.md`](von-cpu.md), reproduced by `scripts/96-von-bench.sh` and
  `scripts/97-von-cpu-bench.sh`. They were not part of the boot-latency changes (A1–A4
  touch machine boot/restore, not an already-warm model's inference loop), so they are
  out of scope here; see those docs directly.
- **The encoder benchmark** (`docs/codificador.md`, `scripts/98-encoder-bench.sh`) and
  **the wake-phase breakdown** (`docs/despertar.md`, `scripts/99-thaw-bench.sh`) are
  their own documents for the same reason — narrower and already tracked next to the
  feature they measure.

## Reproducing everything at once

`scripts/bench-all.sh` runs the boot/snapshot/restore benchmark
(`scripts/40-bench-boot.sh`) and, when the templates or machines they need already exist,
the VON, VON-CPU, encoder and thaw-phase benches, then prints one summary table. It needs
a host with KVM (real hardware or nested virtualization) and fails with a clear message,
before touching anything, if `/dev/kvm` is not usable — none of these numbers can be
produced without it.

## Reproducible bench protocol (F0)

`scripts/bench/` repeats the pool-claim / cold-boot / concurrency figures with a per-phase
breakdown. Full usage in [`scripts/bench/README.md`](../scripts/bench/README.md).

1. **Rig**: nested EC2 (`c8i`, `NestedVirtualization=enabled`) or any host with KVM. `scripts/bench/ec2.sh`
   creates and **deletes** it (trap + `InstanceInitiatedShutdownBehavior=terminate` + `shutdown -h +N`).
   `--dry-run` prints what it would create and delete. Launching it for real costs money.
2. **Run**: `bench.py k|kpool` — warm-up discarded, serial series (`SERIE`, 15), then bursts `CONC`
   (10, 20, 50) with `ESPERA_POOL` seconds to let the pool refill before each burst; 3 runs per point.
3. **Phases**: before and after each batch `fases.py` reads `/metrics` of the daemon and the gateway and
   prints p50/p95/mean per phase (from histogram buckets) next to the client-side latency, under `fases`
   in the JSON.
4. **Sweep**: `KLING_MAX_PARALLEL_BOOT=2,4,8,16`, tenant with and without quotas, jailer on/off.
5. **Report**: the three medians of p50/p95 per point, the phase table, host type and kindling commit.
   The old lab-rig STALE figures were withdrawn; the table below replaces them.
6. **Hygiene**: `remoto/higiene.sh verificar` runs between variants: 0 `firecracker`, 0 `kl-*` netns, 0 `cow.xfs`
   mounts, same free disk (±100 MiB), and warns about `vh-*` interfaces and `firecracker` processes that are not the
   bench daemon's. A dirty host aborts the bench.

## kindling vs OpenSandbox 1.2.1

Median of 3 rounds of p50 and p95, with n per point. **Read the caveats below before quoting any row.**
OSB cells come from the run of 2026-11-21 on the same instance type (the same `bench.py`, OSB 1.2.1 with the
kubernetes runtime on k3s v1.36.4 and the docker runtime), one system at a time on the host. Their raw logs are
not in the repository; rerun them instead of quoting older numbers. Reproduce with `scripts/bench/ec2.sh kindling osbk8s docker`.

| measure | kindling (range between rounds) | OSB 1.2.1 | n | note |
|---|---|---|---|---|
| create from pool, serial | ~1 ms | 34 ms | 15 | pre-warmed machine claimed vs. pod from the pool |
| create from pool, c10 | 7–10 ms | 180 ms (k8s Pool) | 10 per burst | |
| create from pool, c50 | 404–427 ms | 6.4 s with 25/50 failed (k8s Pool) | 50 per burst | pool of 10, refilled while the burst runs |
| create cold, serial | 88–105 ms | 0.91–1.11 s (k8s) · 524–543 ms (docker) | 15 | restored microVM vs. new container |
| create cold, c50 | p50 613–647 ms, **p95 ~1030 ms** | 4.0–4.6 s with 19/50 failed (k8s) · 7.6–7.7 s (docker) | 50 per burst, 2 bursts per round | default gate (4); the p95 always goes with the p50 |
| exec `echo` | 1.9 ms | 1003 ms | 30 | through the gateway, HTTP; OSB looks like a fixed 1 s poll |
| pause → exec | 5.9–6.8 ms | 1027 ms (docker) | 5 | OSB k8s pause not measured (`SIN_PAUSA`: it commits and pushes an image) |
| freeze → exec | 89–101 ms | no equivalent | 5 | dumps memory to disk and releases it |
| failures | 0 in ~1700 creations | 19/50 (k8s cold c50), 25/50 (k8s Pool c50) | | clean host |

Give the range between rounds, not one number. Memory per sandbox (~9 MiB) is in the JSON as `mem_por_sandbox_*_mib`.

### Caveats (all nine apply)

1. **Rig**: nested EC2 (c8i.2xlarge, 8 vCPU), not bare metal; a single instance type and a single region.
2. **Small n**: 15 in serial, 5 for pause and freeze. The ranges between rounds are given instead of a single figure.
3. **Different isolation**: microVM with its own kernel vs. a container (runc). OSB with Kata or gVisor would give
   other numbers; that was not measured.
4. **"Pool" is not the same in both**: kindling's is a set of pre-warmed machines claimed atomically (size `POOL`,
   10 in the bench, refilled in the background); OSB's is a `Pool` CRD of pods (size and refill as set in
   `osbk8s/pool.yaml`). State each size and refill policy next to the figure.
5. **Fast Sandbox's 64 ms** is the figure *they* publish, on *their* hardware; it is not reproduced here.
6. **Memory per sandbox (~9 MiB)** is marginal PSS with the golden image's shared memory, not RSS.
7. **Versions and reproduction**: OSB 1.2.1 (docker and kubernetes runtimes); kindling commit, date, gate
   (`KLING_MAX_PARALLEL_BOOT`, default 4) and `KLING_NET_POOL` as recorded in the JSON (`net_pool`) of the run;
   command: `scripts/bench/ec2.sh`. Quote them with the figures.
8. **Earlier failures (2–7 in 100)** came from orphaned VMMs left on the host by previous variants, not from
   kindling's create path. The hygiene check (`higiene.sh verificar`, protocol step 6) now aborts the bench
   if any are left.
9. **Where OSB is ahead today**: isolated session with diff/commit, a credential vault in the API, diagnostics,
   native Kubernetes CRDs and more SDKs. kindling's code interpreter (`/code`, contexts) is experimental. "Outperforms in everything" does not hold
   yet and must not be written.

New JSON keys that support the table: `kvm_serie` (KVM `pf_fixed`/`exits` delta per serial create, needs root
debugfs), `pausa_exec_nueva_ms` and `pausa_exec_ociosa_ms` (pause→exec on a fresh sandbox vs. one idle `E5_OCIOSA_S`=60 s).
