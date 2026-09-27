# Quickstart on Linux

From nothing to a throwaway microVM, a frozen machine and a template, on a Linux host
with KVM. About 15 minutes; most of it is the first image build.

## What you need

- Linux on amd64 or arm64 with `/dev/kvm` (bare metal, or a VM with nested virtualization
  enabled on its parent). Check: `ls -l /dev/kvm`.
- `nft` (nftables), `iptables` and `ip` (iproute2). Without `nft`, microVMs boot with no
  network; `kling up` catches it.
- systemd, if you want the daemon as a service (`kling up` installs the unit).
- Firecracker and jailer: `sudo ./scripts/20-install-firecracker.sh` from a checkout if
  `kling up` reports them missing. The kernel and the base image ship inside the binary.

## 1. Install the CLI

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
kling version
```

The installer downloads the release binary for your platform, verifies it against the
release's `SHA256SUMS`, puts it in `~/.local/bin` and adds the shell completion line to
your rc (`--no-rc` skips that). Expected:

```
kling 0.14.0
```

## 2. Get the runtime ready

```sh
kling up
```

`kling up` checks KVM, nftables, the `kindling` service user, the artifacts and the base
images, and **prints the commands that need privileges instead of running them**: an
installer that creates system users and touches nftables on its own asks for more trust
than it should. Run what it prints (with `sudo`), then run `kling up` again; it starts the
daemon and its unit. To only check, `kling up -check`.

Then:

```sh
kling doctor
```

Every line is a piece with ✓, `!` or ✗, and each ✗ comes with its `fix:`. Exit code 1 only
when something fails, so it works in scripts. A healthy host looks like this (recorded
against a remote daemon; the runtime block is the same locally):

<p align="center"><img src="../img/demo-doctor.gif" alt="kling doctor and kling status output" width="820"></p>

## 3. A throwaway microVM

```sh
kling try -- uname -a
```

```
kling: sandbox 00284b51f7be (toolchain) ready in 2.261s
Linux 172.16.0.2 6.1.186 #1 SMP PREEMPT_DYNAMIC Wed Sep 16 03:21:30 UTC 2026 x86_64 Linux
```

`try` creates a sandbox from the `toolchain` image (node, npm, python3, pip; built on first
use), runs the command, exits with its exit code and removes the machine. No network unless
you ask for it:

```sh
kling try -egress internet -- sh -c 'wget -qO- https://api.github.com/zen; echo'
kling try                       # no command: an interactive shell, removed on exit
kling try -keep -- true         # keeps it and prints its id, for kling exec / shell / cp
```

## 4. A machine you keep: run, freeze, thaw

```sh
kling run -name demo -image min
kling ps
kling freeze demo
kling ps
kling thaw demo
kling logs -tail 20 demo
```

`run` boots the `min` image (Alpine, ~17 MB shared, no service manager) cold, in well under
a second on native KVM. `freeze` dumps it to a snapshot: the process disappears, what remains
is a sparse file on disk (~35 MB with `min`), and `ps` shows it as `frozen`. `thaw` restores
it in tens of milliseconds with everything as it was. `logs` is the serial console, the only
window inside a machine that has no guest agent.

`kling ps` on a host with a few frozen machines and templates (recorded):

<p align="center"><img src="../img/demo-ps.gif" alt="kling ps, kling template ls and kling top" width="900"></p>

## 5. A template: boot once, restore many

A template (golden snapshot) is what makes everything fast. Prepare a machine with exec
allowed, install what you need, save it, and every machine made from it starts in
milliseconds with that already inside:

```sh
kling image toolchain                                   # once per daemon; a few minutes
kling run -name base -image toolchain -allow-exec -egress internet -mem 1G
kling exec base -- pip install --quiet requests
kling save base ready
kling try -from ready -- python3 -c 'import requests; print(requests.__version__)'
```

`save` waits until the guest is serving before it freezes (a snapshot taken too early
restores fine and never answers), and by default keeps a warm runtime child inside so the
first wake does not pay node's or python's start-up. `kling template ls` lists what you
have; `kling template inspect ready` shows its annotations.

## 6. Where to go next

- Host your agent's tools: [MCP servers for Claude Code, opencode and Cursor](mcp-servers.md).
- Let an agent run code safely: [Sandboxes for AI agents](sandboxes-for-agents.md).
- Keep the daemon here and work from your laptop: [Remote daemon over SSH](remote-daemon-ssh.md).
- Everything `kling` can do, with the design behind it: [`docs/handbook.md`](../handbook.md).

## Cleaning up

```sh
kling rm demo base
kling template rm ready
kling image rm toolchain        # refuses while a template or machine still uses it
```
