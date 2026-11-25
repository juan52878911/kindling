<!--
Draft for review. Every number here is in docs/benchmarks.md with its rig.
Before publishing: fill the <PLACEHOLDER>s, re-run if the code moved, and check the caveats
still match docs/benchmarks.md in substance.
-->

# MicroVM sandboxes in ~1 ms warm and under 100 ms cold: golden snapshots, warm pools, and freezing to disk

*How kindling hands out Firecracker microVMs without booting them, how it compares with
OpenSandbox 1.2.1 on one nested EC2 instance, and what is not solved. Small n, one rig, nine
caveats, all stated below the table.*

I've been building [kindling](https://github.com/juan52878911/kindling) on my own: a daemon
and a CLI, `kling`, that run microVMs (Firecracker on Linux, Virtualization.framework on
macOS). The core idea is that **a machine is never booted on the request path**. Each template
boots once and is frozen once it's ready, and every instance after that is restored from the
frozen file. A frozen machine is just files on disk: it uses no CPU and no RAM until something
thaws it.

On a c8i.2xlarge (8 vCPU, nested virtualization), each figure below is the range of the median
across three rounds:

- handing out a sandbox from a pre-warmed pool: **0.8–1.1 ms** (serial, n=15)
- creating one with no pool, which means restoring a microVM from its golden snapshot:
  **76–91 ms** (serial, n=15)
- a frozen sandbox, from `kling thaw` to its first `echo`: **92–106 ms**; a paused one:
  **6.1–6.6 ms** (n=5)

## Why microVMs at all

kindling started as a way to host MCP servers for a local AI agent. An MCP server is a
50–100 MB Node or Python process. Running it in a microVM costs more than running it in a
container, because every microVM boots its own kernel. The reason to do it anyway is that
**an agent running arbitrary open-source tooling is running untrusted code**. A container is
isolated by namespaces of a kernel it shares with the host, and a microVM by a hypervisor
boundary. That is the project's only honest justification, and the security model starts from
it: the guest is assumed hostile.

The cost is cold start. In my tests a cold boot took seconds. Firecracker's advertised 125 ms
assumes a trimmed kernel and a minimal rootfs on bare metal. Snapshot/restore skips boot
altogether: you map the memory, load the device state and resume. So every template boots
once, is frozen with its service already listening, and is restored on demand.

## What a golden snapshot contains

`kling save <machine> <name>` freezes a running machine into a template:

- **the VM state** (`snap.file`): vCPU and device state, as Firecracker writes it;
- **the guest memory** (`mem.file`);
- **a copy of the overlay disk, taken while paused**. It has to match the memory: the guest
  wakes up with its superblock, inodes and journal cached, so any other disk corrupts it;
- **`meta.json`, signed with HMAC-SHA256** under a key only root on that host can read. The
  signature covers the file hashes and the policy instances inherit (exec, egress, allowed
  domains, volumes, labels, limits). If someone edits the overlay and its hash together, or
  flips exec on, the template won't restore.

The memory also carries state that acts as configuration. The **kernel command line** is
frozen into it, and that is where the host writes whether the machine accepts `exec`, so a
service template can never become a sandbox. `kling save` refuses to freeze until the guest
answers, because a snapshot taken too early restores and then never responds. By default a
**warm child process** (a `node` that has already started, say) is frozen in, so a restore
skips the runtime's own startup.

## Restoring one file N times: three problems

**Disks.** The snapshot records disk paths, so N unmodified restores would all write to the
template's overlay. kindling loads with `resume_vm: false`, re-points each drive at the
instance's own overlay (`PATCH /drives`), then resumes. That overlay is a **reflink clone**:
native on XFS or Btrfs, or inside a loop-mounted, preallocated XFS/Btrfs store on ext4 hosts.
It shares the template's blocks until either side writes, with a per-instance kernel quota so
a compromised VMM can't fill the shared store.

**Network.** Firecracker records the host TAP name in the snapshot and won't let you change it
on restore. So every instance gets *the same* `tap0`, guest IP and MAC, inside its own network
namespace, with its own veth and NAT to the host. The daemon keeps a pool of these slots set
up in advance.

**Identity.** Restored copies share the guest's RNG state. virtio-rng plus `CONFIG_VMGENID`
makes the Linux guest reseed, but macOS has no VMGenID (I saw identical session IDs in two
separate microVMs). So after every restore the daemon sends the agent's `/resync` the host
clock and 64 bytes of `crypto/rand`, and the agent forces a CRNG reseed before hand-out. It
can't fix randomness a process already pulled into userspace (OpenSSL's DRBG in a running
`node`), so the gateways mint routing IDs themselves.

## Memory: why copies are cheap

Firecracker can back guest memory with a file. It **maps** `mem.file` privately instead of
reserving anonymous memory: pages load on demand, N instances share them in the page cache,
and each pays only for the pages it writes. On the lab rig (i7-8700T, Firecracker nested in a
Proxmox VM), 10 instances from one golden snapshot added **68 MiB** of system RAM in total
(12× denser than cold boots), and a stress test fit **142 microVMs in 3.9 GB**. RSS misleads
here because it counts shared pages once per process, so `kling top` reports PSS. On the EC2
run the marginal PSS per sandbox was ~9 MiB.

## Freeze vs pause

**Freeze** pauses the vCPUs, dumps memory and kills the VMM. A copy of a golden snapshot is
dumped as a diff: only the pages it wrote. No process is left, which is why a frozen machine
costs 0 CPU and 0 RAM. On a Mac (M4, one run), three frozen machines had the same energy
impact as the daemon alone. Thawing starts a new VMM and restores. On the copy-on-write store,
rebuilding full memory from the template's mirror plus the diff is reflink metadata work, with
no data copied.

**Pause** keeps the VMM alive with its vCPUs stopped. RAM stays held, and waking it is a single
`Resume`, with no resync: same machine, and its clock kept following the host's.

Measured from invoking `kling thaw` to the first `echo` answered, a pause costs **6.1–6.6 ms**
and a freeze **92–106 ms**. You're trading RAM for latency. The AI gateway does both: it
pauses the busiest small replicas within a memory budget and freezes the rest.

When I instrumented the wake phase by phase (`docs/despertar.md`), Firecracker's
`LoadSnapshot` was a small slice. Most of the time went to rebuilding the network namespace,
starting the jailer, waiting for the VMM socket, and the guest's first steps after restore.
The fixes were plain engineering: keep a frozen machine's network for 30 minutes, pre-read
`mem.file` into the page cache, spawn the VMM straight into its cgroup
(`CLONE_INTO_CGROUP`). Each change went in only if the measured number dropped.

## The pool, claimed atomically

For the 1 ms figure, nothing is created. A template declares a pool, and the sandbox gateway
keeps that many machines of it **running**. A create is a claim: one daemon call,
`POST /sandboxes/claim`, which picks a running pool machine with matching labels, then labels
it for the tenant, names it and renews its TTL, all in one critical section. Two concurrent
claims can't get the same machine, and a failed claim is cleaned up by nonce. Before this
call existed, a claim took two phases with a fixed 50 ms wait.

One detail bit me: pool machines used to freeze after an hour, which quietly made every
"warm" create cold. Now the pool keeps their TTL alive, and refills wait 300 ms after the last
claim and run 2 at a time, so they don't compete with sandboxes in use.

To be explicit: **1 ms is the time to hand out a VM that already exists**, and pool machines
hold RAM while they wait.

## The cold path, and what bounded the c50 p95

"Cold" here means no pool: a restore from the golden snapshot, not a kernel boot. The create
takes a pre-built network slot, clones the overlay in parallel, spawns jailer and Firecracker
in the machine's cgroup, loads the snapshot, re-points the drives, resumes and resyncs. It's
ready when the guest agent answers.

Work on latency began with measurement: every create records a per-phase breakdown in
`/metrics`. Figures measured before the boot changes (a quiet kernel command line, full-core
vCPUs until ready, faster agent polling) were **withdrawn** from the docs, not footnoted. On
the same host, `main` at the time (c98ec937) measured 137–152 ms cold serial and 827–878 ms
cold c50. The gap comes from the changelog's create-path work: the network slot pool, overlay
clone and network setup in parallel, no per-create re-hash of the base image, and the launch
gate.

At 50 concurrent cold creates, the p50 is **603–633 ms** and the p95 **977–1072 ms**. What
bounds them is the **launch gate**, a cap on how many VMMs spawn or restore at once. On a
nested host with ≥ 8 cores it is `min(cores/2, 8)`, which is **4** here. With 50 arrivals and
4 slots, later requests queue behind earlier ones, and that queue shapes the tail. The gate
exists because starting everything at once on a small host causes memory spikes and VMM
deaths, and it backs off after unexpected deaths. `KLING_MAX_PARALLEL_BOOT` overrides it. The
protocol sweeps 2/4/8/16, but the table shows the default. I haven't published a breakdown
that splits c50 time into queueing and work, so "the tail is queueing" is the mechanism, not
a measured result.

## Against OpenSandbox 1.2.1

Both systems ran the same `bench.py` on the same instance type on 2026-11-21, one system at a
time. OSB ran with its kubernetes runtime (k3s v1.36.4) and its docker runtime. kindling ran
at <PLACEHOLDER: commit, net_pool and jailer setting from the run JSON> with the default gate
(4). Cells are the range of the median across 3 rounds.

| measure | kindling | OSB 1.2.1 | n |
|---|---|---|---|
| create from pool, serial | 0.8–1.1 ms | 34 ms | 15 |
| create from pool, c10 | 8–10 ms | 180 ms (k8s Pool) | 10 per burst |
| create with a pool of 10, c50 (~40 cold) | 394–432 ms | 6.4 s, 25/50 failed (k8s Pool) | 50 per burst |
| create cold, serial | 76–91 ms | 0.91–1.11 s (k8s) · 524–543 ms (docker) | 15 |
| create cold, c50 | p50 603–633 ms, p95 977–1072 ms | 4.0–4.6 s, 19/50 failed (k8s) · 7.6–7.7 s (docker) | 50 per burst, 2 per round |
| exec `echo` | 1.7–1.9 ms | 1003 ms (unexplained; OSB documents ~203 ms) | 30 |
| pause → exec | 6.1–6.6 ms | 1027 ms (docker; includes the same ~1 s exec) | 5 |
| freeze → exec | 92–106 ms | no equivalent | 5 |
| failures | 0 in ~1700 creates | 19/50 (k8s cold c50), 25/50 (k8s Pool c50) | |

**The caveats, all nine:**

1. **One nested rig.** A c8i.2xlarge with nested virtualization, not bare metal. One instance
   type, one region.
2. **Small n.** 15 in serial, 5 for pause and freeze, which is why the cells are ranges.
3. **Different isolation.** A microVM with its own kernel against a runc container. OSB with
   Kata or gVisor would differ, and I didn't measure that.
4. **"Pool" means different things.** Mine is running machines claimed atomically (10 here,
   refilled in the background). OSB's is a `Pool` CRD of pods, configured in
   `scripts/bench/osbk8s/pool.yaml`.
5. **Fast Sandbox's 64 ms** is the figure its authors publish, on their hardware. I didn't
   reproduce it.
6. **~9 MiB per sandbox** is marginal PSS, not RSS.
7. **Versions and reproduction.** OSB 1.2.1. kindling's commit, gate and network pool are in
   the run JSON. The command is `scripts/bench/ec2.sh kindling osbk8s docker`.
8. **Earlier failures.** Earlier runs had 2–7 failures in 100. They came from orphaned VMMs
   left by previous variants, not from the create path. A hygiene check now aborts a dirty
   run.
9. **Where OSB is ahead.** OSB has a credential vault in its API, diagnostics, native
   Kubernetes CRDs and more SDKs. It also has isolated sessions, though their diff and commit
   endpoints are stubs. kindling's code interpreter is experimental. kindling does not win
   everywhere.

**OSB's ~1 s exec.** I trust the OSB exec number least. Their docs say about 203 ms, and I
measured about a second. I haven't profiled it, so I'm not claiming a cause, and it inflates
their pause → exec row as well. OSB's raw logs aren't in the repo, so rerun it rather than
trust my table. If you know OSB and can see what I set up wrong, please tell me.

## Security model

`SECURITY.md` starts from one premise: **the guest is hostile**. It protects the host and the
local network, and writes off whatever runs inside a microVM.

- **No network listener.** The daemon serves a Unix socket only, since controlling microVMs is
  root-equivalent. Remote access is over SSH. Optional authz maps the peer uid to `admin` or
  `tenant:<name>`.
- **Unprivileged VMM.** Firecracker runs as a service user with 0 effective capabilities,
  `no_new_privs`, and only the `kvm` group.
- **Jailer by default.** It is mandatory on Linux: each VMM gets its own chroot, and the
  daemon verifies via `SO_PEERCRED` that each API socket's process is rooted in that VM's
  jail.
- **Egress `none` by default.** `internet` never reaches private ranges or cloud metadata,
  and `allowlist` fails closed. Both were verified from inside the guest.
- **Limits.** CPU cgroup, `memory.max`, `pids.max`, disk and network rate limits, and a
  machine cap.
- **Secrets.** Exec is opt-in at boot. Secrets enter the live machine through MMDS, never a
  snapshot. A credential proxy can keep an API key out of the guest entirely, leaving a
  placeholder that the host swaps on the way out.

## What is not solved yet

`SECURITY.md` keeps this list on purpose ("a list of guarantees without its limits is
propaganda"). The most important items:

- **`KLING_JAILER=0` exists.** With it, every VMM shares one user and has no chroot, so a VMM
  escape reaches every microVM on the host. It's meant for a single-user host.
- **macOS has no jailer.** The VMM helper confines itself with a sandbox profile, and there
  are logged cases where it runs without one.
- **No authz policy by default.** Reaching the socket means controlling everything. The policy
  mode is an MVP: names are global, and quotas count declared resources, not written ones.
- **`allowlist` filters by IP, not name.** A domain behind a CDN opens the door to the CDN's
  other tenants, and subdomains of an allowed domain can carry DNS tunnelling. An SNI mode is
  designed but not implemented.
- **Smaller gaps.** Hairpin NAT through the router isn't filtered. A proxied credential can't
  be read, but it can be used against its own domain. Disk quotas are per overlay, not per
  host. There's no encryption at rest (use dm-crypt). Userspace RNG state from before a freeze
  is shared across replicas.

Beyond security:

- **Scale.** "50 services × 200 agents" is still unmeasured: those cells didn't fit the test
  host's RAM and were skipped, not extrapolated.
- **Host variance.** On one host with a ZFS data root, frozen wakes were much slower, and I
  haven't measured why (`docs/thaw-at-scale.md`).
- **Quickstart.** The sandbox quickstart hasn't been run end to end on KVM for this release.
- **E2B shim.** For the real-SDK status, see `docs/e2b.md`.
- **OpenSandbox shim.** It implements 41 of the 90 operations in the 1.2.1 specs, the rest
  return a documented 501, and OSB's Python SDK runs against it in CI.
- **Stability.** 1.x promises SemVer for the core, daemon API, CLI and the `mcp`/`sandbox`
  extensions. `db`, `phone`, the Kubernetes operator and `compose` are experimental.

## Try it

Linux with KVM (amd64 or arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sudo sh -s -- --tag v1.0.0-rc.3
sudo kling up -install   # firecracker, jailer, the kindling users, the systemd unit; starts it
kling try -- uname -a
```

Mac with Apple Silicon (macOS 14+, no root):

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | KLING_VERSION=v1.0.0-rc.3 sh
kling try -- uname -a
```

`install.sh` checks the signature of `SHA256SUMS` and each binary against it, but read it
before you pipe it to a shell. Then:

```sh
kling run -name demo         # boot a machine
kling freeze demo            # freeze it to disk: 0 CPU, 0 RAM
kling thaw demo
```

For agent sandboxes (a token, the gateway, a template with a warm pool, then curl, Python,
TypeScript and E2B clients), see
[`docs/quickstart-sandbox.md`](https://github.com/juan52878911/kindling/blob/main/docs/quickstart-sandbox.md).
To see the benchmark plan, run `scripts/bench/ec2.sh --dry-run kindling osbk8s docker`.
Running it for real launches an EC2 instance that deletes itself, and it costs money.

Apache-2.0. Benchmark corrections and bug reports are welcome. Security reports should go
privately through the repository's Security tab.
