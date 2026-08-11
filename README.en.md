[Léelo en español](README.md)

# kindling

Serverless MCP tools on Firecracker microVMs. The end goal: take any open source MCP
server and turn it automatically into a service that comes up on demand, in
milliseconds, with kernel-level isolation.

> Status: **complete** — the whole circuit works. `kling` manages microVMs behind a
> docker-like interface, with networking, golden snapshots, isolation, events, an MCP
> gateway with sessions and a **stdio→HTTP bridge**: any open source MCP server can be
> hosted on demand, whether it speaks stdio or native Streamable HTTP.

**The guest is assumed hostile**: there is no telling which MCP server will end up being
hosted. See [SECURITY.md](SECURITY.md) for the threat model, the barriers in place and
— above all — what is NOT solved yet.

## kling

> The CLI still prints in Spanish. The transcripts below are reproduced verbatim, so
> what you see here is what the tool actually outputs.

```
$ kling info
endpoint:     ssh://juan@192.168.2.60
daemon:       0.1.0
root:         /var/lib/kindling
KVM:          sí
firecracker:  Firecracker v1.16.1
máquinas:     7

$ kling run -name mcp-demo
efad9e5f7003  mcp-demo  arrancada en frío en 54 ms

$ kling freeze mcp-demo
efad9e5f7003  warm  (754 ms, 256 MiB en disco)

$ kling ps
ID             NOMBRE     IMAGEN    ESTADO   CPU/MEM    EDAD   ÚLTIMA OP
efad9e5f7003   mcp-demo   default   warm     1/256MiB   17s    freeze 754ms, 256MiB

$ kling thaw mcp-demo
efad9e5f7003  running  (22 ms)
```

### Golden snapshots

Freeze a machine once and instantiate N copies that **share its memory**:

```
$ kling commit plantilla golden
golden  snapshot dorado  (80M de memoria)

$ kling run -from golden -name g1
a3f9...  g1  instanciada desde golden en 34 ms

$ kling snapshots
NOMBRE   IMAGEN    CPU/MEM    MEMORIA   DISCO   INSTANCIAS   EDAD
golden   default   1/256MiB   80M       80M     10           21s

$ kling events
23:52:20  machine.frozen   mcp-demo  congelada en 754 ms (256 MiB en disco)
23:52:20  machine.thawed   mcp-demo  descongelada en 22 ms
```

The **`warm`** state is what sets kindling apart from a container runtime: the machine is
frozen on disk, burns neither CPU nor RAM, and wakes up in tens of milliseconds.

### Connecting

The same binary is both CLI and daemon. `kling daemon` runs wherever KVM is; the CLI
talks to it over a Unix socket, locally or through SSH:

```sh
export KLING_HOST=ssh://juan@192.168.2.60   # remote daemon
export KLING_HOST=/run/kling.sock           # local daemon
```

**The daemon never listens on a network port.** Controlling microVMs is equivalent to
root on their host: it can mount disks and boot arbitrary kernels. Exposing it over TCP
would repeat the mistake that has cost Docker a decade of compromised servers. For remote
access it uses SSH with the same technique as `docker context`: instead of requiring
socat or nc on the far end, it invokes `kling dial-stdio`, which bridges the SSH pipe to
the local socket.

### Installation

**Quick option — pre-built binaries (recommended):**

```sh
# macOS / Linux — one line, no dependencies
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh

# To include kling-bridge (the daemon needs it when it rebuilds images):
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh -s -- --bridge

# A specific version (it installs the latest release by default):
curl -fsSL .../install.sh | sh -s -- --tag v0.1.0

# Custom prefix:
curl -fsSL .../install.sh | sh -s -- --prefix ~/.local --bridge
```

Binaries are published on
[Releases](https://github.com/juan52878911/kindling/releases) for **linux/amd64**,
**linux/arm64**, **darwin/amd64** and **darwin/arm64**. Every release ships a
`SHA256SUMS`, and the install script verifies the checksum before moving anything onto
disk. **Windows is not supported** — the code uses POSIX syscalls (`syscall.Kill`,
`Setsid`, `Stat_t`).

**From source — `make`:**

```sh
make install                              # CLI on your machine
make deploy HOST=ssh://juan@192.168.2.60  # daemon on the host with KVM
```

`make install` picks the first writable directory on your PATH and **does not ask for
sudo**: installing a user tool should not require it. Force it with
`make install PREFIX=/usr/local` if you prefer it system-wide.

`make deploy` builds for `linux/amd64`, copies the binary and the systemd unit, and starts
the service. The unit hands the socket to the user you SSH in as, so you don't have to run
the entire client under sudo.

### Configuration

Named contexts, in the style of `docker context`, so you don't have to carry `KLING_HOST`
around:

```sh
kling context add lab ssh://juan@192.168.2.60 -description "Home Proxmox"
kling context use lab
kling context ls
```

And defaults, so you don't repeat the same options on every `run`:

```sh
kling config set defaults.image min
kling config set defaults.ttl_seconds 600
kling config set gateway.idle 5m
kling config show
```

The file lives at `~/.config/kling/config.json` — on macOS too: `UserConfigDir()` would
put it under `~/Library/Application Support`, which is right for desktop apps but
surprising for a CLI.

**Precedence:** `-H` > `$KLING_HOST` > active context > local socket. The flag always
wins, so a one-off invocation doesn't force you to switch contexts.

## Volumes: what outlives the microVM

Each machine's overlay dies with it. A **volume** is the opposite: an ext4 file on the host,
exposed as a third disk (`vdc`), that persists.

```sh
kling volume create notes -size 2G
kling mcp import notes-mcp -volume notes          # or: kling add <server> ...
kling volume ls
```

```
NAME     LOGICAL   ON DISK   USED BY
notes    2.0G      4.0M      notes-a3f9
```

**Why a disk and not a host directory.** The natural request is "mount `~/notes` inside".
It isn't done: Firecracker has no virtio-fs, and more importantly a host directory mounted
read-write hands the guest — which is assumed hostile — a direct channel into the host's
filesystem. That would throw away the very boundary that justifies using microVMs instead of
containers. The file is sparse, so it only takes up what actually gets written.

**A volume is declared at import time, not later.** Firecracker cannot add drives to a
restored VM, so the device has to be present when the golden snapshot is frozen. A service
imported without a volume cannot get one without re-importing, and kling says exactly that
instead of failing inside the guest.

**Who mounts it.** The bridge, not the image's init — so no base image needs rebuilding. The
mountpoint travels on the kernel command line (`kling.volume=/data`), which means the same
golden snapshot works with different volumes.

Deleting a volume that a microVM has mounted would corrupt its filesystem underneath, so
`kling volume rm` refuses and tells you who is using it.

## Why microVMs and not containers

An MCP server is a 50-100 MB Node or Python process. Putting it inside a microVM does not
save resources compared to a container — it costs more, because every microVM boots its
own kernel.

The reason to do it is a different one: **a local AI running arbitrary open source tooling
is untrusted code**. A container's isolation is a namespace of the shared kernel; a
microVM's is a hypervisor boundary. That is the project's only honest justification, and
it is worth being clear about it before writing another line.

## Measured numbers

Measured on Proxmox (Intel i7-8700T) with Firecracker v1.16.1 running **nested** inside a
VM, kernel 6.1.177 and an 800 MB Ubuntu 24.04 rootfs:

| Operation | Time |
|---|---|
| Cold boot | **2,643 ms** |
| Snapshot creation | 305 ms |
| **Restore from snapshot** | **~30 ms** |

Reproduce them with `scripts/40-bench-boot.sh`.

**The conclusion that defines the architecture:** 2.6 s cold makes the one-microVM-per-request
model unworkable. The 125 ms Firecracker advertises assume a trimmed kernel and a minimal
rootfs on bare metal. With snapshot/restore, 30 ms is imperceptible inside a tool call.

Therefore: **every tool boots once, is frozen with the MCP server already listening, and
the gateway restores it on demand.** The snapshot is not an optional optimization; it is
what holds everything else up.

## Disk cost

The base image **is not copied**: it is mounted read-only and shared by every microVM.
Each machine only carries its own sparse overlay, mounted with overlayfs by
`/sbin/overlay-init` inside the guest.

| | |
|---|---|
| Base image `min` (Alpine), shared | **17 MB**, once |
| Base image `default` (Ubuntu), shared | 386 MB |
| Per running machine | **~8 MB** |
| Per `warm` machine, `min` image | **~35 MB** |
| Per `warm` machine, `default` image | ~82 MB |

Before overlays, every machine copied the full 800 MB: three machines cost 2.4 GB, now
they cost 386 MB + 25 MB.

**A `warm` machine consumes no RAM.** `freeze` kills the Firecracker process; what remains
is a file. Its cost is disk, not memory.

Firecracker dumps the entire memory when freezing, but most of it is zeroed pages.
kindling punches them out with `fallocate --dig-holes`: the kernel returns zeros when
reading a hole, which is exactly what was there, so the restore never notices.

**256 MB → 81 MB, and `thaw` is still ~30 ms.**

### What drives that cost

Two measurements that should guide any future optimization:

| Allocated RAM | Frozen cost |
|---|---|
| 512 MiB | 86 MB |
| 256 MiB | 81 MB |
| 96 MiB | 80 MB |

**Allocating more RAM is nearly free** once the file is sparse: what gets stored is the
real working set, not the reserved RAM. Lowering `-mem` is not the lever.

The lever is what boots inside:

| Guest | Frozen cost |
|---|---|
| Ubuntu 24.04 + systemd | 82 MB |
| Alpine without systemd (`min` image) | **35 MB** |

Nearly half the cost was Ubuntu userspace that an ephemeral tool never touches.
`scripts/70-build-minimal-image.sh` builds the `min` image: Alpine with
`/sbin/overlay-init` and no service manager, booting straight into `/entrypoint`.

Beyond that, two Firecracker techniques remain unexploited: **diff snapshots**, which store
only the pages changed relative to a base, and the **UFFD backend**, which lets several
microVMs restored from the same snapshot share pages in RAM. UFFD does not reduce disk
usage, but it is what gives you density when many tools are hot at once.

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
  │ µVM service  │        │ µVM service  │          │ µVM ephemeral│
  │ bridge→stdio │        │ native HTTP  │          │ dies at end  │
  └──────────────┘        └──────────────┘          └──────────────┘
        └────────────────────────┴─────────────────────────┘
                    one network namespace each
                                 ▲
                          ┌───────────┐
                          │  daemon   │  lifecycle, network, snapshots
                          └───────────┘
```

The **gateway** takes the call, restores the matching snapshot, proxies the request and
reaps the microVM once its TTL expires. Inside the guest it always calls the same place,
`:8080/mcp`, whether the server speaks stdio (with `kling-bridge` translating) or native
Streamable HTTP (with nothing in between).

The **daemon** manages the lifecycle and is the only one that can reach the guests: their
IPs only exist on the host's network. That is why it exposes `POST /machines/{ref}/guest`,
which forwards an HTTP request to the server inside. Without it, `kling mcp import` would
only work by running the CLI on the host itself — over SSH the probe has no route and
times out.

## Requirements

- A host with KVM and `cpu: host` (or equivalent) so the virtualization extensions get through
- If it runs nested, nested virtualization enabled on the parent host
- `firecracker` + `jailer`, `e2fsprogs`, `squashfs-tools`, `curl`, `jq`

On **macOS**: Firecracker does not run natively. On Apple Silicon M3 or newer with
macOS 15+ it can run inside an aarch64 Linux VM with nesting. That is fine for
development, not as a runtime — it burns more battery than the solution this project sets
out to avoid.

## Scripts

| | |
|---|---|
| `scripts/install.sh` | curl-pipe-sh installer: downloads the release binary and verifies SHA256 |
| `scripts/release.sh` | Creates the tag and pushes it; triggers the release workflow |
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
- [x] **Phase 4** — Gateway: route call → restore → proxy → reap on idle
- [x] **Phase 5** — stdio→HTTP bridge: servers that only speak over pipes, too

### What is still unresolved

The roadmap is complete; the project is not. What remains, ordered by how much it hurts:

- **Parallel calls to persistent services.** Around 25% of them hit the 60 s timeout.
  It is not the isolation: the bridge on its own dispatches 16 parallel calls in 127 ms,
  and ephemeral services do 8 out of 8 in 785 ms. The fault is in the gateway, not yet
  pinned down. In the meantime, go serial against a persistent service.
- **There is no durable storage.** A persistent service keeps its state for as long as its
  instance lives, no longer. Anything that must survive everything needs a host volume
  mounted inside the microVM, which is not implemented — hence the recommendation to link
  a memory service.
- **The missing barriers** are listed in [SECURITY.md](SECURITY.md): no chroot, no hard
  disk quota, no encryption at rest, unsigned golden snapshots.

## Field notes

See [docs/hallazgos.md](docs/hallazgos.md) — things that take hours to figure out on your
own, such as the fact that the artifact URLs in every tutorial on the internet return 404.

## Density: why the golden snapshot changes everything

A golden snapshot is an artifact **of an image, not of a machine**: you freeze once and N
instances restore from the same file. Because Firecracker **maps** it instead of reserving
anonymous memory, the kernel shares those pages across every instance and each one only
pays for what it writes.

Measured by instantiating one at a time and watching system RAM:

| | 10 from a golden snapshot | 10 cold-booted |
|---|---|---|
| Total RAM added | **+68 MiB** | +824 MiB |
| Per machine | **6.8 MiB** | 82 MiB |
| Time per machine | ~40 ms | ~2.6 s to userspace |

**12x the density.** The proof that pages are shared is in the gap between two numbers: the
sum of RSS across the ten processes came to 258 MiB, but system RAM only rose by 68 MiB.
The 190 MiB difference is shared pages that each process counts as its own.

This is, in practice, what UFFD is after — and it falls out of the `File` backend, without
writing a page-fault handler.

## Networking: one namespace per microVM

```
$ kling topo
kindling  ssh://juan@192.168.2.60
          KVM ok · Firecracker v1.16.1

  host  172.30.0.0/16
   ├─◆ golden           snapshot dorado · 82M de memoria compartida
   │  ├── g3             running  172.30.0.18       384K  thaw 28ms
   │  ├── g2             running  172.30.0.14       384K  thaw 26ms
   │  └── g1             running  172.30.0.10       384K  thaw 41ms
   │
   └─◆ (arrancadas en frío)
      └── plantilla      running  172.30.0.6          8M  boot 46ms

  4 running · 0 warm · 0 parada(s)   disco: 9M propio + 83M compartido
```

**The problem:** a snapshot records the name of the host's TAP device. If N instances
restore from the same golden snapshot, all N ask for the same TAP and collide. And you
cannot just reassign it: Firecracker does not allow patching `host_dev_name`.

**The solution:** one network namespace per microVM. Inside each one the TAP is always
called `tap0` and the guest always has the same IP, so **one snapshot works for all of
them**. All the differentiation happens on the host, on the far side of a veth:

```
        host                  │  netns kl-<id>          │  microVM
 vh-<id> 172.30.a.b/30 ◄─veth─► vg-<id> 172.30.a.b+1    │
                              │ tap0    172.16.0.1/30   ├─ eth0 172.16.0.2
```

The guest configures itself from the kernel's `ip=` parameter, with no need for networking
tools inside the image. From the host each machine is reachable at its namespace's IP,
which DNATs to the guest.

It is the same approach AWS Lambda uses, and for the same reason.

## Isolation

The guest is third-party code: assume it is hostile.

| Barrier | How |
|---|---|
| Daemon unreachable over the network | Unix socket only; remote access exclusively over SSH |
| Unprivileged VMM | `setpriv` to a service user: **CapEff 0**, `no_new_privs`, only the `kvm` group |
| No LAN access | Egress `none` by default; with `internet`, private networks stay blocked |
| No degrading the neighbours | 128 MiB/s of disk and 16 MiB/s of network per machine; cap of 256 |
| No repeated keys | virtio-rng + `CONFIG_VMGENID`: the guest reseeds on restore |

Verified **from inside the guest**, which is the only measurement that counts:

```
RESULT 192.168.2.100:   BLOCKED        (Proxmox host)
RESULT 192.168.2.1:     BLOCKED        (home router)
RESULT 10.10.10.1:      BLOCKED        (WireGuard tunnel)
RESULT 169.254.169.254: BLOCKED        (cloud metadata)
RESULT 1.1.1.1:         REACHABLE
```

## Lifecycle and robustness

```
$ kling run -image min -ttl 300 -cpu 25 -egress internet
$ kling logs <ref> -tail 50        # serial console: the only window inside
```

- **`-ttl`** freezes the machine by itself once that time passes. Freeze, not kill: it
  stops costing CPU and RAM, but comes back in ~30 ms. It is what makes the model
  serverless.
- **`-cpu`** bounds CPU usage with its own cgroup (50% of a core by default).
- **Reconciliation at startup**: the daemon compares its saved state against the host's
  reality, re-adopts the microVMs that are still alive and cleans up orphaned namespaces
  and cgroups.
- **Continuous watchdog**: every 10 s it checks that whatever claims to be `running` is
  actually running. A machine whose process disappeared moves to `failed` and releases its
  resources.

**Restarting the daemon does not kill the microVMs.** The unit carries `KillMode=process`;
without it systemd drags the whole cgroup down and takes the running machines with it.

### Measured with 8 instances

```
RAM añadida:          113 MiB   (14 MiB por instancia)
conectividad:         9/9
VMM sin privilegios:  9/9
cgroups activos:      9
```

## MCP gateway

Routes tool calls and wakes them on demand. It runs **separately from the daemon**, on
purpose: the daemon never listens on the network because controlling it is equivalent to
root on its host. The gateway does listen, but all it knows how to do is wake instances of
snapshots that already exist.

```sh
kling gateway -listen 127.0.0.1:8080 -idle 5m
curl http://127.0.0.1:8080/mcp/echo/       # the tool appears on its own
curl http://127.0.0.1:8080/services        # inventory and what is hot
```

Measured end to end with a real MCP server inside the microVM:

| Path | Latency |
|---|---|
| Cold (instantiate from the golden snapshot) | **244 ms** |
| Hot | **9 ms** |
| After freezing on idle | **218 ms** (29 ms of thaw + guest networking) |

When the idle timeout expires the tool **freezes, it is not killed**: it stops costing CPU
and RAM, and the next call brings it back in milliseconds.

### Wrapping an MCP server

```sh
sudo ./scripts/80-mcp-image.sh my-tool ./my-server "nodejs npm"
kling run -name tmpl -image my-tool -service my-tool
kling commit tmpl my-tool && kling stop tmpl
```

The directory needs an executable `entrypoint` that listens on port 8080. See
[examples/echo](examples/echo).

## Turning any MCP server into a service

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data

kling mcp import filesystem
```

`mcp import` does the whole cycle:

```
1/5  arrancando la plantilla...        ✓ 0aeee853 en 172.30.0.6
2/5  esperando al servidor MCP...      ✓
3/5  preguntando qué sabe hacer...     ✓ 14 herramienta(s)
4/5  congelando como snapshot dorado...✓
5/5  guardando el catálogo...          ✓
```

Step 3 is **introspection**: the server is asked what it can do exactly once, and step 5
stores that catalog alongside the snapshot.

**`-n` pre-installs the npm packages into the image.** That is not convenience: microVMs
boot with no internet egress, so an `npx -y` at runtime would fail to download.

## Listing the inventory does not touch the services

Without a persisted catalog, asking "what tools are there?" forces a `tools/list` against
every server, and that **wakes their microVMs**. One inventory question would end up
booting twenty machines.

With `mcp import`, the catalog lives on disk next to the snapshot:

```sh
kling mcp list -v          # every tool, without starting anything
kling mcp refresh <svc>    # recapture after updating the server
```

Verified: listing the full inventory through the aggregator leaves the machine counter at
**0**.

## Ephemeral mode: one microVM per action

```sh
kling gateway -ephemeral -prewarm 3
```

Every call gets **its own microVM**: one is taken from the pool of pre-warmed machines, it
serves the action and is destroyed. It is born, acts and dies.

```
action 1: 19 ms   pid=305 llamadas_en_esta_sesion=1
action 2: 24 ms   pid=305 llamadas_en_esta_sesion=1
action 3: 19 ms   pid=305 llamadas_en_esta_sesion=1
```

`llamadas_en_esta_sesion=1` **in all of them**: no action sees what the previous one did.

### From 350 ms to 19 ms

Profiling an unoptimized ephemeral action:

| Stage | Cost |
|---|---|
| Restore the microVM | 131 ms |
| Wait for networking to come back | 53 ms |
| `initialize` (launches the MCP server) | 61 ms — **with node it is 300-500 ms** |
| `tools/call` | 9 ms |
| Destroy the machine | ~100 ms |

Everything except `tools/call` can be paid up front or afterwards:

- **`-prewarm N`** keeps N instances already restored and **with their MCP session open**.
  The call skips restoring, waiting for the network and initializing.
- **Destruction is asynchronous.** It used to sit in a `defer`, so the client waited for
  the namespace teardown and the file deletion: 100 ms on top of a 2 ms call. The machine
  dies all the same; the client just no longer waits for it to happen.

Result: **2 ms of actual execution, 19 ms end to end.**

The trade-off is that there is no state between calls. Tools that need it — memory,
step-by-step reasoning — have to use the session-based route (`/mcp/<service>`), which
keeps the process alive.

## Any MCP server, hosted on demand

Most open source MCP servers only speak **stdio**: a persistent child process you talk to
over pipes. There is no port to call, and the client dictates the lifecycle. It is the
opposite of invocable on demand.

`kling-bridge` runs **inside** the microVM, launches the server as a child and exposes its
protocol over Streamable HTTP:

```
gateway ──HTTP──> kling-bridge ──stdin/stdout──> MCP server
```

From the outside, a stdio server looks HTTP-native. Wrapping one is a single line:

```sh
make bridge
sudo ./scripts/80-mcp-image.sh stdio files -p "nodejs npm" -- \
     npx -y @modelcontextprotocol/server-filesystem /data

kling run -name files-tmpl -image files -service files
kling commit files-tmpl files && kling stop files-tmpl
```

### Servers that already speak HTTP

If the server speaks **native Streamable HTTP** there is no need for a bridge: it listens
itself and the gateway talks to it directly. The `http` mode accepts the same options:

```sh
sudo ./scripts/80-mcp-image.sh http everything -p "nodejs npm" \
     -n "@modelcontextprotocol/server-everything" -- mcp-server-everything streamableHttp

kling mcp import everything -image everything
```

Two conditions, and the generated entrypoint sets both:

- **listen on `$PORT` (8080)**, which is where the gateway looks inside the guest
- **serve the protocol at `/mcp`**, which is the path it calls

Tested with `@modelcontextprotocol/server-everything`, the protocol's reference server.
The image carries no `kling-bridge` anywhere:

```
$ kling connect everything
Estado:    ✓ mcp-servers/everything v2.0.0 · 12 herramienta(s): echo, get-sum, …

$ call_tool everything.get-sum {"a":100,"b":23}
The sum of 100 and 23 is 123.
```

### Sessions

MCP identifies conversations with `Mcp-Session-Id`, and a stdio server is **single-session
by nature**: its state lives in the process. Hence:

- **The bridge launches one child process per session.** Two concurrent conversations do
  not trample each other's state.
- **The gateway routes stickily.** The same session always goes back to the same microVM;
  sending it to another instance would find a server without that state.

Demonstrated with the `session_info` tool, which reports its pid and its call count:

```
session 1 (3 extra calls):  pid=305 llamadas_en_esta_sesion=5
session 2 (freshly created): pid=309 llamadas_en_esta_sesion=1
```

### The full circuit

```
local model   ──>  gateway  ──>  microVM  ──>  MCP server
 (your Mac)       (Proxmox)     (Firecracker)   (stdio or HTTP)
```

[examples/agent/agent.py](examples/agent/agent.py) closes it: an MCP client plus a
tool-calling loop against ollama.

```
$ python3 examples/agent/agent.py "usa echo para decir hola"
→ kindling-echo v1.0.0  sesión b2787e00
→ herramientas: echo, session_info
→ llamando echo({"text": "hola"})
← hola
```

The model knows nothing about microVMs: it asks for a tool and the tool shows up. If it
had not been used for a while it was frozen, and waking it costs milliseconds.

| Path | Latency |
|---|---|
| Cold MCP handshake, from the Mac | **310 ms** |
| Tool call, hot | **9 ms** |

## Type repair

Several MCP clients and models mangle JSON types before sending them: arrays arrive as
objects with `"0"`, `"1"` keys, numbers as strings, booleans as `"true"`. The server
rejects them with "expected array, received object", and from the outside it looks like a
tool failure when the tool never even saw the call.

Since the catalog stores each tool's declared schema, the aggregator undoes the damage
before forwarding. Four broken shapes are recognized where the schema asks for an array:

| What arrives | Repaired to |
|---|---|
| `{"0":…,"1":…}` indexed object | `[…,…]` |
| `"[{…}]"` string with JSON inside | `[{…}]` |
| `{…}` a bare object | `[{…}]` |
| `{"paths":{"paths":[…]}}` wrapped | `[…]` |

Only what contradicts the schema is converted: a legitimate object is left untouched.
Every repair is logged, and if a server rejects the arguments **despite** the repair, what
was sent to it gets logged too — without that it is impossible to know what shape the
client gave them.

## What persists and what does not

Worth being clear about, because it is not obvious:

| | Survives |
|---|---|
| State of an **ephemeral** service | nothing: the microVM dies after every action |
| State of a **persistent** service | freezes and thaws of ITS instance |
| | but **not** that instance being deleted |
| Base image and golden snapshot | everything: they are files on the host |

A persistent service keeps its contents for as long as its instance lives; the instance
freezes when idle and comes back intact. But if that instance is deleted — manual cleanup,
`kling rm`, reinstalling the service — the state goes with it, because it lives in its
overlay.

**kindling gives you session persistence, not durable storage.** For data that has to
survive everything you need a host volume mounted inside the microVM, which is not
implemented today.

## Connecting it to your AI agent

```sh
kling connect                          # step-by-step guide
kling connect eco                      # URL, status and configuration
kling connect eco -install opencode    # writes it for you
kling connect eco -install claude-code
```

`connect` **actually checks the service** — it does a real MCP `initialize` and lists the
tools — before handing you anything. A configuration that looks right and does not respond
is worse than none at all, because the failure shows up inside the agent and is far more
expensive to diagnose there.

```
Servicio:  eco
Endpoint:  http://192.168.2.60:8080/mcp/eco
Estado:    ✓ kindling-echo v1.0.0 · 2 herramienta(s): echo, session_info
```

With `-install` it backs up the file before touching it (`.kling-backup`) and preserves the
rest of the configuration. For Claude Code it uses `claude mcp add` if the CLI is
available, which is the official route, and only writes the JSON if it is not.

`gateway.url` is the address **agents** use to reach the gateway, which need not be the
listen address:

```sh
kling config set gateway.url http://192.168.2.60:8080
```

### Surviving reboots

```sh
sudo install -m644 packaging/kling-gateway.service /etc/systemd/system/
sudo systemctl enable --now kling-gateway
```

The gateway **does not run as root**: it only talks to the daemon over its socket and
proxies. All the privileged work stays in `kling.service`.

## A single entry point for every service

An MCP client loads the definitions of **all** tools when it connects. With twenty
services of ten tools each that is two hundred JSON schemas in the model's context before
it starts working.

```sh
kling connect -all -install opencode              # every service
kling connect -all -only eco,notas -install opencode   # only some
kling connect -all -expand                        # full catalog
```

The `/mcp/_all` endpoint is an MCP server that routes to the others. It has two modes:

**`proxy`** (default) — exposes **four meta-tools** instead of N:

| | |
|---|---|
| `list_services` | which servers exist and how many tools each one has |
| `find_tools` | search by keyword; returns names and descriptions, **without schemas** |
| `describe_tool` | the full schema of a single tool |
| `call_tool` | executes, routing to whichever microVM is needed |

The model searches for what it needs, asks for the schema of what it is going to use, and
calls.

**`expand`** — flattens the catalog with `service.tool` names, for clients that work better
with everything loaded.

### Which one is cheaper depends on how many tools you have

The `proxy` mode has a **fixed cost** of ~300 tokens; `expand` grows with every tool. With
few tools, proxy comes out **more expensive**. That is why `connect -all` measures it
against your real catalog and tells you:

```
Coste en contexto, con tu catálogo actual:
  proxy    3 definiciones  ≈  248 tokens
  expand  28 definiciones  ≈ 4327 tokens
  → El modo proxy que estás usando ahorra 4079 tokens.
```

The crossover sits around 8 tools. With 28, proxy saves **17x**; with 200, tens of
thousands of tokens in every conversation.

## Official MCP servers running

Anthropic's official servers, hosted as microVMs:

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data
kling mcp import filesystem
```

```
SERVICIO     HERRAMIENTAS   CATÁLOGO   MEMORIA   INSTANCIAS
everything   13             6m atrás   128M      1
thinking      1             3h atrás   121M      1
memory        9             3h atrás   122M      1
filesystem   14             3h atrás   125M      1
notas         2             3h atrás    42M      1
eco           2             3h atrás    42M      1
engram       11             2h atrás     —       externo: http://192.168.2.3:9100/mcp

52 herramienta(s) en 7 servicio(s) (6 microVM, 1 externo(s)).
```

`everything` is the protocol's reference server and speaks **native Streamable HTTP**: it
carries no bridge. The rest speak stdio and are wrapped. From the outside you cannot tell
them apart.

Real usage, through the aggregator and on single-use microVMs:

```
filesystem.read_text_file  /data/test.txt     ->  "hello from kindling"   (31 ms)
memory.create_entities     kindling/project   ->  entity created          (31 ms)
everything.get-sum         {"a":100,"b":23}   ->  "The sum … is 123."     (native)
```

**31 ms per action**, each one on its own machine, which dies when it finishes.

### Three things to know when packaging a node server

- **`-n` pre-installs the npm package.** microVMs boot with no internet egress: an
  `npx -y` would fail to download.
- **The `entrypoint` is PID 1 and the kernel gives it no PATH.** Without setting it, the
  binaries npm installs are not found: `executable file not found in $PATH`.
- **The directories the server expects must exist INSIDE the image.** `server-filesystem`
  wants `/data`; creating it on the host does nothing.
- **If the server speaks HTTP, it has to listen where it is told.** The entrypoint sets
  `PORT=8080` and the server must serve the protocol at `/mcp`: that is what the gateway
  looks for.

## Ephemeral or persistent: decided automatically

An ephemeral microVM dies with everything of its own, memory **and disk**. So the question
is not "does the server keep state?" but:

> does something one call writes have to be visible to a later call?

`kling mcp import` infers it from the catalog and says so:

```
eco          EFÍMERO      porque solo consulta: no deja nada que preservar
notas        PERSISTENTE  porque escribe con guardar_nota y lee con session_info
filesystem   PERSISTENTE  porque escribe con write_file y lee con read_file
memory       PERSISTENTE  porque read_graph sugiere que acumula contexto
thinking     PERSISTENTE  porque sequentialthinking sugiere que acumula contexto
```

**`filesystem` is persistent too**, even though it may not look like it: it writes to the
guest's disk, which is exactly as volatile as its memory.

The reliable signal is structural — the server exposing both writing and reading tools at
once — with a handful of words matched against the tool NAME for servers that only have
one. Looking for those words in the descriptions classified everything as persistent:
"session" or "sequence" show up in passing in almost any text.

You can force it with `-stateful` or `-ephemeral`.

### When in doubt, persistent

Getting it wrong towards ephemeral produces **silent loss**: the call answers fine and what
was written disappears. Getting it wrong towards persistent only costs one frozen
instance, which spends neither CPU nor RAM.

The aggregator flags it in its inventory so the model knows:

```
memory [recuerda entre llamadas]: create_entities, add_observations, ...
filesystem: read_text_file, write_file, list_directory, ...
```

## The inventory travels in the handshake

Tool names are cheap — 27 of them take ~100 tokens — what is expensive are the argument
schemas. That is why `initialize` returns the **full inventory** in its `instructions`
field:

```
Herramientas disponibles, agrupadas por servicio:

filesystem: read_text_file, write_file, list_directory, ...
memory [recuerda entre llamadas]: create_entities, ...

Llámalas con call_tool y el nombre completo servicio.herramienta.
Si no conoces sus argumentos, pide primero describe_tool.
```

That way the model knows what is there **from the first moment** and goes straight to
`call_tool`, instead of spending a call on discovery. Three meta-tools remain:
`find_tools`, `describe_tool` and `call_tool`.

### Persistent does not mean always on

A persistent service keeps its state, but **stops consuming when it is done**. Measured
with `-idle 30s`:

```
write /data/note.txt              →  Successfully wrote
read /data/note.txt (another call) →  "this must survive"

running:      running   41 MiB above baseline
after 35 s:   warm      RAM back to 0   (freeze 661 ms)
on return:    "this must survive"       (thaw 18 ms)
```

Freezing is not shutting down: the instance stops existing as a process — zero CPU, zero
RAM — but its state stays on disk and comes back in milliseconds. The cost is the memory
file: 151 MiB for this service while it is frozen.

## Bring your own memory service

kindling does not implement shared storage, and that is deliberate: mounting a common
filesystem across microVMs was tried and discarded. Firecracker only exposes block
devices, and an ext4 shared between several VMs corrupts itself; the alternatives — NFS,
virtio-fs — add a lot of machinery for something an MCP server already solves.

Instead, you link an **external** MCP server:

```sh
kling mcp link engram http://192.168.2.3:9100/mcp -description "shared memory"
kling mcp unlink engram
```

It does not run in a microVM: it stays where it already was, and kindling only routes to
it. It shows up in the aggregator as one more service, so any tool — and the model — can
save to it and read from it.

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

Self-contained: no CDN, no remote fonts, no requests when you open it — it describes a
homelab's topology and has no business telling anyone about it. It is generated on **your**
machine, not on the daemon.

It is the same tree as always — the host on the left, its services in a column, the
instances on the right — but navigable: every box with children opens and closes, and
whichever one you pick is detailed below.

```
                        ┌ eco ──────────┐
                        │ 2 herramientas│  sin instancias · ~250 ms
                        └───────────────┘
                        ┌ engram ───────┐
                        │ 11 herramient.│  no corre aquí
 ┌ host ────────────┐   └───────────────┘
 │ ssh://…2.60    − ├───┌ filesystem ───┐   ┌ fs-66e51d ────┐
 └──────────────────┘   │ 14 herramient.├───┤ 244f32b7      │  172.30.0.54 · thaw 30 ms
                        └───────────────┘   └───────────────┘
```

The border tells you the state: green serving, amber asleep, dotted grey ready but with no
instance, blue external. A dotted service is not broken — it appears on its own as soon as
someone calls it, and the grey annotation on the right says how much that will cost.

### Four views of the same system

| view | what it shows |
|---|---|
| **Topology** | the host, its services and the live instances of each one |
| **Layers** | what a call goes through: gateway → aggregator → microVM → kernel, rootfs, overlay, bridge → MCP server |
| **MCP** | the full catalog: every service with its tools, marking which ones write |
| **Network** | who can reach the internet and who is isolated, namespace by namespace |

### Drill down

Clicking a box opens it. If you want to go all the way down a level, the panel offers
**Drill down**: that node becomes the root and a breadcrumb appears to get back.

```
catálogo › filesystem
```

The bottom panel changes with whatever you select: the node's data, the step-by-step flow
of a call to that service, and — if it writes anything — where what it writes ends up.

Nodes are grouped by the `service` label, and failing that by their source snapshot: two
machines from the same snapshot share memory and belong together even if nobody labelled
them.

```sh
kling run -from eco -service eco -label tier=prod
```

```
Ojo con lo que escriba
Escribe con create_directory, edit_file, move_file, write_file.
Vive mientras viva la instancia → guárdalo en engram.
```

That warning — "watch what it writes; it lives only as long as the instance does, so put it
in engram" — is the rule that causes the most confusion: a microVM's overlay dies with it. If a
tool needs to persist a file, a row in a database or anything else, **the right
destination is the linked memory service**, not the guest's disk. The Layers view marks it
on the `overlay propio` node itself.

## Usage memory (optional)

Off by default: kindling does not write into anyone's memory unless asked. The bridge
binary is always installed, though, so turning it on is one command rather than a project.

```sh
kling memory status            # whether it is on and against what
kling memory install-service   # leaves the local bridge as a permanent service (macOS)
kling memory enable            # uses engram; -service <svc> for another one
kling memory disable
```

When it is on, the gateway records in the memory service which tool resolved each request,
and uses that history to rank subsequent searches better:

```
search "leer un fichero de texto"  →  filesystem.read_text_file
use the tool                       →  "hello from kindling"
engram then holds:  kindling: la petición "leer un fichero de texto"
                    se resolvió con la herramienta filesystem.read_text_file
```

It stores nothing of its own: it leans on whichever MCP service you linked, and looks
through that service's catalog for a writing tool instead of assuming any particular API.

### Bilingual search

The model asks in the user's language and the tools are described in English. Searching
"leer un fichero de texto" against *"Read the complete contents of a file"* did not match a
single term, so `find_tools` returned junk. A table of domain synonyms — leer/read,
fichero/file, carpeta/directory… — fixes it without pulling in a search engine.

## Memory: what is real and what is cache

After many freeze and thaw cycles, the hypervisor may show the lab VM at 80% memory. Almost
all of it is **disk cache**, not real usage:

```
Cached:      2.9 GiB    ← what you see in the panel
AnonPages:   273 MiB    ← process memory, the real number
```

You can check by dropping it: the cache falls from 3,098 to 178 MiB, usage settles at
~600 MiB and the microVMs keep responding. It is reclaimable memory; the kernel releases it
under pressure.

Two things help:

- **`qemu-guest-agent` in the lab VM.** Without it, the hypervisor cannot tell usage from
  cache and reports everything the guest has ever touched. With it, the panel went from
  3.26 GiB to 961 MiB for the same real state.
- **kindling drops the page cache of the memory file after freezing.** That file is written
  in full and read back to punch holes in it, and then nobody touches it until someone
  thaws that particular machine. It brought the accumulation down from ~150 MiB to ~54 MiB
  per cycle.

**Golden** snapshots are deliberately not dropped: there the cache is precisely what lets N
instances share pages.
