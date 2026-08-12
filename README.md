<p align="center">
  <img src="docs/kindling-banner.svg" alt="kindling — small dry twigs that catch fire first" width="760">
</p>

# kindling

**English** · [Español](README.es.md)

Serverless MCP tooling on Firecracker microVMs. The goal: take any open source MCP
server and automatically turn it into a service that spins up on demand, in
milliseconds, with kernel-level isolation.

The name is the idea: *kindling* is the small dry wood that catches fire first and
ignites the larger fire. Here, every tool is kindling — tiny, cold, costing nothing
while it waits, and ablaze in milliseconds the moment it's needed.

> Status: **complete** — the full circuit works. `kling` manages microVMs with a
> docker-like interface: networking, golden snapshots, isolation, events, an MCP
> gateway with sessions and a **stdio→HTTP bridge**. Any open source MCP server can be
> hosted on demand, whether it speaks stdio or native Streamable HTTP.

**The guest is considered hostile**: you don't know which MCP server you'll be
hosting. See [SECURITY.md](SECURITY.md) for the threat model, the barriers in place
and — above all — what is NOT solved yet.

## The idea

An AI agent wants tools. Most open source MCP servers are Node or Python processes
designed to be spawned by the client and kept alive for the whole conversation. That
model breaks down the moment you want *many* tools, from *untrusted* sources, on a
*shared* machine:

- Twenty always-on tool processes burn RAM and CPU whether they're used or not.
- Arbitrary open source tooling driven by an AI is untrusted code, and a container's
  namespace is a thin wall to put around it.
- Loading two hundred tool schemas into the model's context before it starts working
  is its own kind of waste.

kindling's answer: run every MCP server inside its own Firecracker microVM, boot it
**once**, freeze it with the server already listening, and let a gateway restore it on
demand. A frozen tool is just a file on disk — zero CPU, zero RAM — and waking it up
takes ~30 ms, imperceptible inside a tool call.

## Advantages at a glance

| | |
|---|---|
| **Hypervisor isolation** | Each tool runs behind a KVM boundary, not a shared-kernel namespace |
| **Zero idle cost** | Frozen machines are files: no CPU, no RAM, ~35–82 MB of disk |
| **Millisecond wake-up** | ~30 ms restore; 9 ms warm calls end to end |
| **12× density** | Golden snapshots share memory pages: 6.8 MiB per instance instead of 82 MiB |
| **Any MCP server** | stdio servers get a bridge; native Streamable HTTP servers run as-is |
| **Small model context** | One aggregator endpoint with 3 meta-tools instead of N×M schemas |
| **Nothing exposed** | The daemon never listens on the network; remote control is SSH-only |

## kling

The CLI. If you've used docker, you already know it:

```
$ kling info
endpoint:     ssh://juan@192.168.2.60
daemon:       0.1.0
root:         /var/lib/kindling
KVM:          yes
firecracker:  Firecracker v1.16.1
machines:     7

$ kling run -name mcp-demo
efad9e5f7003  mcp-demo  cold-booted in 54 ms

$ kling freeze mcp-demo
efad9e5f7003  warm  (754 ms, 256 MiB on disk)

$ kling ps
ID             NAME       IMAGE     STATE    CPU/MEM    AGE    LAST OP
efad9e5f7003   mcp-demo   default   warm     1/256MiB   17s    freeze 754ms, 256MiB

$ kling thaw mcp-demo
efad9e5f7003  running  (22 ms)
```

The **`warm`** state is what separates kindling from a container runtime: the machine
is frozen on disk, consumes no CPU or RAM, and wakes in tens of milliseconds.

### Golden snapshots

Freeze a machine once and instantiate N copies that **share its memory**:

```
$ kling commit template golden
golden  golden snapshot  (80M of memory)

$ kling run -from golden -name g1
a3f9...  g1  instantiated from golden in 34 ms

$ kling snapshots
NAME     IMAGE     CPU/MEM    MEMORY   DISK   INSTANCES   AGE
golden   default   1/256MiB   80M      80M    10          21s

$ kling events
23:52:20  machine.frozen   mcp-demo  frozen in 754 ms (256 MiB on disk)
23:52:20  machine.thawed   mcp-demo  thawed in 22 ms
```

### Connecting

The same binary is both CLI and daemon. `kling daemon` runs wherever KVM is; the CLI
talks to it over a Unix socket, locally or through SSH:

```sh
export KLING_HOST=ssh://juan@192.168.2.60   # remote daemon
export KLING_HOST=/run/kling.sock           # local daemon
```

**The daemon never listens on a network port.** Controlling microVMs is equivalent to
root on their host: it can mount disks and boot arbitrary kernels. Exposing that over
TCP would repeat the mistake that has cost Docker a decade of compromised servers. For
remote use, SSH is used with the same technique as `docker context`: instead of
requiring socat or nc on the target, the CLI invokes `kling dial-stdio`, which bridges
the SSH pipe to the local socket.

### Installation

**Fast path — pre-built binaries (recommended):**

```sh
# macOS / Linux — one line, no dependencies
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh

# To include kling-bridge (the daemon needs it when rebuilding images):
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh -s -- --bridge

# A specific version (defaults to the latest release):
curl -fsSL .../install.sh | sh -s -- --tag v0.1.0

# Custom prefix:
curl -fsSL .../install.sh | sh -s -- --prefix ~/.local --bridge
```

Binaries are published on [Releases](https://github.com/juan52878911/kindling/releases)
for **linux/amd64**, **linux/arm64**, **darwin/amd64** and **darwin/arm64**. Every
release ships `SHA256SUMS`, and the installer verifies the checksum before moving
anything onto disk. **Windows is not supported** — the code uses POSIX syscalls
(`syscall.Kill`, `Setsid`, `Stat_t`).

**From source — `make`:**

```sh
make install                              # CLI on your machine
make deploy HOST=ssh://juan@192.168.2.60  # daemon on the KVM host
```

`make install` picks the first writable directory in your PATH and **never asks for
sudo**: installing a user tool shouldn't require it. Force a system location with
`make install PREFIX=/usr/local` if you prefer.

`make deploy` cross-compiles for `linux/amd64`, copies the binary and a systemd unit,
and starts the service. The unit hands the socket to the user you SSH in as, so the
client never needs to run under sudo.

### Configuration

Named contexts, in the style of `docker context`, so you don't drag `KLING_HOST`
around:

```sh
kling context add lab ssh://juan@192.168.2.60 -description "Home Proxmox"
kling context use lab
kling context ls
```

And defaults, so you don't repeat flags on every `run`:

```sh
kling config set defaults.image min
kling config set defaults.ttl_seconds 600
kling config set gateway.idle 5m
kling config show
```

The file lives at `~/.config/kling/config.json` — on macOS too: `UserConfigDir()`
would put it under `~/Library/Application Support`, which is right for desktop apps
but surprising for a CLI.

**Precedence:** `-H` > `$KLING_HOST` > active context > local socket. The flag always
wins, so a one-off invocation never forces a context switch.

## Why microVMs and not containers

An MCP server is a 50–100 MB Node or Python process. Putting it in a microVM does not
save resources compared to a container — it costs more, because every microVM boots
its own kernel.

The reason is a different one: **a local AI executing arbitrary open source tooling is
untrusted code**. A container's isolation is a namespace in a shared kernel; a
microVM's is a hypervisor boundary. That is the only honest justification for this
project, and it's worth being clear about it before writing another line.

## Measured numbers

Measured on Proxmox (Intel i7-8700T) with Firecracker v1.16.1 **nested** inside a VM,
kernel 6.1.177 and an 800 MB Ubuntu 24.04 rootfs:

| Operation | Time |
|---|---|
| Cold boot | **2,643 ms** |
| Snapshot creation | 305 ms |
| **Restore from snapshot** | **~30 ms** |

Reproduce them with `scripts/40-bench-boot.sh`.

**The conclusion that defines the architecture:** 2.6 s cold makes a
microVM-per-request model unviable. Firecracker's advertised 125 ms is with a trimmed
kernel and minimal rootfs on bare metal. With snapshot/restore, 30 ms is imperceptible
inside a tool call.

Therefore: **each tool boots once, gets frozen with the MCP server already listening,
and the gateway restores it on demand.** The snapshot is not an optional optimization;
it's what holds everything else up.

## Disk cost

The base image is **never copied**: it's mounted read-only and shared by every
microVM. Each machine only owns a sparse overlay of its own, mounted with overlayfs by
`/sbin/overlay-init` inside the guest.

| | |
|---|---|
| Base image `min` (Alpine), shared | **17 MB**, once |
| Base image `default` (Ubuntu), shared | 386 MB |
| Per running machine | **~8 MB** |
| Per `warm` machine, `min` image | **~35 MB** |
| Per `warm` machine, `default` image | ~82 MB |

Before overlays, each machine copied the full 800 MB: three machines cost 2.4 GB, now
they cost 386 MB + 25 MB.

**A `warm` machine consumes no RAM.** `freeze` kills the Firecracker process; what
remains is a file. Its cost is disk, not memory.

Firecracker dumps the entire memory when freezing, but most of it is zero pages.
kindling punches holes through them with `fallocate --dig-holes`: the kernel returns
zeros when reading a hole, which is exactly what was there, so the restore never
notices.

**256 MB → 81 MB, and `thaw` stays at ~30 ms.**

### What drives that cost

Two measurements that orient any future optimization:

| RAM assigned | Frozen cost |
|---|---|
| 512 MiB | 86 MB |
| 256 MiB | 81 MB |
| 96 MiB | 80 MB |

**Assigning more RAM is nearly free** once the file is sparse: what gets stored is the
real working set, not the reserved RAM. Lowering `-mem` is not the lever.

The lever is what boots inside:

| Guest | Frozen cost |
|---|---|
| Ubuntu 24.04 + systemd | 82 MB |
| Alpine without systemd (`min` image) | **35 MB** |

Almost half the cost was Ubuntu userspace that an ephemeral tool never uses.
`scripts/70-build-minimal-image.sh` builds the `min` image: Alpine with
`/sbin/overlay-init` and no service manager, booting straight into `/entrypoint`.

Beyond that lie two unexploited Firecracker techniques: **diff snapshots**, which
store only the pages changed against a base, and the **UFFD backend**, which lets
several microVMs restored from the same snapshot share pages in RAM. UFFD doesn't
reduce disk, but it's what buys density when many tools are hot at once.

## Architecture

```
   Your agent (Claude Code, opencode, a local model…)
        │  MCP / Streamable HTTP
        ▼
  ┌───────────┐   link    ┌──────────────────┐
  │  gateway  │──────────>│ external server  │  outside kindling
  └─────┬─────┘           └──────────────────┘
        │  restores (~30 ms) and proxies to :8080/mcp
        ├────────────────────────┬─────────────────────────┐
        ▼                        ▼                         ▼
  ┌──────────────┐        ┌──────────────┐          ┌──────────────┐
  │ service µVM  │        │ service µVM  │          │ ephemeral µVM│
  │ bridge→stdio │        │ native HTTP  │          │ dies at end  │
  └──────────────┘        └──────────────┘          └──────────────┘
        └────────────────────────┴─────────────────────────┘
                     one network namespace each
                                 ▲
                          ┌───────────┐
                          │  daemon   │  lifecycle, networking, snapshots
                          └───────────┘
```

The **gateway** receives the call, restores the matching snapshot, proxies the
request, and collects the microVM when its TTL expires. Inside the guest it always
calls the same place, `:8080/mcp`, whether the server speaks stdio (with
`kling-bridge` translating) or native Streamable HTTP (nothing in between).

The **daemon** manages the lifecycle and is the only one that can reach the guests:
their IPs only exist in the host's network. That's why it exposes
`POST /machines/{ref}/guest`, which forwards an HTTP request to the server inside.
Without it, `kling mcp import` would only work when running the CLI on the host
itself — over SSH the probe has no route and times out.

## Requirements

- A host with KVM and `cpu: host` (or equivalent) so virtualization extensions pass
  through
- If running nested, nested virtualization enabled on the parent host
- `firecracker` + `jailer`, `e2fsprogs`, `squashfs-tools`, `curl`, `jq`

On **macOS**: Firecracker doesn't run natively. On Apple Silicon M3 or later with
macOS 15+ it can run inside a Linux aarch64 VM with nesting. Good for development, not
as a runtime — it burns more battery than the thing this project is trying to avoid.

## Scripts

| | |
|---|---|
| `scripts/install.sh` | curl-pipe-sh installer: downloads the release binary and verifies SHA256 |
| `scripts/release.sh` | Creates and pushes the tag; triggers the release workflow |
| `scripts/10-provision-lab.sh` | Creates the lab VM on Proxmox |
| `scripts/20-install-firecracker.sh` | Installs Firecracker and jailer from the latest release |
| `scripts/30-fetch-artifacts.sh` | Discovers and downloads kernel + rootfs from CI |
| `scripts/40-bench-boot.sh` | Measures cold boot, snapshot and restore |
| `scripts/50-prepare-image.sh` | Injects `overlay-init` and registers the base image |
| `scripts/80-mcp-image.sh` | Packages an MCP server (stdio + bridge, or native HTTP) into an image |

Release cycle details: [`docs/releases.md`](docs/releases.md).
Per-version changes: [`CHANGELOG.md`](CHANGELOG.md).

## Roadmap

- [x] **Phase 1** — Lab: a microVM that boots, snapshot/restore measured
- [x] **Phase 1.5** — `kling`: lifecycle, states, events, local and SSH transport
- [x] **Phase 1.6** — Overlays, sparse snapshots and golden snapshots with shared memory
- [x] **Phase 2** — TAP networking with one namespace per microVM
- [x] **Phase 2.5** — Hardening: dropped privileges, filtered egress, rate limits
- [x] **Phase 2.6** — Minimal image, TTL, CPU cgroups, reconciliation and watchdog
- [x] **Phase 3** — A real MCP server inside, speaking native Streamable HTTP, no bridge
- [x] **Phase 4** — Gateway: route call → restore → proxy → collect on idle
- [x] **Phase 5** — stdio→HTTP bridge: servers that only speak through pipes, too

### What remains unsolved

The roadmap is complete; the project isn't. What's left, in order of how much it
hurts:

- **Parallel calls to persistent services.** Around 25% exhaust the 60 s timeout. It's
  not the isolation: the bridge on its own dispatches 16 parallel calls in 127 ms, and
  ephemeral services do 8 of 8 in 785 ms. The bug is in the gateway, not yet located.
  Meanwhile, go serial against a persistent service.
- **No durable storage.** A persistent service keeps its state as long as its instance
  lives, no longer. Anything that must survive everything needs a host volume mounted
  inside the microVM, which isn't implemented — hence the recommendation to link a
  memory service.
- **The missing barriers** are listed in [SECURITY.md](SECURITY.md): no chroot, no
  hard disk quota, no encryption at rest, golden snapshots unsigned.

## Field notes

See [docs/hallazgos.md](docs/hallazgos.md) — things that take hours to discover on
your own, like the artifact URLs in every tutorial on the internet returning 404.

## Density: why the golden snapshot changes everything

A golden snapshot is an **image artifact, not a machine artifact**: freeze once, and N
instances restore from the same file. Because Firecracker **maps** it instead of
allocating anonymous memory, the kernel shares those pages across all instances and
each one only pays for what it writes.

Measured by instantiating one at a time and watching system RAM:

| | 10 from golden snapshot | 10 cold-booted |
|---|---|---|
| Total RAM added | **+68 MiB** | +824 MiB |
| Per machine | **6.8 MiB** | 82 MiB |
| Time per machine | ~40 ms | ~2.6 s to userspace |

**12× the density.** The proof that pages are shared is in the gap between two
numbers: the RSS of the ten processes summed to 258 MiB, but system RAM only rose
68 MiB. The 190 MiB difference is shared pages that every process counts as its own.

This is, in practice, what UFFD is chased for — and it falls out of the `File`
backend, without writing a page-fault handler.

## Networking: one namespace per microVM

```
$ kling topo
kindling  ssh://juan@192.168.2.60
          KVM ok · Firecracker v1.16.1

  host  172.30.0.0/16
   ├─◆ golden           golden snapshot · 82M shared memory
   │  ├── g3             running  172.30.0.18       384K  thaw 28ms
   │  ├── g2             running  172.30.0.14       384K  thaw 26ms
   │  └── g1             running  172.30.0.10       384K  thaw 41ms
   │
   └─◆ (cold-booted)
      └── template       running  172.30.0.6          8M  boot 46ms

  4 running · 0 warm · 0 stopped   disk: 9M owned + 83M shared
```

**The problem:** a snapshot records the host's TAP device name. If N instances restore
from the same golden snapshot, all N ask for the same TAP and collide. And you can't
reassign it: Firecracker doesn't allow patching `host_dev_name`.

**The solution:** one network namespace per microVM. Inside each one the TAP is always
called `tap0` and the guest always has the same IP, so **the snapshot works for all of
them**. Differentiation happens entirely on the host, across a veth pair:

```
        host                  │  netns kl-<id>          │  microVM
 vh-<id> 172.30.a.b/30 ◄─veth─► vg-<id> 172.30.a.b+1    │
                              │ tap0    172.16.0.1/30   ├─ eth0 172.16.0.2
```

The guest configures itself from the kernel's `ip=` parameter, needing no network
tooling inside the image. From the host, each machine is reached through its
namespace's IP, which DNATs to the guest.

It's the same approach AWS Lambda uses, and for the same reason.

## Isolation

The guest is third-party code: assume hostile.

| Barrier | How |
|---|---|
| Daemon unreachable over the network | Unix socket only; remote exclusively via SSH |
| Unprivileged VMM | `setpriv` to a service user: **CapEff 0**, `no_new_privs`, only the `kvm` group |
| No access to the LAN | Egress `none` by default; with `internet`, private networks stay blocked |
| No degrading the neighbors | 128 MiB/s disk and 16 MiB/s network per machine; cap of 256 |
| No repeated keys | virtio-rng + `CONFIG_VMGENID`: the guest reseeds on restore |

Verified **from inside the guest**, which is the only measurement that counts:

```
RESULT 192.168.2.100: BLOCKED      (Proxmox host)
RESULT 192.168.2.1:   BLOCKED      (home router)
RESULT 10.10.10.1:    BLOCKED      (WireGuard tunnel)
RESULT 169.254.169.254: BLOCKED    (cloud metadata)
RESULT 1.1.1.1:       REACHABLE
```

## Lifecycle and robustness

```
$ kling run -image min -ttl 300 -cpu 25 -egress internet
$ kling logs <ref> -tail 50        # serial console: the only window inside
```

- **`-ttl`** freezes the machine by itself after that time. Freeze, not kill: it stops
  costing CPU and RAM, but comes back in ~30 ms. It's what makes the model serverless.
- **`-cpu`** bounds CPU usage with its own cgroup (50% of a core by default).
- **Reconciliation on startup**: the daemon compares its saved state against the
  host's reality, re-adopts microVMs that are still alive, and cleans up orphaned
  namespaces and cgroups.
- **Continuous watchdog**: every 10 s it checks that whatever claims `running`
  actually runs. A machine whose process vanished moves to `failed` and frees its
  resources.

**Restarting the daemon does not kill the microVMs.** The unit carries
`KillMode=process`; without it, systemd drags the whole cgroup down and takes the
running machines with it.

### Measured with 8 instances

```
RAM added:          113 MiB   (14 MiB per instance)
connectivity:       9/9
unprivileged VMM:   9/9
active cgroups:     9
```

## MCP Gateway

Routes tool calls and wakes tools on demand. It runs **separate from the daemon**, on
purpose: the daemon never listens on the network because controlling it equals root on
its host. The gateway does listen, but all it knows how to do is wake instances of
snapshots that already exist.

```sh
kling gateway -listen 127.0.0.1:8080 -idle 5m
curl http://127.0.0.1:8080/mcp/echo/       # the tool appears by itself
curl http://127.0.0.1:8080/services        # inventory and what's hot
```

Measured end to end with a real MCP server inside the microVM:

| Path | Latency |
|---|---|
| Cold (instantiate from golden snapshot) | **244 ms** |
| Warm | **9 ms** |
| After freezing on idle | **218 ms** (29 ms thaw + guest networking) |

When the idle timeout expires the tool **freezes, it isn't killed**: it stops costing
CPU and RAM, and the next call brings it back in milliseconds.

### Wrapping an MCP server

```sh
sudo ./scripts/80-mcp-image.sh my-tool ./my-server "nodejs npm"
kling run -name tmpl -image my-tool -service my-tool
kling commit tmpl my-tool && kling stop tmpl
```

The directory needs an executable `entrypoint` listening on port 8080. See
[examples/echo](examples/echo).

## Turning any MCP server into a service

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data

kling mcp import filesystem
```

`mcp import` runs the whole cycle:

```
1/5  booting the template...           ✓ 0aeee853 at 172.30.0.6
2/5  waiting for the MCP server...     ✓
3/5  asking what it can do...          ✓ 14 tool(s)
4/5  freezing as golden snapshot...    ✓
5/5  saving the catalog...             ✓
```

Step 3 is **introspection**: the server is asked once what it can do, and step 5
stores that catalog next to the snapshot.

**`-n` pre-installs the npm packages into the image.** Not a convenience: microVMs
boot with no internet egress, so a runtime `npx -y` would fail to download.

## Inventory never touches the service

Without a persisted catalog, asking "what tools are there?" forces a `tools/list`
against every server, and that **wakes their microVMs**. An inventory question would
end up booting twenty machines.

With `mcp import`, the catalog lives on disk next to the snapshot:

```sh
kling mcp list -v          # every tool, without booting anything
kling mcp refresh <svc>    # recapture after updating the server
```

Verified: listing the full inventory through the aggregator leaves the machine counter
at **0**.

## Ephemeral mode: one microVM per action

```sh
kling gateway -ephemeral -prewarm 3
```

Every call gets **its own microVM**: one is taken from the prewarmed pool, serves the
action, and is destroyed. It is born, acts, and dies.

```
action 1: 19 ms   pid=305 calls_in_this_session=1
action 2: 24 ms   pid=305 calls_in_this_session=1
action 3: 19 ms   pid=305 calls_in_this_session=1
```

`calls_in_this_session=1` **in every one**: no action sees what the previous one did.

### From 350 ms to 19 ms

Profiling an unoptimized ephemeral action:

| Phase | Cost |
|---|---|
| Restore the microVM | 131 ms |
| Wait for networking to return | 53 ms |
| `initialize` (spawns the MCP server) | 61 ms — **with node it's 300–500 ms** |
| `tools/call` | 9 ms |
| Destroy the machine | ~100 ms |

Everything except `tools/call` can be paid up front or afterwards:

- **`-prewarm N`** keeps N instances already restored **with their MCP session open**.
  The call skips restoring, waiting for the network, and initializing.
- **Destruction is asynchronous.** It sat in a `defer`, so the client waited for the
  namespace teardown and file deletion: 100 ms on top of a 2 ms call. The machine dies
  all the same; the client just no longer waits for it to happen.

Result: **2 ms of real execution, 19 ms end to end.**

The trade-off remains: no state between calls. Tools that need it — memory,
step-by-step reasoning — must use the session route (`/mcp/<service>`), which keeps
the process alive.

## Any MCP server, hosted on demand

Most open source MCP servers only speak **stdio**: a persistent child process you talk
to through pipes. There's no port to call, and the client dictates the lifecycle. It's
the opposite of invocable on demand.

`kling-bridge` runs **inside** the microVM, spawns the server as a child, and exposes
its protocol over Streamable HTTP:

```
gateway ──HTTP──> kling-bridge ──stdin/stdout──> MCP server
```

From the outside, a stdio server looks HTTP-native. Wrapping one is a one-liner:

```sh
make bridge
sudo ./scripts/80-mcp-image.sh stdio files -p "nodejs npm" -- \
     npx -y @modelcontextprotocol/server-filesystem /data

kling run -name files-tmpl -image files -service files
kling commit files-tmpl files && kling stop files-tmpl
```

### Servers that already speak HTTP

If the server speaks **native Streamable HTTP** no bridge is needed: it listens on its
own and the gateway talks to it directly. The `http` mode accepts the same options:

```sh
sudo ./scripts/80-mcp-image.sh http everything -p "nodejs npm" \
     -n "@modelcontextprotocol/server-everything" -- mcp-server-everything streamableHttp

kling mcp import everything -image everything
```

Two conditions, and the generated entrypoint pins both:

- **listen on `$PORT` (8080)**, which is where the gateway looks inside the guest
- **serve the protocol at `/mcp`**, which is the path it calls

Tested with `@modelcontextprotocol/server-everything`, the protocol's reference
server. The image carries no `kling-bridge` anywhere:

```
$ kling connect everything
Status:    ✓ mcp-servers/everything v2.0.0 · 12 tool(s): echo, get-sum, …

$ call_tool everything.get-sum {"a":100,"b":23}
The sum of 100 and 23 is 123.
```

### Sessions

MCP identifies conversations with `Mcp-Session-Id`, and a stdio server is
**single-session by nature**: its state lives in the process. Therefore:

- **The bridge spawns one child process per session.** Two concurrent conversations
  don't trample each other's state.
- **The gateway routes stickily.** The same session always returns to the same
  microVM; sending it elsewhere would find a server without that state.

Demonstrated with the `session_info` tool, which reports its pid and its call count:

```
session 1 (3 extra calls):  pid=305 calls_in_this_session=5
session 2 (freshly opened): pid=309 calls_in_this_session=1
```

### The full circuit

```
local model  ──>  gateway  ──>  microVM  ──>  MCP server
  (your Mac)     (Proxmox)    (Firecracker)   (stdio or HTTP)
```

[examples/agent/agent.py](examples/agent/agent.py) closes it: an MCP client plus a
tool-calling loop against ollama.

```
$ python3 examples/agent/agent.py "use echo to say hello"
→ kindling-echo v1.0.0  session b2787e00
→ tools: echo, session_info
→ calling echo({"text": "hello"})
← hello
```

The model knows nothing about microVMs: it asks for a tool and the tool appears. If it
had been idle for a while it was frozen, and waking it costs milliseconds.

| Path | Latency |
|---|---|
| Cold MCP handshake, from the Mac | **310 ms** |
| Tool call, warm | **9 ms** |

## Type repair

Several MCP clients and models mangle JSON types before sending: arrays arrive as
objects with `"0"`, `"1"` keys, numbers as strings, booleans as `"true"`. The server
rejects them with "expected array, received object" and from the outside it looks like
the tool failed, when it never even saw the call.

Since the catalog stores each tool's declared schema, the aggregator undoes the damage
before forwarding. Four broken shapes are recognized where the schema asks for an
array:

| What arrives | Repaired to |
|---|---|
| `{"0":…,"1":…}` indexed object | `[…,…]` |
| `"[{…}]"` string with JSON inside | `[{…}]` |
| `{…}` a bare object | `[{…}]` |
| `{"paths":{"paths":[…]}}` wrapped | `[…]` |

Only what contradicts the schema is converted: a legitimate object is left intact.
Every repair is logged, and if a server rejects the arguments **despite** the repair,
what was sent is logged too — without that, it's impossible to know what shape the
client produced.

## What persists and what doesn't

Worth being explicit, because it isn't obvious:

| | Survives |
|---|---|
| State of an **ephemeral** service | nothing: the microVM dies after each action |
| State of a **persistent** service | freezes and wake-ups of ITS instance |
| | but **not** that instance being removed |
| Base image and golden snapshot | everything: they're files on the host |

A persistent service keeps its contents for as long as its instance lives; it freezes
when idle and comes back intact. But if that instance is removed — manual cleanup,
`kling rm`, reinstalling the service — the state goes with it, because it lives in its
overlay.

**kindling provides session persistence, not durable storage.** Data that must survive
everything needs a host volume mounted inside the microVM, which is not implemented
today.

## Connecting your AI agent

```sh
kling connect                          # step-by-step guide
kling connect echo                     # URL, status and configuration
kling connect echo -install opencode   # writes it for you
kling connect echo -install claude-code
```

`connect` **actually probes the service** — it performs an MCP `initialize` and lists
the tools — before giving you anything. A configuration that looks right but doesn't
respond is worse than none, because the failure surfaces inside the agent, where it's
much harder to diagnose.

```
Service:   echo
Endpoint:  http://192.168.2.60:8080/mcp/echo
Status:    ✓ kindling-echo v1.0.0 · 2 tool(s): echo, session_info
```

With `-install` it backs the file up before touching it (`.kling-backup`) and
preserves the rest of the configuration. For Claude Code it uses `claude mcp add` when
the CLI is available — the official route — and only writes the JSON when it isn't.

`gateway.url` is the address **agents** use to reach the gateway, which need not be
the listen address:

```sh
kling config set gateway.url http://192.168.2.60:8080
```

### Surviving reboots

```sh
sudo install -m644 packaging/kling-gateway.service /etc/systemd/system/
sudo systemctl enable --now kling-gateway
```

The gateway **does not run as root**: it only talks to the daemon through its socket
and proxies requests. All the privileged parts stay in `kling.service`.

## One entry point for every service

An MCP client loads the definitions of **all** tools on connect. With twenty services
of ten tools each, that's two hundred JSON schemas in the model's context before it
starts working.

```sh
kling connect -all -install opencode                    # every service
kling connect -all -only echo,notes -install opencode   # just some
kling connect -all -expand                              # the full catalog
```

The `/mcp/_all` endpoint is an MCP server that routes to the others. It has two modes:

**`proxy`** (default) — exposes **four meta-tools** instead of N:

| | |
|---|---|
| `list_services` | which servers exist and how many tools each one has |
| `find_tools` | keyword search; returns names and descriptions, **no schemas** |
| `describe_tool` | the full schema of a single tool |
| `call_tool` | executes, routing to the right microVM |

The model searches for what it needs, fetches the schema of what it's about to use,
and calls.

**`expand`** — flattens the catalog into `service.tool` names, for clients that work
better with everything loaded.

### Which one is cheaper depends on how many tools you have

`proxy` mode has a **fixed cost** of ~300 tokens; `expand` grows with every tool. With
few tools, proxy is actually **more expensive**. That's why `connect -all` measures it
against your real catalog and tells you:

```
Context cost, with your current catalog:
  proxy    3 definitions  ≈  248 tokens
  expand  28 definitions  ≈ 4327 tokens
  → The proxy mode you're using saves 4079 tokens.
```

The crossover sits around 8 tools. With 28, proxy saves **17×**; with 200, tens of
thousands of tokens in every conversation.

## Official MCP servers, running

Anthropic's official servers, hosted as microVMs:

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data
kling mcp import filesystem
```

```
SERVICE      TOOLS   CATALOG   MEMORY   INSTANCES
everything   13      6m ago    128M     1
thinking      1      3h ago    121M     1
memory        9      3h ago    122M     1
filesystem   14      3h ago    125M     1
notes         2      3h ago     42M     1
echo          2      3h ago     42M     1
engram       11      2h ago      —      external: http://192.168.2.3:9100/mcp

52 tool(s) across 7 service(s) (6 microVM, 1 external).
```

`everything` is the protocol's reference server and speaks **native Streamable HTTP**:
no bridge. The rest speak stdio and are wrapped. From the outside they're
indistinguishable.

Real use, through the aggregator and in single-use microVMs:

```
filesystem.read_text_file  /data/test.txt      ->  "hello from kindling"   (31 ms)
memory.create_entities     kindling/project    ->  entity created          (31 ms)
everything.get-sum         {"a":100,"b":23}    ->  "The sum … is 123."     (native)
```

**31 ms per action**, each in its own machine that dies when done.

### Things to know when packaging a node server

- **`-n` pre-installs the npm package.** MicroVMs boot without internet egress: an
  `npx -y` would fail to download.
- **The `entrypoint` is PID 1 and the kernel gives it no PATH.** Without setting one,
  the binaries npm installs aren't found: `executable file not found in $PATH`.
- **Directories the server expects must exist INSIDE the image.** `server-filesystem`
  wants `/data`; creating it on the host does nothing.
- **If the server speaks HTTP, it must listen where it's told.** The entrypoint sets
  `PORT=8080` and the server must serve the protocol at `/mcp`: that's where the
  gateway looks.

## Ephemeral or persistent: decided automatically

An ephemeral microVM dies with everything it owns, memory **and disk**. So the
question isn't "does the server hold state?" but:

> does anything written by one call have to be seen by a later call?

`kling mcp import` deduces it from the catalog and says so:

```
echo         EPHEMERAL    because it only queries: leaves nothing to preserve
notes        PERSISTENT   because it writes with save_note and reads with session_info
filesystem   PERSISTENT   because it writes with write_file and reads with read_file
memory       PERSISTENT   because read_graph suggests it accumulates context
thinking     PERSISTENT   because sequentialthinking suggests it accumulates context
```

**`filesystem` is persistent too**, even if it doesn't look like it: it writes to the
guest's disk, which is as volatile as its memory.

The reliable signal is structural — the server exposing both writing and reading
tools — with a handful of words matched against the tool's NAME for single-tool
servers. Matching those words against descriptions classified everything as
persistent: "session" or "sequence" show up in passing in any text.

Override with `-stateful` or `-ephemeral`.

### When in doubt, persistent

Erring toward ephemeral produces **silent loss**: the call responds fine and what was
written disappears. Erring toward persistent only costs a frozen instance, which
spends neither CPU nor RAM.

The aggregator flags it in its inventory so the model knows:

```
memory [remembers across calls]: create_entities, add_observations, ...
filesystem: read_text_file, write_file, list_directory, ...
```

## The inventory rides the handshake

Tool names are cheap — 27 of them take ~100 tokens; what's expensive is the argument
schemas. That's why `initialize` returns the **full inventory** in its `instructions`
field:

```
Available tools, grouped by service:

filesystem: read_text_file, write_file, list_directory, ...
memory [remembers across calls]: create_entities, ...

Call them with call_tool and the full service.tool name.
If you don't know their arguments, ask describe_tool first.
```

So the model knows what exists **from the very first moment** and goes straight to
`call_tool`, instead of spending a call on discovery. Three meta-tools remain:
`find_tools`, `describe_tool` and `call_tool`.

### Persistent does not mean always-on

A persistent service keeps its state but **stops consuming when it's done**. Measured
with `-idle 30s`:

```
write /data/note.txt              →  Successfully wrote
read /data/note.txt (later call)  →  "this must survive"

running:     running   41 MiB above baseline
after 35 s:  warm      RAM back to 0   (freeze 661 ms)
on return:   "this must survive"       (thaw 18 ms)
```

Freezing is not shutting down: the instance stops existing as a process — zero CPU,
zero RAM — but its state stays on disk and returns in milliseconds. The cost is the
memory file: 151 MiB for this service while frozen.

## Bring your own memory service

kindling does not implement shared storage, and that's deliberate: mounting a common
filesystem across microVMs was tried and discarded. Firecracker only exposes block
devices, and an ext4 shared between several VMs corrupts; everything else — NFS,
virtio-fs — adds a lot of machinery for something an MCP server already solves.

Instead, you link an **external** MCP server:

```sh
kling mcp link engram http://192.168.2.3:9100/mcp -description "shared memory"
kling mcp unlink engram
```

It doesn't run in a microVM: it keeps living wherever it already was, and kindling
just routes to it. It shows up in the aggregator as one more service, so any tool —
and the model — can store and read there.

### If your server speaks stdio

The same bridge used inside the microVMs works on your machine:

```sh
make bridge-local
./kling-bridge-local -listen 0.0.0.0:9100 -- engram mcp --tools=agent
kling mcp link engram http://<your-ip>:9100/mcp
```

## Topology report

```sh
kling export -o topology.html
```

Self-contained: no CDN, no remote fonts, no requests on open — it describes a
homelab's topology and has no business telling anyone about it. It's generated on
**your** machine, not on the daemon.

It's the same tree as always — the host on the left, its services in a column, the
instances on the right — but navigable: every box with children opens and closes, and
the one you pick gets detailed below.

```
                        ┌ echo ─────────┐
                        │ 2 tools       │  no instances · ~250 ms
                        └───────────────┘
                        ┌ engram ───────┐
                        │ 11 tools      │  doesn't run here
 ┌ host ────────────┐   └───────────────┘
 │ ssh://…2.60    − ├───┌ filesystem ───┐   ┌ fs-66e51d ────┐
 └──────────────────┘   │ 14 tools      ├───┤ 244f32b7      │  172.30.0.54 · thaw 30 ms
                        └───────────────┘   └───────────────┘
```

The border tells the state: green serving, amber asleep, dotted gray ready but with no
instance, blue external. A dotted service isn't broken — it appears the moment someone
calls it, and the gray annotation on the right says what that will cost.

### Four views of the same system

| view | what it shows |
|---|---|
| **Topology** | the host, its services and each one's live instances |
| **Layers** | what a call traverses: gateway → aggregator → microVM → kernel, rootfs, overlay, bridge → MCP server |
| **MCP** | the full catalog: every service with its tools, marking which ones write |
| **Network** | who can reach the internet and who is isolated, namespace by namespace |

### Drilling down

Clicking a box opens it. To go all the way down, the panel offers **Drill down**: that
node becomes the root and a breadcrumb appears to come back.

```
catalog › filesystem
```

The bottom panel follows your selection: the node's data, the step-by-step flow of a
call to that service, and — if it writes anything — where what's written ends up.

Nodes group by the `service` label, and failing that by their snapshot of origin: two
machines from the same snapshot share memory and belong together even if nobody
labeled them.

```sh
kling run -from echo -service echo -label tier=prod
```

> **Watch what it writes.** It writes with `create_directory`, `edit_file`,
> `move_file`, `write_file`. It lives as long as the instance does → store it in
> **engram**.

That's the rule that causes the most confusion: a microVM's overlay dies with it. If a
tool needs to persist a file, a database row, or anything else, **the right
destination is the linked memory service**, not the guest's disk. The Layers view
marks it on the `own overlay` node itself.

## Usage memory (optional)

Off by default: kindling writes to nobody's memory unless asked to. The bridge binary
is always installed, though, so enabling it is a command, not a project.

```sh
kling memory status            # whether it's on and over what
kling memory install-service   # leaves the local bridge as a permanent service (macOS)
kling memory enable            # uses engram; -service <svc> for another
kling memory disable
```

When enabled, the gateway records in the memory service which tool resolved each
request, and uses that history to rank subsequent searches better:

```
search "read a text file"   →  filesystem.read_text_file
use the tool                →  "hello from kindling"
left in engram:  kindling: the request "read a text file"
                 was resolved by the tool filesystem.read_text_file
```

It stores nothing of its own: it leans on whichever MCP service you linked, and looks
in its catalog for a writing tool instead of assuming anyone's specific API.

### Bilingual search

The model asks in the user's language and tools are described in English. Searching
"leer un fichero de texto" against *"Read the complete contents of a file"* matched
not a single term, so `find_tools` returned junk. A domain synonym table —
leer/read, fichero/file, carpeta/directory… — fixes it without embedding a search
engine.

## Memory: what's real and what's cache

After many freeze/thaw cycles, the hypervisor may show the lab VM at 80% memory.
Almost all of it is **disk cache**, not real usage:

```
Cached:      2.9 GiB    ← what the panel shows
AnonPages:   273 MiB    ← process memory, the real thing
```

Proven by dropping it: cache falls from 3,098 to 178 MiB, usage settles at ~600 MiB,
and the microVMs keep responding. It's reclaimable memory; the kernel releases it
under pressure.

Two things help:

- **`qemu-guest-agent` in the lab VM.** Without it, the hypervisor can't tell usage
  from cache and reports everything the guest ever touched. With it, the panel went
  from 3.26 GiB to 961 MiB for the same real state.
- **kindling drops the memory file's cache after freezing.** That file is written in
  full and re-read to punch holes, then untouched until someone thaws that specific
  machine. Accumulation dropped from ~150 MiB to ~54 MiB per cycle.

**Golden** snapshots are deliberately not dropped: there, the cache is precisely what
lets N instances share pages.
