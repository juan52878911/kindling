# Show HN: draft

HN rules to keep in mind:

- Show HN is for something people can run now. The link should be the repo or the article,
  not a signup page.
- No superlatives ("fastest", "revolutionary") and no exclamation marks in the title.
- Never ask anyone to upvote. Vote rings get the post flagged and the account penalised.
  Share the link without telling people to vote.
- Post the first comment yourself, right away, and stay in the thread for the first 6–8 hours.

## Title (72 chars)

**Show HN: Kindling – Firecracker microVMs that wake from frozen snapshots**

Link: `https://github.com/juan52878911/kindling` (or the article, if it's published by then:
an article link gets more discussion, a repo link more people trying it).

### Alternatives

1. `Show HN: Kindling – microVMs that freeze to disk and restore instead of booting` (79)
2. `Show HN: Kindling – MicroVM sandboxes for code agents, on Linux and macOS` (73)
3. `Show HN: Kindling – Self-hosted microVM sandboxes restored from golden snapshots` (80)

Don't put numbers ("1 ms") in the title. The figure only means something next to its caveats,
and the first reply will be someone reading it as "a VM boots in 1 ms".

## First comment

> Hi HN, I'm <PLACEHOLDER: name>. I built kindling on my own: a daemon and a CLI (`kling`)
> that run microVMs (Firecracker on Linux, Virtualization.framework on macOS) without booting
> them on the request path. Each template boots once and is frozen as a "golden snapshot":
> VM state, memory, a copy of the disk taken while paused, all signed. Every instance is a
> restore. Memory is a mapped file shared through the page cache, the disk is a reflink
> clone, and a frozen machine is just files: 0 CPU, 0 RAM.
>
> I started it to run MCP servers for a local agent, because agent-installed tooling is
> untrusted code, and I wanted a hypervisor boundary instead of a shared kernel. It grew a
> multi-tenant sandbox gateway with a warm pool claimed atomically, an
> OpenSandbox-compatible API, and an E2B shim (see docs/e2b.md for its status).
>
> On one nested EC2 instance (c8i.2xlarge, n=15 serial): a claim from the pool takes
> 0.8–1.1 ms, and a cold create (a restore, not a boot) 76–91 ms. Against OpenSandbox 1.2.1 it
> is microVM vs runc container, a small n, and OSB's exec took ~1 s for a reason I couldn't
> explain. All nine caveats are in docs/benchmarks.md, including where OSB is ahead.
>
> What isn't solved: macOS has no jailer, `allowlist` egress filters by IP (CDN co-tenants),
> authz is an MVP, there's been no external audit, and a lot of the deep docs are in Spanish
> (my first language).
>
> What I'd like from you: a review of the threat model in SECURITY.md, benchmark
> methodology holes, and reports from `kling try` on your hardware.

(That's about 280 words. Before posting, fill the name placeholder and confirm the "no
external audit" line is true.)

## FAQ: the hardest questions, with draft answers

### 1. How is this different from E2B, firecracker-containerd, Kata, gVisor or Fly Machines?

> Short version: it's a single daemon and CLI that you run on your own Linux box or Mac.
> It's designed around snapshots, not containers.
>
> - **firecracker-containerd** and **Kata** put containers inside VMs, under a container
>   runtime or Kubernetes. kindling is not an OCI runtime. It imports Docker images
>   (`kling image import`), but the unit is a microVM template frozen once and restored N
>   times.
> - **gVisor** is a userspace kernel that intercepts syscalls. kindling's boundary is a
>   hypervisor, with the guest's own kernel. (On macOS, kindling uses gVisor's network stack
>   inside its VMM helper, which is a different use.)
> - **Fly Machines** and **E2B** are platforms you mostly consume as a service. kindling is
>   self-hosted, and its gateway speaks the OpenSandbox 1.2.1 API, plus an E2B shim (see
>   docs/e2b.md for status), so existing clients can point at it.
> - What I haven't seen packaged this way elsewhere: freeze to disk as a normal lifecycle
>   state (`-ttl` freezes rather than kills), golden snapshots whose memory is shared across
>   instances, and the same CLI on Linux and macOS.
>
> I haven't benchmarked Kata, gVisor, E2B or Fly, so I make no performance claims about them.

*(Author: double-check each one-line description of the other projects before posting. They
come from general knowledge, not from this repo.)*

### 2. Is the benchmark fair?

> Partly, and docs/benchmarks.md says where it isn't. Same `bench.py`, same instance type,
> one system at a time on the host. But it's nested virtualization, one region, n=15 (5 for
> pause/freeze), and a microVM against a runc container, not against OSB with Kata or gVisor.
> "Pool" means different things in the two systems, and OSB's exec at ~1 s against its
> documented ~203 ms suggests something in my setup. OSB's raw logs aren't in the repo, and
> the script that reproduces everything is `scripts/bench/ec2.sh kindling osbk8s docker`. If
> you can spot the misconfiguration, I'll rerun it and update the table, including if it
> makes kindling look worse.

### 3. Why not just Docker?

> If the code is yours and trusted, use Docker: it's cheaper. A microVM costs more than a
> container because it runs its own kernel. The only reason for kindling is untrusted code,
> such as what an agent installs and runs: there, the isolation boundary is a hypervisor
> rather than namespaces of a kernel shared with the host. Docker images still work:
> `kling image import postgres:17-alpine` runs the image's ENTRYPOINT inside a microVM.

### 4. "1 ms" sounds like marketing. What actually takes 1 ms?

> Handing out a microVM that is already running in a warm pool: an atomic claim that labels
> it for your tenant and renews its TTL. Nothing boots. A cold create, meaning a restore from
> the golden snapshot with no pool, is 76–91 ms serial on that rig. A kernel cold boot takes
> seconds, and that's why the design avoids it. Pool machines hold RAM while they wait. That
> is the trade-off.

### 5. Is it production-ready?

> Some of it. 1.x promises SemVer for the core, daemon API, CLI and the `mcp` and `sandbox`
> extensions, and `kling upgrade` rolls back if a new version fails its check. The install
> command pins `v1.0.0-rc.3`. Not production-ready: `db`, `phone`, the Kubernetes operator
> and `compose` (experimental), and the code interpreter (`/code`). Authz is an MVP and is off
> by default. The sandbox quickstart hasn't been run end to end on KVM for this release. The
> large-scale thaw matrix (50 services × 200 agents) is unmeasured. It's maintained by one
> person. I'd run it for my own agents today, and I'd want an external review before running
> other people's tenants on it.

### 6. What happens when a guest escapes the VM? Has it been audited?

> SECURITY.md assumes the guest is hostile. A Firecracker escape lands in an unprivileged
> user (0 capabilities, `no_new_privs`, only the `kvm` group) inside a per-VM jailer chroot.
> The daemon is not on the network. Weak spots, listed in SECURITY.md: `KLING_JAILER=0` makes
> one escape reach every VM on the host; macOS has no jailer; `allowlist` egress is IP-based;
> a proxied credential can be used, but not read. <PLACEHOLDER: confirm> There's been no
> third-party audit. Reports go through GitHub private vulnerability reporting.

### 7. License and business model?

> Apache-2.0, all of it, extensions included. Business model: <PLACEHOLDER: be explicit,
> e.g. "none today, it's a personal project", or what you actually plan. HN will ask, and a
> vague answer reads as an upcoming rug-pull.>

### 8. Who's behind it? Is it AI-generated?

> One person, <PLACEHOLDER: name, location, short background>. The commit history shows a
> single author. <PLACEHOLDER: be upfront about AI coding assistants if you used them: how
> you used them and how you review and test what they produce. The PR history makes it
> visible, so say it before someone else does.> I'm a Spanish speaker, which is why parts of
> the docs, including SECURITY.md, are in Spanish. English translations are <PLACEHOLDER:
> planned / in progress>.

### Also likely

- **"Why is SECURITY.md in Spanish?"** Answer as in #8, and before launch add at least an
  English summary of the threat model and the "what is not solved" list (see plan.md).
- **"Does it need Kubernetes?"** No. It's one daemon and a CLI. There's an optional,
  experimental operator and Helm chart.
- **"Does it work on my Mac?"** Apple Silicon, macOS 14+, native backend, no root. The
  published benchmarks are from Linux/Firecracker, and isolation on macOS differs (no jailer,
  a sandbox profile instead; SECURITY.md §11 and §22).
