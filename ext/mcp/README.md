# kindling-mcp

Serverless MCP tools on [kindling](https://github.com/juan52878911/kindling)'s
Firecracker microVMs. Take any open source MCP server and turn it automatically into a
service that comes up on demand, in milliseconds, with kernel-level isolation.

kindling-mcp is an **extension of `kling`**: it does not add a new command to learn. Once
installed, `kling mcp`, `kling add`, `kling search`, `kling connect`, `kling gateway`,
`kling export`, `kling memory` and `kling migrate` appear in the same `kling` you already
use, in its help and in its shell completion.

> Status: **v0.1.0** — the MCP half of kindling v0.4/v0.5, now on its own on top of the
> kindling v0.6 core. Nothing changes for whoever used it: the same commands, the same
> snapshots (migrated in place by the daemon), the same gateway.

**The guest is assumed hostile**: there is no telling which MCP server will end up being
hosted. See kindling's [SECURITY.md](https://github.com/juan52878911/kindling/blob/main/SECURITY.md).

## Installation

kindling first — the core with the daemon — and this extension on top:

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling-mcp/main/scripts/install.sh | sh
kling plugins                 # kling-mcp should be listed
```

That installs `kling-mcp` and `kling-bridge` on your machine. The host with the daemon
needs its half too — the gateway, self-healing, the packager and the bridge that goes
inside every image:

```sh
make deploy HOST=ssh://juan@192.168.2.60   # after kindling's own make deploy
make deploy-mac HOST=ssh://user@lima-vm    # arm64 Linux VM on Apple Silicon
```

| kindling | kindling-mcp |
|---|---|
| v0.6.x | v0.1.x |

The installer refuses to install next to a `kling` older than the minimum the extension
declares.

## Upgrading from kindling v0.5 or earlier

Before v0.6 all of this was inside `kling`. After upgrading kindling, install this
extension and everything keeps working: the daemon migrates catalogs, health and links
into its generic annotations and store, and old `memory.*` settings move to
`mcp.memory.*` on their own. Redeploy the systemd units with `make deploy` so they run
`kling-mcp` instead of `kling`.

---

# MCP services

## Catalog: searching for and adding MCP servers

You don't need to know how to package anything. `kling` talks to the official registry
(`registry.modelcontextprotocol.io`):

```sh
kling search filesystem                    # what is out there, and what it can package on its own
kling add io.github.domdomegg/filesystem-mcp
```

`kling add` builds the image, boots a template, asks it what it can do, freezes it as a
golden snapshot and stores its catalog. From then on, listing its capabilities **does not
wake the microVM**.

It packages **npm and PyPI** servers that speak stdio (npm wins when a server publishes
on both). When `search` says a server cannot be packaged unattended, it explains why and
what the alternative is, instead of failing halfway through the build. Useful flags:

| Flag | What it does |
|---|---|
| `-bundle` | collapses `node_modules` into **one** file with esbuild — measured 1205 files → 1, cold `initialize` ~7 s → ~2.5 s. The main lever on Mac/arm64 |
| `-base node` / `-base python` | builds a small **layer** on a shared runtime base instead of a monolithic image ([layered images](https://github.com/juan52878911/kindling#layered-images-one-base-per-runtime-family)); picked automatically when a base named after the runtime family exists |
| `-env KEY=value` | bakes environment switches into the entrypoint (plain text: for toggles, **not secrets** — those go [via MMDS](https://github.com/juan52878911/kindling#secrets-that-never-touch-a-snapshot-mmds)) |
| `-cmd "..."` | overrides the inferred start command (PyPI entry points are inferred by convention and verified at build time) |
| `-volume name[:/mount][:ro]` | attach [persistent storage](https://github.com/juan52878911/kindling#volumes-what-outlives-the-microvm), repeatable |
| `-dry-run` | show what it would do without doing it |

kindling also **auto-detects capabilities**: if a server needs a browser, internet egress
or native binaries, the image and the machine's network policy are configured
accordingly — a browser-based server gets a shared Chromium with one context per session.

## Turning any MCP server into a service

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data

kling mcp import filesystem
```

`mcp import` does the whole cycle:

```
Importing "filesystem" from image "filesystem"

  1/5  starting the template... ✓ 0aeee853 at 172.30.0.6
  2/5  waiting for the MCP server... ✓
  3/5  asking what it can do... ✓ 14 tool(s)
  4/5  freezing as golden snapshot... ✓
  5/5  saving the catalog... ✓
```

Step 3 is **introspection**: the server is asked what it can do exactly once, and step 5
stores that catalog alongside the snapshot. The template's memory, vCPUs, egress policy,
volumes and labels all end up **baked into the snapshot** — and are reused verbatim when
the service is [healed](#self-healing-kling-mcp-heal) or refreshed.

**`-n` pre-installs the npm packages into the image.** That is not convenience: microVMs
boot with no internet egress, so an `npx -y` at runtime would fail to download.

### Listing the inventory does not touch the services

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
Status:    ✓ mcp-servers/everything v2.0.0 · 12 tool(s): echo, get-sum, …

$ call_tool everything.get-sum {"a":100,"b":23}
The sum of 100 and 23 is 123.
```

There is a third mode: the bridge can also act as an **HTTP/SSE proxy** for servers that
speak HTTP but should still start under the bridge's supervision — useful for stateless
services, where a single warm server process is shared and frozen alive inside the
snapshot (measured on `context7`: first `initialize` 25.7 s as stdio → 2.4 s steady as
http-proxy).

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

## Sessions and parallel replicas

MCP identifies conversations with `Mcp-Session-Id`, and a stdio server is **single-session
by nature**: its state lives in the process. Hence:

- **The bridge launches one child process per session.** Two concurrent conversations do
  not trample each other's state.
- **The gateway routes stickily.** The same session always goes back to the same microVM;
  sending it to another instance would find a server without that state.
- **The same tool can be used in parallel.** When concurrent sessions exceed what one
  instance can serve, the gateway creates **replicas per service** on demand from the
  golden snapshot (copy-on-write, so they share memory). Verified with 4 concurrent
  sessions against a service whose bridge caps at 1 session each.
- **When a bridge hits its session cap**, it recycles the most idle session instead of
  refusing, so clients reconnect cleanly.

Demonstrated with the `session_info` tool, which reports its pid and its call count:

```
session 1 (3 extra calls):  pid=305 llamadas_en_esta_sesion=5
session 2 (freshly created): pid=309 llamadas_en_esta_sesion=1
```

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

## Ephemeral or persistent: decided automatically

An ephemeral microVM dies with everything of its own, memory **and disk**. So the question
is not "does the server keep state?" but:

> does something one call writes have to be visible to a later call?

`kling mcp import` infers it from the catalog and says so:

```
eco          EPHEMERAL    because it only queries: nothing to preserve
notas        PERSISTENT   because it writes with guardar_nota and reads with session_info
filesystem   PERSISTENT   because it writes with write_file and reads with read_file
memory       PERSISTENT   because read_graph suggests it accumulates context
thinking     PERSISTENT   because sequentialthinking suggests it accumulates context
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
memory [remembers between calls]: create_entities, add_observations, ...
filesystem: read_text_file, write_file, list_directory, ...
```

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
Context cost, with your current catalog:
  proxy    3 definitions   ≈  248 tokens
  expand  28 definitions   ≈ 4327 tokens
  → The proxy mode you're using saves 4079 tokens.
```

The crossover sits around 8 tools. With 28, proxy saves **17x**; with 200, tens of
thousands of tokens in every conversation.

### The inventory travels in the handshake

Tool names are cheap — 27 of them take ~100 tokens — what is expensive are the argument
schemas. That is why `initialize` returns the **full inventory** in its `instructions`
field:

```
Available tools, grouped by service:

filesystem: read_text_file, write_file, list_directory, ...
memory [remembers between calls]: create_entities, ...

Call them with call_tool and the full service.tool name
(e.g. filesystem.read_text_file). If you don't know its arguments,
ask describe_tool first. find_tools is for searching by keyword.
```

That way the model knows what is there **from the first moment** and goes straight to
`call_tool`, instead of spending a call on discovery. Three meta-tools remain:
`find_tools`, `describe_tool` and `call_tool`.

### Bilingual search

The model asks in the user's language and the tools are described in English. Searching
"leer un fichero de texto" against *"Read the complete contents of a file"* did not match a
single term, so `find_tools` returned junk. A table of domain synonyms — leer/read,
fichero/file, carpeta/directory… — fixes it without pulling in a search engine.

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

## Connecting it to your AI agent

```sh
kling connect                          # step-by-step guide
kling connect eco                      # URL, status and configuration
kling connect eco -install opencode    # writes it for you
kling connect eco -install claude-code
kling connect -all -install all        # every detected agent at once
```

Seven clients are supported with `-install`: **Claude Code, opencode, Cursor, VS Code,
Windsurf, Cline and Zed** — or `-install all` to write to every one it detects.

`connect` **actually checks the service** — it does a real MCP `initialize` and lists the
tools — before handing you anything. A configuration that looks right and does not respond
is worse than none at all, because the failure shows up inside the agent and is far more
expensive to diagnose there.

```
Service:   eco
Endpoint:  http://192.168.2.60:8080/mcp/eco
Status:    ✓ kindling-echo v1.0.0 · 2 tool(s): echo, session_info
```

With `-install` it backs up the file before touching it (`.kling-backup`) and preserves the
rest of the configuration. For Claude Code it uses `claude mcp add` if the CLI is
available, which is the official route, and only writes the JSON if it is not.

`gateway.url` is the address **agents** use to reach the gateway, which need not be the
listen address:

```sh
kling config set gateway.url http://192.168.2.60:8080
```

## Migrating an existing MCP without breaking anything

```sh
kling migrate <mcp> -install <client>
```

`migrate` moves an MCP server you already use into kindling **keeping the entry's name
and the tools' names** — it connects through the per-service endpoint, so skills and
prompts that referenced `filesystem.read_text_file` keep working without a rewrite. That
is the difference from adding the server by hand and pointing your agent at the
aggregator, where names change.

## Bring your own memory service

A [volume](https://github.com/juan52878911/kindling#volumes-what-outlives-the-microvm) gives one service durable storage, but it
has **one writer**: it cannot be shared read-write across microVMs (an ext4 mounted twice
read-write corrupts itself — NFS or virtio-fs would add a lot of machinery for something
an MCP server already solves). For state that many tools and the model itself should
share, link an **external** MCP server:

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
./kling-bridge-local -- engram mcp --tools=agent
kling mcp link engram http://127.0.0.1:9100/mcp
```

Since v0.4.0 the local bridge listens on `127.0.0.1:9100` **by default**: what it wraps
is usually your personal memory, it does not authenticate, and `/reset` would be reachable
by anyone who can reach the port. If the gateway runs on another machine, exposing it is
still one explicit flag: `-listen 0.0.0.0:9100`.

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
engram then holds:  kindling: request "leer un fichero de texto"
                    was resolved with tool filesystem.read_text_file
```

It stores nothing of its own: it leans on whichever MCP service you linked, and looks
through that service's catalog for a writing tool instead of assuming any particular API.

## Official MCP servers running

Anthropic's official servers, hosted as microVMs:

```sh
sudo ./scripts/80-mcp-image.sh stdio filesystem \
     -n "@modelcontextprotocol/server-filesystem" -- mcp-server-filesystem /data
kling mcp import filesystem
```

```
SERVICE      TOOLS   CATALOG   HEALTH         MEMORY   INSTANCES
everything   13      6m ago    healthy (6m)   128M     1
thinking     1       3h ago    healthy (1h)   121M     1
memory       9       3h ago    healthy (2h)   122M     1
filesystem   14      3h ago    healthy (1h)   125M     1
notas        2       3h ago    healthy (3h)   42M      1
eco          2       3h ago    healthy (3h)   42M      1
engram       11      2h ago    —              —        external: http://192.168.2.3:9100/mcp

52 tool(s) across 7 service(s) (6 microVM, 1 external).
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

---

# Operations

## MCP gateway

Routes tool calls and wakes them on demand. It runs **separately from the daemon**, on
purpose: the daemon never listens on the network because controlling it is equivalent to
root on its host. The gateway does listen, but all it knows how to do is wake instances of
snapshots that already exist.

```sh
kling gateway -listen 127.0.0.1:8080 -idle 5m   # generates the token the first time

# The gateway REQUIRES a token: waking a snapshot is running code, and while the
# daemon protects itself by not listening, the gateway does listen.
T=$(kling config path >/dev/null && echo "$KLING_GATEWAY_TOKEN")
curl -H "Authorization: Bearer $T" http://127.0.0.1:8080/mcp/echo/
curl -H "Authorization: Bearer $T" http://127.0.0.1:8080/services
curl http://127.0.0.1:8080/healthz              # open: it is the liveness probe
```

The token is stored in `gateway.token` on the host the gateway runs on, and copied to the
client with `kling config set gateway.token …` (`kling connect` does it for you). To skip it
during development there is `-no-auth`, which insists on listening on loopback. The
gateway **never forwards its own token** to guests or third-party URLs — a compromised
MCP server must not walk away with the aggregator's credential. When one token is shared
by several tenants, **per-token quotas** keep one of them from starving the rest.

Measured end to end with a real MCP server inside the microVM:

| Path | Latency |
|---|---|
| Cold (instantiate from the golden snapshot) | **244 ms** |
| Hot | **9 ms** |
| After freezing on idle | **218 ms** (29 ms of thaw + guest networking) |

When the idle timeout expires the tool **freezes, it is not killed**: it stops costing CPU
and RAM, and the next call brings it back in milliseconds.

### Surviving reboots

```sh
sudo install -m644 packaging/kling-gateway.service /etc/systemd/system/
sudo systemctl enable --now kling-gateway
```

The gateway **does not run as root**: it only talks to the daemon over its socket and
proxies. All the privileged work stays in `kling.service`.

### Health is recorded from real traffic

The gateway already knows when a service fails — it returns a 502 — and that signal is
now **recorded** instead of thrown away: `kling mcp health` shows it, state changes are
persisted, and a later success recovers the service. This exists because nine services
once spent 26 hours down while `status` said "✓ 9": it was reporting inventory, and being
read as health.

## Self-healing: `kling mcp heal`

A host reboot invalidates **every** golden snapshot at once — Firecracker ties them to
the TSC frequency — and they used to require manual re-import. `heal` probes each
service and rebuilds **only what the TSC invalidated**: a service that is sick for any
other reason is not "fixed" by rebuilding it, and re-importing it would be noise covering
the real problem. It rebuilds with the service's **original** configuration — memory,
vCPUs, egress, volumes, labels — not the defaults.

Run it on a timer and reboots heal themselves:

```sh
sudo cp packaging/kling-heal.service packaging/kling-heal.timer /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl enable --now kling-heal.timer
# OnBootSec=2min, OnUnitActiveSec=6h
```

## Verifying a service, for real

```sh
kling mcp verify <service>     # -deep is the default
```

A verification that cannot fail is not a verification: `verify` used to ask for
`tools/list` and exit 0 without exercising anything. Now it **calls a real tool**
(`browser_navigate` on `about:blank` in browser images) and asks the bridge's `/dns`
endpoint for the guest's nameservers and whether they resolve — so a service with a
broken egress or a dead browser fails the check instead of passing it.

## The bridge lives inside every image

It is the guest's PID 1, so it is copied in when the image is built. Updating kindling on the
host does **not** update the bridge of services that are already packaged:

```sh
kling mcp refresh-bridge              # all of them
kling mcp refresh-bridge semgrep      # just one
kling images rm <image>           # retire an image nothing uses any more
kling images recipe <image>       # how it was built
```

This is not a missing feature, it is a baffling failure if you forget it: an old bridge does
not understand the new kernel command line parameters, dies at startup and — being PID 1 —
the guest panics. Deducing from a kernel panic that an image needs updating is asking too
much.

It never touches an image some microVM is using (modifying an ext4 that another system has
mounted corrupts it, even if that system holds it read-only), it compares by content so it
does not rewrite what is already current, and it writes alongside and renames: either the old
bridge is there or the new one, never a truncated one. If the bridge no longer fits, the
image is **grown** instead of failing. Refreshing invalidates the golden snapshot, and that
is **recorded as health** — so the service shows up as needing a re-import instead of
silently breaking. On a layered image it touches the layer (or, if the bridge is baked
into the base, just the base — once for every service on it).
