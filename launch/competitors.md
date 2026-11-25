# Agent sandbox landscape (November 2026)

Snapshot as of 2026-11-25, to feed kindling's positioning and its "how are you different" FAQ.
Every figure is cited. Vendor numbers are vendor claims, measured on their hardware with their
own definition of "start"; **no row here was reproduced by kindling** except where it says so.
Cells marked *unverified* could not be checked against a primary source. Pages were read
through search results, not opened directly, so recheck any figure before quoting it in public.

A note on "start time": vendors measure different things (cold boot, restore from a snapshot,
claim from a warm pool, resume from standby). A pool claim and a cold boot cannot be compared.
Fly.io's own comparison notes that none of E2B, Modal, Daytona or Sprites publishes p50/p99
start latency ([Fly.io](https://fly.io/learn/sprites-vs-e2b-vs-modal-vs-daytona/)).

## Landscape

| Product | Open source? | Isolation | Self-hostable? | Pricing model | Published start / create latency |
|---|---|---|---|---|---|
| **E2B** | Yes, Apache-2.0 core and infra ([E2B open source](https://e2b.dev/open-source)) | Firecracker microVM ([Developers Digest](https://www.developersdigest.tech/agent-sandboxes/e2b)) | Yes, via Terraform from `e2b-dev/infra`. The repo README marks GCP supported and AWS in progress; the guide expects a Cloudflare-managed domain ([self-host.md](https://raw.githubusercontent.com/e2b-dev/infra/main/self-host.md), [infra](https://github.com/e2b-dev/infra)). BYOC is an Enterprise option ([E2B BYOC](https://docs.e2b.dev/byoc)). | Per second: $0.000014 per vCPU-s and $0.0000045 per GiB-s ([E2B docs](https://docs.e2b.dev/faq/calculate-sandbox-price.md)); Pro $150/month (third party: [Morph](https://www.morphllm.com/e2b-pricing)) | "~150 ms" is repeated by third parties but was not found on E2B's own pages (*unverified*); paused-sandbox resume is about 1 s per E2B's persistence docs, as cited by [Fly.io](https://fly.io/learn/sprites-vs-e2b-vs-modal-vs-daytona/) |
| **Daytona** | **Closed source since 2026-06-11.** The last open release stays public; the community fork Nightona continues from v0.190.0 under AGPL-3.0 ([Daytona](https://daytona.io/dotfiles/updates/daytona-is-going-closed-source), [Nightona](https://cdn.jsdelivr.net/gh/nightona-co/nightona@main/README.md)) | Contested: containers per a critical comparison, a dedicated kernel per Daytona's materials ([Upstash](https://upstash.com/blog/best-daytona-alternatives), *unverified*) | Only the archived open release or the fork | Usage-based (*unverified*) | "Sub 90ms sandbox creation" (vendor, [daytona.io](https://www.daytona.io)); one analysis says it likely measures a container start from a cached image ([pixeljets](https://pixeljets.com/blog/ai-sandboxes-daytona-vs-microsandbox/)) |
| **Modal Sandboxes** | No | gVisor by default, with a VM mode ([Northflank](https://northflank.com/blog/modal-vs-vercel-sandbox), third party) | No | Per-second compute; Starter $30/month in credits, Team $250/month plus compute ([Beam](https://www.beam.cloud/blog/modal-pricing-explained), third party) | "Sub-second" (third party); memory snapshots for Sandboxes are alpha ([startuphub](https://www.startuphub.ai/news/daytona-vs-e2b-vs-modal-vs-vercel-sandbox-2026)) |
| **Morph Cloud** | No | VM snapshots ("Infinibranch") | No | Morph Compute Units: 1 MCU = 1 vCPU-h, 4 GB RAM-h, 16 GB disk-h or 5 TB snapshot-h; about $0.05/MCU per a third party ([Agent37](https://www.agent37.com/compare/morph)) | Snapshot, branch and restore "in under 250ms" (vendor, [Morph](https://cloud.morph.so/docs/developers)); 176 ms median resume measured by a third party over 38 runs ([Agent37](https://www.agent37.com/compare/morph)) |
| **Vercel Sandbox** | No | Firecracker microVM ([Vercel](https://vercel.com/sandbox)) | No | Active CPU $0.128/h on Pro (I/O wait not billed) plus provisioned memory; the GB-h rate differs between sources ([Vercel pricing](https://vercel.com/docs/sandbox/pricing)) | Not found (*unverified*) |
| **Cloudflare Sandbox SDK / Containers** | SDK yes; the platform no | "Each sandbox in its own VM" ([Cloudflare security docs](https://developers.cloudflare.com/sandbox/concepts/security)); the scheduler supports gVisor, Firecracker and QEMU, and which one sandboxes use is not documented ([Cloudflare blog](https://blog.cloudflare.com/container-platform-preview)) | No | Active CPU: $0.000020 per vCPU-s and $0.0000025 per GiB-s, with a free allotment ([Cloudflare](https://www.cloudflare.com/products/sandboxes/)). GA on 2026-04-13 ([changelog](https://developers.cloudflare.com/changelog/post/2026-04-13-containers-sandbox-ga/)) | Not found (*unverified*) |
| **Fly Machines / Sprites** | No | Firecracker microVM ([Fly.io](https://fly.io/blog/fly-machines/)) | No | Machines per second, from $0.0082/h (shared-cpu-1x, 1 GB); Sprites $0.07 per CPU-h and $0.04375 per GB-h in use ([Fly.io pricing](https://fly.io/learn/ai-sandbox-pricing/)) | Machines "milliseconds" to "under a second" depending on the source; Sprites cold start 1–2 s ([Fly.io](https://fly.io/learn/sprites-vs-e2b-vs-modal-vs-daytona/)) |
| **Northflank Sandboxes** | No | microVMs for CPU (Kata with Cloud Hypervisor, per several sources), gVisor for GPU ([Northflank docs](https://northflank.com/docs/v1/application/sandboxes/sandboxes-on-northflank-cloud.md)) | BYOC, in your own cloud ([Northflank](https://northflank.com/product/sandboxes)) | About $0.01667 per vCPU-h and $0.00833 per GB-h, per second ([Northflank](https://northflank.com/blog/ai-sandbox-pricing)) | Not found (*unverified*) |
| **Runloop** | No | microVM per Devbox ([Runloop](https://runloop.ai/product/security-compliance)) | VPC deployment ([Runloop](https://runloop.ai/deploy-to-vpc)) | $0.108 per CPU-h, $0.0252 per GB-h ([AWS Marketplace](https://aws.amazon.com/marketplace/pp/prodview-6v2m5a6coprts)) | "Sub-second startup"; p95 command execution under 50 ms (vendor, AWS listing) |
| **OpenSandbox** (opensandbox-group, formerly alibaba) | Yes, Apache-2.0; about 11.5k stars in June 2026 ([star-history](https://www.star-history.com/opensandbox-group/opensandbox)) | Containers (runc) by default; gVisor, Kata or Firecracker as secure runtimes ([README](https://github.com/alibaba/OpenSandbox)) | Yes: Docker or Kubernetes | Free (your infrastructure) | Its docs claim ~203 ms exec (as cited in kindling's bench). **Measured by kindling** on the same c8i.2xlarge with OSB 1.2.1: cold create 0.91–1.11 s on k8s and 524–543 ms on docker; pool create 34 ms serial ([kindling benchmarks](../docs/benchmarks.md#kindling-vs-opensandbox-121), caveats included) |
| **CubeSandbox** (Tencent Cloud) | Yes, Apache-2.0, released 2026-04-21 ([WaveSpeed](https://wavespeed.ai/blog/posts/what-is-cubesandbox-for-ai-agents/)) | KVM microVMs on a RustVMM-based VMM (not Firecracker) ([dev.to](https://dev.to/jerrylin_0101/cube-sandbox-is-now-open-source-why-we-built-a-fast-and-secure-sandbox-for-ai-agents-d4k)) | Yes. **Its CubeAPI gateway is E2B-compatible**: switching only changes a URL environment variable ([README](https://github.com/lipiji/CubeSandbox)) | Free (your infrastructure) | "Under 60ms" average end-to-end cold start and under 5 MB of overhead per sandbox (vendor); one review mentions about 9.9k stars ([dev.to](https://dev.to/andrew-ooo/cubesandbox-review-tencents-sub-60ms-ai-agent-sandbox-5ch2)) |
| **microsandbox** | Yes (license *unverified*) | libkrun microVM, its own guest kernel ([README](https://github.com/superradcompany/microsandbox)) | Yes, embeddable, "no long-running daemon"; beta | Free | "Average boot times under 100 milliseconds" (vendor, README) |
| **Zeroboot** | Yes, Apache-2.0; "working prototype" ([README](https://raw.githubusercontent.com/zerobootdev/zeroboot/main/README.md)) | KVM VMs forked copy-on-write from a Firecracker snapshot | Yes | Free | **Spawn p50 0.79 ms** (vendor). Forks are single-vCPU, have no networking and talk over serial only ([README](https://raw.githubusercontent.com/zerobootdev/zeroboot/main/README.md), [rywalker](https://rywalker.com/research/zeroboot)) |
| **edvabe** | Yes, MIT ([pkg.go.dev](https://pkg.go.dev/github.com/contember/edvabe)) | *unverified* | Yes: a single binary that is a local E2B-compatible runtime for the unmodified E2B SDKs | Free | Not found |
| **Blaxel** (reported acquired by Baseten on 2026-09-10; single source, *unverified* [Northflank](https://northflank.com/blog/northflank-vs-blaxel)) | No | microVM | No | Active RAM $0.0000115 per GB-s; no memory charge in standby (vendor-hosted comparison) | Resume from standby "under 25 milliseconds" (vendor, [docs](https://docs.blaxel.ai/Sandboxes/Overview)) |
| **AWS Lambda MicroVMs** (launched June 2026) | No | Firecracker | No | Per second, plus snapshot operations and storage; no compute charge while suspended ([AWS](https://aws.amazon.com/lambda/lambda-microvms), [The Register](https://theregister.com/devops/2026/06/23/aws-debuts-lambda-microvms-with-up-to-8-hours-runtime/5260035)) | "Near-instant launch and resume", up to 8 h of state (vendor) |
| **kubernetes-sigs/agent-sandbox** | Yes | Runtime-agnostic: gVisor or Kata through RuntimeClass ([docs](https://agent-sandbox.sigs.k8s.io/docs/)) | Yes, on Kubernetes (GKE has a managed version) | Free | Cold start "less than one second" with SandboxWarmPool (launch post: [Google](https://opensource.googleblog.com/2025/11/unleashing-autonomous-ai-agents-why-kubernetes-needs-a-new-standard-for-agent-execution.html)) |
| **Kata Containers** | Yes, Apache-2.0 | A lightweight VM per pod (QEMU, Cloud Hypervisor or Firecracker) | Yes (an OCI runtime, not a sandbox product) | Free | Depends on the VMM and the host (*unverified*) |
| **gVisor** | Yes, Apache-2.0 | User-space kernel that intercepts syscalls; no hardware virtualization | Yes (`runsc` OCI runtime) | Free | Close to a container start (*unverified*) |
| **kindling** | Yes, Apache-2.0 | Firecracker microVM (Linux/KVM, amd64 and arm64); Virtualization.framework (macOS, Apple Silicon, no root) | Yes: one daemon and CLI; the sandbox gateway has a Helm chart | Free (your hardware) | **Measured by kindling** on nested EC2 (c8i.2xlarge), median of 3 rounds: pool claim 0.8–1.1 ms serial; cold create (snapshot restore) 76–91 ms serial; cold c50 p50 603–633 ms and p95 977–1072 ms; `exec echo` 1.7–1.9 ms; 0 failures in about 1700 creates ([benchmarks](../docs/benchmarks.md#kindling-vs-opensandbox-121)) |

### Facts that change the positioning

1. **kindling is not the only E2B-compatible server you can host yourself.** CubeSandbox (Tencent,
   Apache-2.0, about 9.9k stars per one review, under 60 ms claimed) and edvabe (MIT, single binary)
   both say they run the unmodified E2B SDKs, and E2B's own infra is Apache-2.0. "Drop-in E2B
   replacement" alone does not set kindling apart. Speaking **both** E2B and OpenSandbox 1.2.1,
   on a laptop as well as a server, still looks uncommon (no other product found that does both).
2. **Someone else already claims sub-millisecond creation.** Zeroboot publishes a spawn p50 of
   0.79 ms, which is the same order as kindling's 0.8–1.1 ms pool claim. Zeroboot forks a fresh VM
   (single vCPU, no network, prototype); kindling hands over a machine that was already warm. Never
   write "the only sub-ms sandbox". Say "0.8–1.1 ms to claim a pre-warmed microVM, 76–91 ms to
   restore one cold, on nested EC2" and link the caveats.
3. **kindling's cold number is in the same band as vendor claims, not ahead of them**: Daytona
   under 90 ms, CubeSandbox under 60 ms, microsandbox under 100 ms, Blaxel under 25 ms (resume).
   Those are vendor numbers on vendor hardware; kindling's were measured on *nested*
   virtualization, which is slower than bare metal. The claim that survives is "reproducible:
   here is the script" (`scripts/bench/ec2.sh`), not "fastest".
4. **Daytona leaving open source** (June 2026) frees up "open-source, self-hosted alternative to
   Daytona", and the E2B and Daytona APIs are the ones agent frameworks already integrate.

## Five honest differentiators

1. **It runs on a laptop and on a server with the same binary.** On a Mac it is native
   Virtualization.framework without root; on Linux it is Firecracker plus jailer, set up by
   `sudo kling up -install`. Self-hosting E2B means Terraform on GCP (AWS in progress) with a
   Cloudflare domain. OpenSandbox's isolated runtimes assume Kubernetes. kindling needs one
   `install.sh` (README).
2. **It speaks two other vendors' protocols plus its own.** `kling sbx e2b` and `kling sbx osb`
   (OpenSandbox 1.2.1, tested with the real Python SDK) sit in front of the same gateway, so
   frameworks that support either one can point at kindling by changing environment variables
   (see `launch/integrations.md` for which ones). Every unsupported endpoint answers a precise 501
   instead of failing silently (`docs/e2b.md`, `docs/opensandbox.md`).
3. **Idle machines cost nothing, and published numbers can be rerun.** A frozen machine is a file
   on disk with no CPU and no RAM, and comes back in tens of milliseconds. Every figure in
   `docs/benchmarks.md` has a script, a rig, a date and caveats, including where the other side
   wins.
4. **It is one runtime for the whole agent stack, not just code execution.** The same daemon hosts
   MCP servers that wake on demand behind one gateway (`kling mcp`), throwaway Postgres, MySQL or
   Redis per test (`kling db`), and Android phones (`kling phone`), all isolated by a hypervisor.
5. **A hostile guest is the starting assumption, and the gaps are written down.** `SECURITY.md`
   states the threat model and what is not solved. Egress goes through per-sandbox SNI allowlists.
   The recipes in `docs/recetas/` keep the model API key outside the VM. It is Apache-2.0 with no
   open-core split, and the 1.x stability promise is in writing (`docs/compatibilidad.md`).

## Where competitors are clearly better

- **Managed scale and reach:** Modal, Cloudflare, Vercel, Fly and AWS run fleets across regions
  with quotas in the thousands. Cloudflare reports up to 15,000 concurrent lite instances on its
  standard plan ([InfoQ](https://infoq.com/news/2026/04/cloudflare-sandboxes-ga)). kindling is your
  hardware, and multi-host scheduling is up to you.
- **GPUs:** Modal and Northflank (gVisor GPU sandboxes) offer them; kindling's microVMs do not.
- **The E2B feature surface:** template builds, persistent snapshots and fork across hosts, the
  Jupyter code interpreter, metrics, logs and PTY are all 501 in kindling's shim today
  (`docs/e2b.md`). `run_code` from `e2b_code_interpreter` does not work yet, and that rules out
  smolagents, the CrewAI Python tool and Agno's code tool until it does.
- **Kubernetes-native operations:** OpenSandbox (CRDs, pools) and kubernetes-sigs/agent-sandbox
  (with a managed GKE version) fit clusters better. kindling's operator is experimental (README).
- **OpenSandbox ahead on its own turf:** isolated sessions, a credential vault in the API,
  diagnostics endpoints and more SDK languages (`docs/benchmarks.md`, caveat 9).
- **Community and ecosystem:** OpenSandbox has about 11.5k stars, CubeSandbox about 9.9k (one
  review), and E2B is a built-in provider in the OpenAI Agents SDK, Mastra, CrewAI, Agno and
  smolagents. kindling has about 53 stars and no SDK on PyPI or npm yet.
- **Compliance and support:** Northflank, Runloop and E2B Enterprise sell BYOC or VPC deployments
  with SLAs. kindling is a self-hosted project with no commercial support.
- **Computer use / desktop sandboxes:** E2B Desktop and Daytona are what Mastra's computer-use
  launch covered ([Mastra](https://mastra.ai/blog/introducing-sandbox-computer-use)). kindling has
  nothing equivalent (`kling phone` covers Android only).
- **Windows hosts:** smolvm runs on Windows through the Windows Hypervisor Platform (per the
  pydantic-ai-harness source, `_smolvm_cli.py`). kindling supports Linux and macOS only.
