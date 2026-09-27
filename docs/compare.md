# kindling compared with the alternatives (September 2026)

kindling v0.14.0 next to the closest hosted and self-hosted options. kindling's figures come
from this repository ([README](../README.md), [`handbook.md`](handbook.md),
[`exec-sandbox.md`](exec-sandbox.md), [`mac.md`](mac.md), [`ext/sandbox`](../ext/sandbox));
third-party figures come from their public documentation and blogs, linked at the bottom.
"?" means not found. Stars and licenses as reported by the GitHub API on 2026-09-27. This
page will age; check the date before quoting it.

## Where kindling sits

kindling touches four categories at once. Nobody else covers the four, and each one has a
leader that does *that* thing with more polish:

| Category | Who leads | kindling |
|---|---|---|
| Hosted sandboxes for agents | E2B, Daytona, Modal, Vercel Sandbox, Cloudflare, Blaxel, Morph, Together/CodeSandbox, Docker Cloud Sandboxes, Northflank, Runloop | `ext/sandbox`: yes, but self-hosted, no SaaS |
| Self-hosted microVM runtimes | microsandbox, Arrakis, Flintlock, firecracker-containerd, Kata, libkrun/krunvm, Apple `container`, Docker Sandboxes (local) | the `kling` core + the `vz` backend |
| MCP gateways and hosting | Docker MCP Gateway, ToolHive, Smithery, MetaMCP, mcp-proxy, Microsoft MCP Gateway, Cloudflare Agents | `ext/mcp` + `kling connect` |
| Scale-to-zero model serving and routing | llama-swap, Ollama, KServe/Knative, Modal, RunPod; vLLM semantic-router, RouteLLM, NotDiamond | `kling ai` (Chispa, VON, encoder) |

That is both its thesis (one binary, one operating model: freeze and wake) and its risk
(four fronts, one maintainer).

## The table

| | Start / resume | Idle cost | Isolation | Self-hosted / SaaS | macOS | Snapshot / branching | MCP | License | Price | Maturity |
|---|---|---|---|---|---|---|---|---|---|---|
| **kindling** | thaw ~30 ms; paused → ~2.2 ms; cold 2.6 s (nested); sandbox from template ~300 ms; prewarmed claim 16 ms; Mac restore 121–159 ms | 0 RAM when `frozen` (disk only: 35–82 MB); 142 microVMs in 3.9 GB (1 vCPU, 64 MiB Alpine guests) | Firecracker/KVM; vz on Mac; jailer when installed | Self-hosted; no SaaS | Native (vz, Apple Silicon, v0.9+); ~350 MiB per restore, no image builds, no jailer | Golden snapshot with shared memory (+68 MiB per 10 copies); no branching of a *live* sandbox | Own gateway, stdio→HTTP bridge, `connect` to 7 clients | Apache-2.0 | Free | 1 maintainer, created Aug 2026, 14 releases |
| E2B | resume ~1 s after pause; cold "sub-second" | paused persists; billed while alive | Firecracker (nested L2 on GCP) | both (infra Apache-2.0, GCP-first) | no (SaaS) | pause/resume, no branching | via SDK, no gateway | Apache-2.0 | $0.000014/vCPU·s + $0.0000045/GiB·s; Pro $150/mo | 14k★ SDK; $48.3M raised |
| Daytona | <90 ms cold (snapshot restore); 71 ms create | billed while running; $0.10/h at 2 vCPU | Firecracker/containers by deployment | SaaS; source **closed since Jun 2026** (last AGPL v0.190.0; fork Nightona) | no | image snapshots | no | AGPL-3.0 (frozen) / proprietary | $200 credit; usage | 71.7k★ (repo archived de facto); $24M Series A |
| Morph Cloud | snapshot + branch <250 ms | scale-to-zero with memory preserved | own microVM | SaaS | no | **Infinibranch**: fork a live VM into N | no | proprietary | usage | ? |
| Blaxel | resume <25 ms from standby | "perpetual sandbox" on standby | microVM | SaaS | no | snapshots | no | proprietary | ~$345/GB·month allocated; $200 credit | $7M seed (YC S25) |
| Modal Sandboxes | cold 0.88 s measured; memory snapshots not GA (Feb 2026) | billed per second alive | gVisor | SaaS | no | FS snapshot → new sandbox | no | proprietary | $0.00003942/core·s | large, funded |
| Vercel Sandbox | Firecracker on EC2 metal; GA Jan 2026 | only active CPU | Firecracker + container | SaaS | no | ? | no | proprietary | $0.128/CPU·h active, $0.0212/GB·h | Vercel; $1M bug bounty |
| Cloudflare Sandbox SDK | container in a Durable Object; GA Apr 2026 | stops when asleep | gVisor/container + V8 isolates | SaaS | no | ? | remote MCP on Workers | SDK open (1.1k★) | $5/mo + $0.00002/vCPU·s | Cloudflare |
| Fly Machines | resume "hundreds of ms" (Firecracker snapshot); autostop/autostart | suspended: rootfs only $0.15/GB·month | Firecracker | SaaS | no | suspend/resume, no branching | no | proprietary | usage | Fly.io |
| Docker Sandboxes / Cloud Sandboxes | "fast cold start" (no figure); Cloud since 24 Sep 2026 | local free; cloud from $0.07/h | own microVM, native on Mac/Win/Linux | both; **not open source** | **yes, native** | ? | **MCP gateway built in** + MCP Toolkit | proprietary | local free; cloud hourly | Docker Inc. |
| microsandbox | <100 ms cold (libkrun); ~320 ms measured on metal | pays only while running | libkrun (KVM / HVF) | self-hosted | **yes** (HVF) | "branchable" (README), no figures | no | Apache-2.0 | free | 8.4k★ |
| Arrakis | ? (Cloud Hypervisor) | ? | Cloud Hypervisor | self-hosted | no | snapshot/restore ("backtracking") | own MCP server | AGPL-3.0 | free | 882★; **no commits since Jun 2025** |
| ToolHive | n/a (containers) | n/a | container + permissions | self-hosted + k8s operator (alpha CRD) | yes (Docker) | no | **yes**, registry and gateway | Apache-2.0 | free | 2.2k★; Stacklok |
| Apple `container` | one VM per container; no official figure | pays while running | Virtualization.framework | local | **yes** (macOS 26) | no | no | Apache-2.0 | free | 50.2k★; Apple |
| Kata Containers | ~100–200 ms VMM (CH/FC/QEMU) | n/a | microVM via CRI | self-hosted, k8s | no | no | no | Apache-2.0 | free | 8.8k★; OpenInfra |
| llama-swap | loads the model on demand; TTL unloads | 0 idle VRAM | none (a process) | self-hosted | yes | n/a | n/a | MIT | free | 5.8k★ |
| vLLM semantic-router | ModernBERT classifier in front of vLLM | n/a | n/a | self-hosted | n/a | n/a | n/a | Apache-2.0 | free | 5.9k★; Red Hat |

Also worth watching: **Unikraft Cloud** (unikernels, 10–20 ms cold, SaaS), **Anthropic
sandbox-runtime** (Seatbelt/bubblewrap, no VM: Claude Code's default sandbox), **Nightona**
(AGPL fork of Daytona), **Flintlock** (MPL-2.0, microVMs on containerd, no golden snapshots).
Weave Ignite is archived (Dec 2023).

## Where kindling is better, worse, or even

### Better

- **Zero idle cost with full state.** `frozen` is a sparse 35–82 MB file, 0 RAM, 0 CPU,
  and comes back in ~30 ms with the process already listening. E2B pauses but resumes in
  ~1 s; Fly in "hundreds of ms"; Modal has no memory snapshots in GA; llama-swap unloads a
  model but reloads it from scratch. Only Blaxel (<25 ms) and Morph (<250 ms) are in the
  same range, and both are SaaS.
- **Density from a shared golden snapshot.** 10 copies = +68 MiB; 142 microVMs in 3.9 GB
  (1 vCPU, 64 MiB Alpine guests, not real workloads). No self-hosted runtime in the list
  documents an equivalent; microsandbox says "branchable" without figures.
- **MCP as a first-class citizen.** stdio→HTTP bridge inside the VM, lazy tool discovery
  (≈248 vs ≈4327 tokens), `kling connect` to seven clients, secrets via MMDS that never
  touch the snapshot, egress allowlist. ToolHive and Docker gateway over containers
  (shared kernel); Smithery/Cloudflare require rewriting the server as a remote one.
- **Native Mac without an intermediate VM**, with Firecracker's API: only Docker
  Sandboxes (closed) and microsandbox match it; Apple `container` has no snapshots.
- **Documented honesty**: reproducible numbers with scripts, a SECURITY.md that lists what
  is NOT solved, a CHANGELOG that names incompatibilities.
- **One static Go binary** with no dependencies (except `vz/`, a separate cgo module).

### Worse, no excuses

- **Maturity and community.** One maintainer, a project born in August 2026; releases
  every week is speed, not maturity. No known users beyond the author.
- **No external security audit**, with declared gaps: jailer only when installed, no
  per-operation authorization on the daemon socket, soft disk quotas, no encryption at
  rest, an unauthenticated local bridge. Vercel pays $1M in bug bounties; Docker and Apple
  have security teams.
- **Needs KVM on Linux** (or Apple Silicon with macOS 14+ for vz). No Windows, no Intel
  Mac, no VPS without nested virtualization. The SaaS options hide all of that.
- **Multi-host is a spread, not a scheduler.** `ext/sandbox` picks a host by free room and
  retries elsewhere; the Kubernetes operator is a single replica without a lease, no Pod per
  sandbox, no CNI/CSI; templates are tied to their host (a reboot invalidates goldens). No
  migration, no affinity, no HA.
- **Mac density does not travel.** ~350 MiB per restore, 10 replicas = 3.1 GiB, no image
  builds, no CPU caps, no jailer. A development environment, not a runtime.
- **No SDK.** Everything is CLI plus a documented HTTP API; there is no
  `pip install kindling` with `Sandbox.create().run(...)`, which E2B, Daytona, Modal,
  Vercel, Cloudflare and microsandbox all offer.
- **Most of `docs/` is in Spanish**, with a bilingual README and English guides.
- **Concurrency per host**: 20 concurrent calls to one service p50 4.66 s (down from 44 s).
  An elastic SaaS has no such per-host ceiling.

### Even

- Cold boot (2.6 s nested; 266–317 ms on vz): comparable to CodeSandbox (~2 s), worse than
  Daytona or microsandbox, and irrelevant by design thanks to the snapshot.
- Egress allowlist, volumes, shared folders: every serious SaaS has them.
- The Kubernetes operator is on a par with ToolHive's (alpha CRD, not for production).
- The routing cascade with statistical gates: comparable in idea to semantic-router and
  RouteLLM; fewer models and public evaluations, but zero idle cost with isolation.

## Who should pick what

| You are… | Best option | Why not kindling |
|---|---|---|
| a startup that wants sandboxes today and operates nothing | E2B, Vercel Sandbox, Cloudflare | no SaaS; you need KVM |
| doing best-of-N / tree search over agent states | Morph (Infinibranch) | kindling instantiates N from a golden but does not branch a *live* sandbox |
| running coding agents on a laptop, no snapshots needed | Docker Sandboxes, Apple `container`, microsandbox | free, zero configuration, OCI images directly |
| deploying MCP in an enterprise with policies and audit | ToolHive, Docker MCP Enterprise Gateway, Microsoft MCP Gateway | RBAC, registry, k8s, support |
| publishing public remote MCP servers | Cloudflare Agents + Smithery | global hosting, OAuth, no VM |
| serving local models without idle VRAM | llama-swap, Ollama | simpler, no isolation |
| doing LLM routing with research behind it | vLLM semantic-router, RouteLLM | public benchmarks, community |
| running multi-node k8s with microVMs | Kata Containers | real CRI, real scheduler, HA |
| **a homelab or edge box with KVM, many tools that almost never run, data that stays home** | **kindling** | this is where it wins: 0 idle RAM, ~30 ms thaw, hypervisor isolation, one binary |
| **on an Apple Silicon Mac wanting microVMs with snapshots and MCP built in** | **kindling (vz)**, with reservations | the only open-source option that does it; poor density |

## What kindling should borrow

1. An SDK in Python and TypeScript with three calls (`create / run / snapshot`) on top of the
   sandbox frontal's API, with examples for LangGraph, OpenAI Agents and the Claude Agent SDK.
2. Explicit branching: `kling sandbox fork <ref>` from a live sandbox (Morph) — technically
   pause + snapshot + N restores, which already exists in pieces.
3. OCI images as input (`kling image import docker.io/…`) like microsandbox and Apple
   `container`; today rootfs images are built with scripts.
4. A `docs/benchmarks.md` with hardware, date and script for every figure so a third party
   can repeat it.
5. A bounded audit or symbolic bug bounty of one piece (the bridge, the jailer by default)
   before saying "hostile by default" too loudly.

## Claims this project avoids

- "Faster than E2B/Daytona/Modal": there is no benchmark under equal conditions; their
  figures are on metal, kindling's nested on Proxmox.
- "Secure", "production-ready", "hardened": no audit, jailer optional, no granular daemon
  authorization.
- "Kubernetes-native" or "multi-host": the operator is one replica without a lease and the
  host spread is by free room, without HA or migration.
- "Works on Mac like on Linux": ~350 MiB per restore and no builds, jailer or caps.
- "142 microVMs" without context: 1 vCPU / 64 MiB Alpine guests, not real workloads.
- "Chispa beats LLMs": on one task the 0.5B–3B models did not match Chispa alone; that is
  evidence about one task, not general superiority.

## Sources

E2B: [pricing and nested virtualization](https://bex.co/blog/2026/09/24/e2b-l2-guests-firecracker-nested-virtualization-vs-hetzner-microvms), [alternatives](https://opencomputer.dev/guides/e2b-alternatives/), [funding](https://tracxn.com/d/companies/e2b/__U7C82j6Wk3VH-rgW0n4LFnUqqq-LuBw6rnIcnLGz2yU/funding-and-investors).
Daytona: [review](https://baeseokjae.github.io/posts/daytona-review-2026/), [going closed](https://bex.co/blog/2026/09/09/daytona-closed-source-self-hostable-meaning), [Nightona](https://github.com/nightona-co/nightona), [funding](https://blaxel.ai/blog/daytona-dev-environment-pricing-alternatives).
Morph: [Infinibranch](https://cloud.morph.so/docs/blog/developers). Blaxel: [comparison](https://northflank.com/blog/top-blaxel-alternatives-for-ai-sandbox-and-agent-infrastructure), [funding](https://startupintros.com/orgs/blaxel).
Modal: [pricing](https://www.agenticwire.news/article/e2b-vs-modal-agent-sandbox-cost-comparison), [snapshots](https://modal.com/docs/guide/sandbox-snapshots), [cold start](https://www.superagent.sh/blog/ai-code-sandbox-benchmark-2026).
Vercel: [pricing](https://vercel.com/docs/sandbox/pricing), [GA](https://www.marktechpost.com/2026/08/27/best-agent-sandboxes-2026-cold-start-pricing-network-policy/).
Cloudflare: [GA](https://developers.cloudflare.com/changelog/post/2026-04-13-containers-sandbox-ga/), [pricing](https://developers.cloudflare.com/containers/pricing/), [MCP](https://blog.cloudflare.com/mcp-v2/).
Fly: [suspend/resume](https://fly.io/docs/reference/suspend-resume/), [billing](https://fly.io/docs/about/billing/).
Docker: [Sandboxes](https://www.docker.com/blog/why-microvms-the-architecture-behind-docker-sandboxes/), [Cloud](https://www.docker.com/blog/introducing-cloud-sandboxes-start-on-your-laptop-finish-in-the-cloud/), [pricing and license](https://aicybr.com/blog/docker-cloud-sandboxes-kits-agent-pricing), [MCP gateway](https://docs.docker.com/ai/sandboxes/mcp-gateway/).
microsandbox: [repo](https://github.com/superradcompany/microsandbox), [libkrun](https://emirb.github.io/blog/microvm-2026/). Arrakis: [repo](https://github.com/abshkbh/arrakis).
ToolHive: [docs](https://docs.stacklok.com/toolhive/guides-k8s/intro), [v0.46](https://aicybr.com/blog/toolhive-mcp-server-security-deployment-guide). Apple: [container](https://github.com/apple/container).
Kata: [guide](https://northflank.com/blog/kata-containers-vs-firecracker-vs-gvisor). Ignite: [archived repo](https://github.com/weaveworks/ignite). Flintlock: [repo](https://github.com/liquidmetal-dev/flintlock). Unikraft: [10–20 ms](https://unikraft.cloud/blog/death-to-cold-starts/).
llama-swap: [repo](https://github.com/mostlygeek/llama-swap). semantic-router: [Red Hat](https://developers.redhat.com/articles/2026/03/25/getting-started-vllm-semantic-router-athena-release). RouteLLM: [survey](https://arxiv.org/html/2603.04445v2). Anthropic sandbox-runtime: [gist](https://gist.github.com/wincent/2752d8d97727577050c043e4ff9e386e).
