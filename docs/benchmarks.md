# Benchmarks

Every performance number that appears in the [full guide](guide.md) (formerly the README, and the equivalent
ones in [`guia.md`](guia.md)), with the hardware and rough date it was
measured on, and the script that reproduces it. Where no dedicated script exists, the
table says so — those numbers come from a one-off run of `kling` itself, shown for
context, not as a benchmark claim.

Numbers marked **STALE** were measured before the boot-latency work in this remediation
round (A1–A4 below) and need to be re-measured; the change that could move them is named
next to each one.

## The boot-latency changes (A1–A4)

| Node | Change | Where |
|---|---|---|
| A1 | Kernel command line gains `quiet`; on amd64 also disables i8042 (PS/2) emulation | `internal/machine/manager.go`, `bootArgs`/`archBootArg` |
| A2 | A booting or restoring machine gets `max(cpu_pct, 100)` (up to one full core) until the guest agent answers, capped at 10 s, then drops to its configured `cpu_pct` | `internal/machine/arranque_cpu.go` |
| A3 | SSH remote dialing reuses a `ControlMaster` socket (60 s persist) instead of opening a fresh connection per call | `pkg/transport/dial.go` |
| A4 | The daemon polls the guest agent every 5–10 ms during boot (was a coarser interval) and records a per-phase latency breakdown | part of the N8 node, `internal/machine` |

A1 and A2 apply to the Firecracker (Linux) backend only — A1's i8042 flag is amd64-only,
and A2's boost is implemented with a Linux cgroup, so it has no effect on the `vz`
backend (macOS). A4 is backend-agnostic. A3 affects only the CLI's SSH transport, not
in-VM latency.

Any number below that measures a cold boot, a restore/thaw, or an end-to-end action that
includes one of those, is a candidate to move after A1/A2/A4 and is flagged **STALE**.
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
| Thaw a frozen tool | ~30 ms | Lab rig | 2026-08-13 | `scripts/40-bench-boot.sh` | **STALE** (A2, A4) |
| Ephemeral action, end to end | 19 ms (2 ms actual execution) | Lab rig | 2026-08-13 | none — ad hoc `kling try` timing, not scripted | **STALE** (A2, A4) |
| Tool call, hot | 9 ms | Lab rig | 2026-08-13 | none — ad hoc, not scripted | unaffected (no boot on the hot path) |
| RAM of a `frozen` machine | 0 | Lab rig | — | — (definitional: a frozen machine is a file, not a process) | unaffected |
| Density: 142 microVMs in 3.9 GB | 3.9 GB / 142 | Lab rig | 2026-09-01 | none — see `docs/densidad-zram.md` for the zram density methodology | unaffected |
| 10 instances from one golden snapshot | +68 MiB total (12× denser than cold boots) | Lab rig | 2026-08-13 | none — ad hoc `kling top` reading after 10 `kling run -from` | unaffected |
| Disk for a service, layered images | 1300 MiB → 433 MiB (7-service fleet) | Lab rig | — | `docs/three-layers.md` methodology | unaffected |
| 20 concurrent calls, same service | p50 4.66 s (was 44 s before v0.4) | Lab rig | — | gateway load test, not scripted here | unaffected |

## README "Measured numbers" table

| Claim | Value | Hardware | Measured around | Script | Status |
|---|---|---|---|---|---|
| Cold boot | 2,643 ms | Lab rig | 2026-08-13 | `scripts/40-bench-boot.sh` | **STALE** (A1, A2, A4 all touch the cold-boot path) |
| Snapshot creation | 305 ms | Lab rig | 2026-08-13 | `scripts/40-bench-boot.sh` | unaffected (Snapshot/dump path, not boot args or CPU cap) |
| Restore from snapshot | ~30 ms | Lab rig | 2026-08-13 | `scripts/40-bench-boot.sh` | **STALE** (A2, A4 — the CPU boost and agent-poll interval both apply to restore) |

## README "Measured with 8 instances" block

| Claim | Value | Hardware | Measured around | Script | Status |
|---|---|---|---|---|---|
| RAM added for 8 more instances | 113 MiB total (14 MiB/instance) | Lab rig | 2026-08-13 | none — ad hoc `kling top`, not scripted | unaffected |

## README "Sandboxes for code agents" section

| Claim | Value | Hardware | Measured around | Script | Status |
|---|---|---|---|---|---|
| Sandbox `-from` a template, already has the toolchain inside | ~300 ms | Lab rig | 2026-09-22 | none — ad hoc, not scripted | **STALE** (A2, A4 — this is a restore) |
| Five sandboxes `-from`, in parallel | 0.57 s | Lab rig | 2026-09-22 | none — ad hoc, not scripted | **STALE** (A2, A4) |

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
