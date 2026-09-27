<p align="center">
  <img src="docs/kindling-banner.svg" alt="kindling — small dry twigs that catch fire first" width="760">
</p>

<p align="center">
  <a href="https://github.com/juan52878911/kindling/releases"><img src="https://img.shields.io/github/v/release/juan52878911/kindling?label=release&color=e25822" alt="latest release"></a>
  <a href="https://github.com/juan52878911/kindling/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/juan52878911/kindling/ci.yml?label=ci" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-4c8dae" alt="Apache-2.0"></a>
  <img src="https://img.shields.io/badge/platforms-linux%20amd64%20%7C%20arm64%20·%20macOS%20arm64-4c8dae" alt="platforms">
  <img src="https://img.shields.io/badge/isolation-Firecracker%20%7C%20vz%20microVMs-6aa84f" alt="Firecracker">
</p>

<p align="center"><b>English</b> · <a href="README.es.md">Español</a></p>

# kindling

**Firecracker microVMs that wake from a file in ~30 ms and cost 0 RAM while they sleep —
for MCP servers, agent sandboxes and small models, on your own Linux or Apple Silicon
hardware.**

One static binary, `kling`, with a docker-style CLI. Boot a machine once, freeze it with
its server already listening, and let the gateway wake it per call: kernel-level isolation
for code you do not trust, at the idle cost of a file on disk.

<p align="center">
  <img src="docs/img/hero.gif" alt="Frozen microVMs wake in milliseconds when a request arrives; Chispa decides in microseconds" width="360">
</p>

## Try it in 30 seconds

On a Linux host with KVM (or a Mac with Apple Silicon — see [Install](#install)):

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
kling up                       # checks KVM, nftables, the kindling user; prints the privileged commands, never runs them
kling try -- uname -a          # a throwaway microVM: create, run, return the exit code, delete
```

What that looks like against a real daemon (recorded, not typed by hand):

<p align="center">
  <img src="docs/img/demo-try.gif" alt="kling try running uname, python3 and a wget inside throwaway microVMs" width="820">
</p>

Then host your first MCP server and plug your agent into it:

```sh
kling plugin install mcp                              # the MCP extension, from the same release
kling mcp add io.github.domdomegg/filesystem-mcp      # packages it, boots it once, freezes it as a service
kling connect -all -install all                       # Claude Code, opencode, Cursor, VS Code, Windsurf, Cline, Zed
```

## Why kindling

Every number is measured on the project's own lab and documented where it was taken.
Nothing here is estimated.

| | Measured | Where |
|---|---|---|
| Wake a frozen machine (thaw) | **~30 ms**, server already listening | [handbook: measured numbers](docs/handbook.md#measured-numbers), `scripts/40-bench-boot.sh` |
| Wake a paused machine | **~2.2 ms** to first answer, seen by the client | [`docs/despertar.md`](docs/despertar.md) |
| A frozen machine while it sleeps | **0 CPU, 0 RAM** — a sparse file of 35–82 MB | [handbook: disk cost](docs/handbook.md#disk-cost) |
| Density from one golden snapshot | **142 microVMs in 3.9 GB** of host RAM (1 vCPU, 64 MiB, Alpine `min` guests); 10 copies = **+68 MiB** total | [`docs/estabilidad.md`](docs/estabilidad.md), [handbook: density](docs/handbook.md#density-why-the-golden-snapshot-changes-everything) |
| Ephemeral MCP action, end to end | **19 ms** (2 ms of execution); hot tool call **9 ms** | [`ext/mcp/README.md`](ext/mcp/README.md#ephemeral-mode-one-microvm-per-action) |
| Context an agent pays for your tools | **≈248 tokens** for 3 services through lazy meta-tools vs **≈4327** with 28 schemas loaded | [`ext/mcp/README.md`](ext/mcp/README.md#a-single-entry-point-for-every-service) |
| Sandbox from a prewarmed pool | **16 ms** to claim vs 683 ms to create from the snapshot | [`ext/sandbox/CHANGELOG.md`](ext/sandbox/CHANGELOG.md) |
| A small LLM (Qwen2.5-1.5B) to first token | **0.41 s** from its golden snapshot vs **9.1 s** cold, on an i7-8700T | [`docs/von.md`](docs/von.md#x86-sin-anidar-i7-8700t) |
| Tiny classifier (Chispa) per prediction | **1.5–6 µs**, no daemon, calibrated `confident`/`escalate` | [`docs/chispa.md`](docs/chispa.md) |
| Disk for a 7-service MCP fleet, layered images | **1300 MiB → 433 MiB** | [`docs/three-layers.md`](docs/three-layers.md) |
| Copy-on-write memory on a Mac (nested lab) | **~40× less RAM** than native processes for restored replicas | [`docs/mac-arm64.md`](docs/mac-arm64.md) |

The idea that holds everything up: a cold Firecracker boot with a real rootfs is seconds,
not the 125 ms of the brochure. So **every tool boots once, is frozen with its server
serving, and restores on demand**. The snapshot is not an optimization; it is the
architecture.

## What people use it for

| | You want to… | Start here |
|---|---|---|
| **Host MCP servers** | run any open source MCP server (npm or PyPI, stdio or HTTP) in its own microVM and connect Claude Code, opencode or Cursor with one command; secrets injected live, never into a snapshot | [guide](docs/guides/mcp-servers.md) · [`ext/mcp`](ext/mcp/README.md) |
| **Sandbox an AI agent** | let an agent run code it just wrote, with no network unless asked, streaming exec, file copy, templates that restore in ~300 ms and N sandboxes in parallel | [guide](docs/guides/sandboxes-for-agents.md) · [`docs/exec-sandbox.md`](docs/exec-sandbox.md) |
| **Serve small models serverless** | a Chispa classifier in microseconds, a local LLM behind an OpenAI-compatible API that wakes per request and freezes when idle, with a cascade that is only enabled when an evaluation proves it helps | [guide](docs/guides/serverless-ai.md) · [`docs/ai-gateway.md`](docs/ai-gateway.md) |
| **Run all of it from a laptop** | keep the daemon on a Linux box with KVM and drive it over SSH, or run natively on Apple Silicon with the `vz` backend | [remote daemon](docs/guides/remote-daemon-ssh.md) · [macOS](docs/guides/quickstart-macos.md) |
| **Ask for sandboxes with kubectl** | a thin operator: `kind: Sandbox` objects, microVMs stay outside the cluster | [guide](docs/guides/kubernetes-operator.md) |

## Install

**Pre-built binaries (recommended).** linux/amd64, linux/arm64, darwin/amd64 and
darwin/arm64; every release ships a `SHA256SUMS` and the installer verifies it before
moving anything onto disk. Windows is not supported.

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
curl -fsSL .../install.sh | sh -s -- --with mcp,sandbox    # extensions from the same release
curl -fsSL .../install.sh | sh -s -- --tag v0.14.0         # a specific version
curl -fsSL .../install.sh | sh -s -- --prefix ~/.local --no-rc   # do not touch your shell rc
```

**From Claude Code.** The repository is its own plugin marketplace; `/kindling:setup`
asks where microVMs should run and which pieces you want, runs the installer above and
ends with `kling doctor`. From a terminal, `install.sh --claude` does the same.

```
/plugin marketplace add juan52878911/kindling
/plugin install kindling@kindling
/kindling:setup
```

The plugin also ships the `kindling-dev` agent, which Claude calls to check risk before
editing (with [chrono](https://github.com/juan52878911/chrono)), run code in throwaway
sandboxes, use hosted MCP servers and train Chispa classifiers
([`plugins/claude-code/README.md`](plugins/claude-code/README.md)).

**From source.** `make install` puts `kling` in the first writable directory on your
PATH without sudo; `make deploy HOST=ssh://user@host` builds the daemon for the host with
KVM, copies the binary and the systemd unit and starts it ([`docs/releases.md`](docs/releases.md)).

**Kubernetes.** `kindling-operator` ships as `ghcr.io/juan52878911/kindling-operator:<version>`
with its manifests in every release ([guide](docs/guides/kubernetes-operator.md)).

After any of them: `kling doctor` prints a ✓/✗ line per piece, with the fix next to
each ✗.

## How it works

<p align="center">
  <img src="docs/img/architecture.svg" alt="Agents talk to a gateway; the gateway to the daemon; the daemon to Firecracker or vz microVMs restored from golden snapshots" width="900">
</p>

- **The daemon** owns the machines and never listens on a network port: a Unix socket
  locally, SSH remotely (`kling context add lab ssh://user@host`). Controlling microVMs is
  root on their host; kindling does not put that on TCP.
- **Gateways** sit in front for the two things agents call: `kling mcp serve` routes MCP
  tool calls (sessions, replicas, ephemeral mode, a single `_all` entry point) and
  `kling ai serve` classifies with Chispa and generates with small models.
- **Three nouns**: an *image* (rootfs) boots cold once; `kling save` turns that machine
  into a *template* (golden snapshot); `kling run -from` makes *machines* from it in
  milliseconds that share its memory. A machine is `running`, `paused` (RAM kept, ~2 ms
  back) or `frozen` (a file, ~30 ms back).

<p align="center">
  <img src="docs/img/lifecycle.svg" alt="image → template → machine; running, paused and frozen states with their costs" width="900">
</p>

- **Isolation** is a hypervisor boundary: each microVM has its own kernel, its own
  network namespace, egress `none` by default (`internet` never reaches private ranges,
  `allowlist` is fail-closed), an unprivileged VMM with zero capabilities, and secrets
  delivered through MMDS to the live machine only — a machine that received secrets can
  no longer be frozen.

The long version, with transcripts and every measurement: [`docs/handbook.md`](docs/handbook.md).

## Compare

*September 2026. Third-party figures are from their public docs and blogs; kindling's from
this repository. The full table with sources: [`docs/compare.md`](docs/compare.md).*

kindling touches four categories at once, and each has a leader that does that one thing
with more polish: hosted agent sandboxes (E2B, Daytona, Modal, Vercel, Cloudflare, Blaxel,
Morph), self-hosted microVM runtimes (microsandbox, Arrakis, Kata, Apple `container`,
Docker Sandboxes), MCP gateways (Docker MCP Gateway, ToolHive, Smithery) and scale-to-zero
model serving with routing (llama-swap, vLLM semantic-router).

| Choose **kindling** when… | Choose something else when… |
|---|---|
| your tools almost never run and you want them to cost nothing while they wait — `frozen` is 0 RAM and comes back in ~30 ms with state intact | you want a hosted sandbox today and do not want to operate anything: E2B, Vercel Sandbox, Cloudflare |
| the code inside is untrusted and a shared kernel is not enough: Firecracker/KVM on Linux, `vz` on Apple Silicon | you need to fork a *live* sandbox into N branches: Morph (Infinibranch) |
| you host MCP servers for Claude Code, opencode or Cursor and want them isolated, connected with one command, secrets never in a snapshot | you want MCP with enterprise RBAC, registry and support: ToolHive, Docker MCP Gateway |
| one static binary and the same freeze-and-wake model for tools, sandboxes and small models, on your own hardware | you need multi-node Kubernetes with a real scheduler and HA: Kata Containers |
| a homelab or edge box with KVM, many tools, data that stays home | you just want local containers with OCI images and no snapshots: Docker Sandboxes, Apple `container`, microsandbox |

Three honest differentiators: (1) microVMs that wake from a file in ~30 ms and cost 0 RAM
asleep — 142 machines (1 vCPU, 64 MiB Alpine guests) in 3.9 GB, on your own hardware;
(2) any MCP server, stdio or HTTP, isolated in its microVM and connected to your agent
with one command, secrets never touching the snapshot; (3) one static binary, no
dependencies, the same model for tools, sandboxes and small models, on Linux/KVM or
native Apple Silicon.

What it is not: not a SaaS, not benchmarked against E2B or Daytona under equal conditions,
not audited, not multi-host beyond a best-effort spread across daemons, and macOS is a
development environment (restores cost ~350 MiB per VM, no image builds, no CPU caps).

## Honest limits

- **The daemon needs KVM on Linux**, or Apple Silicon with macOS 14+ for the `vz`
  backend. No Windows, no Intel Macs, no VPS without nested virtualization.
- **macOS is not Linux.** `vz` restores copy ~350 MiB per VM (no shared golden memory),
  cannot build images (copy them from a Linux arm64 daemon), applies no CPU ceiling and has
  no jailer. Good for development; Linux for density ([`docs/mac.md`](docs/mac.md)).
- **Nested virtualization is slower.** Cold boots measured at 2.6 s nested on Proxmox and
  ~16 s on a Lima VM on Apple Silicon (with levers down to ~2.5 s). Thaw stays in
  milliseconds either way.
- **Security is a threat model, not a certificate.** Jailer only when installed, no
  per-operation authorization on the daemon socket, soft disk quotas, no encryption at
  rest by kindling. All of it listed in [SECURITY.md](SECURITY.md).
- **One host per daemon.** Spreading across hosts is a best-effort choice by free room in
  `ext/sandbox`; the Kubernetes operator is a single replica without leader election.
- **A volume has one writer** (ext4 physics); state shared across services goes through a
  linked memory service.
- **Young project, one maintainer.** Measured, documented, and moving fast; not battle
  tested by anyone but its author yet.

## Guides

| Guide | Read it if… |
|---|---|
| [Quickstart on Linux](docs/guides/quickstart-linux.md) | you have a Linux box with KVM and 15 minutes |
| [Quickstart on macOS (vz)](docs/guides/quickstart-macos.md) | you have an Apple Silicon Mac and want microVMs on it |
| [Remote daemon over SSH](docs/guides/remote-daemon-ssh.md) | the daemon lives on a server and you work from a laptop |
| [Host MCP servers for Claude Code / opencode / Cursor](docs/guides/mcp-servers.md) | you want your agent's tools isolated and on demand |
| [Safe sandboxes for AI agents](docs/guides/sandboxes-for-agents.md) | an agent has to run code without touching your machine |
| [Serverless AI: Chispa + a local LLM behind `kling ai`](docs/guides/serverless-ai.md) | you want classification in microseconds and generation that scales to zero |
| [Kubernetes operator](docs/guides/kubernetes-operator.md) | you want `kubectl apply` to hand out sandboxes |
| [Writing an extension](docs/guides/writing-an-extension.md) | you want `kling yourthing` |
| [Troubleshooting with `kling doctor`](docs/guides/troubleshooting.md) | something is red |

The index of everything under `docs/`, with a "which guide do I need?" table:
[`docs/README.md`](docs/README.md).

## FAQ

**Is this a container runtime?** No. A container is a namespace of a shared kernel; a
microVM has its own kernel behind a hypervisor. kindling costs more than a container per
machine and exists because the code inside is assumed hostile.

**Why not one microVM per request?** Because a cold boot with a real rootfs is seconds.
kindling boots once, freezes with the server serving and restores in ~30 ms. Ephemeral
mode (one microVM per action) exists and costs 19 ms end to end thanks to a prewarmed pool.

**Does a frozen machine really cost nothing?** It costs disk: 35 MB (`min` image) to
~82 MB (Ubuntu) per machine, sparse. No process exists while it sleeps.

**What survives?** A volume survives everything (journaled ext4 on the host). A persistent
service survives its own freeze/thaw but not the deletion of its instance. An ephemeral
action keeps nothing. Details in the [handbook](docs/handbook.md#what-persists-and-what-does-not).

**Can I mount a host folder?** Yes: `-share SRC:DST[:copy|ro|rw]` — a read-only copy by
default, or live through a FUSE agent served with `os.Root` so nothing escapes the folder
([`docs/compartir.md`](docs/compartir.md)).

**Does it run on my Mac?** Natively on Apple Silicon with macOS 14+ (`vz` backend), or
against a Linux daemon over SSH. Intel Macs and Windows: no.

**Which MCP servers work?** Any npm or PyPI server that speaks stdio (`kling-bridge` turns
it into HTTP inside the VM), any server that speaks native Streamable HTTP, and external
servers you `kling mcp link`. `kling mcp search` tells you what it can package unattended.

**Where do secrets go?** Through MMDS into the live machine (`kling machine secret`),
per session. Never into a snapshot: a machine holding secrets refuses to freeze.

**Is it open source?** Yes, Apache-2.0 ([LICENSE](LICENSE)). Contributions are welcome
under the same license, no CLA ([CONTRIBUTING.md](CONTRIBUTING.md)).

## More

- **Documentation**: [`docs/README.md`](docs/README.md) (index), [`docs/handbook.md`](docs/handbook.md)
  (the long version), [`docs/api.md`](docs/api.md) (daemon API), [CHANGELOG](CHANGELOG.md).
- **Resources**: benchmarks, examples, releases and community in [`docs/resources.md`](docs/resources.md).
- **Examples**: [`examples/ci-triage`](examples/ci-triage) (Chispa + VON on CI logs),
  [`examples/domotica`](examples/domotica) (a smart-home room on serverless models; a
  standalone program, not a `kling` subcommand), [`examples/hello-extension`](examples/hello-extension).
- **Security**: threat model, barriers and what is not solved, in [SECURITY.md](SECURITY.md).
- **Contributing**: [CONTRIBUTING.md](CONTRIBUTING.md). Bugs go with `kling doctor`
  output: [open an issue](https://github.com/juan52878911/kindling/issues/new/choose).
- **License**: Apache-2.0 — see [LICENSE](LICENSE) and [NOTICE](NOTICE).
