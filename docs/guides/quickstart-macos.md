# Quickstart on macOS (Apple Silicon, `vz` backend)

Since v0.9 the daemon runs **on the Mac itself**: instead of Firecracker (KVM, so Linux) it
launches one `kling-vz` per microVM, a helper that speaks Firecracker's API on top of
Apple's Virtualization.framework. No root, no Linux VM in between. Reference:
[`docs/mac.md`](../mac.md).

Two other ways to use kindling from a Mac still work: drive a Linux daemon over SSH
([Remote daemon over SSH](remote-daemon-ssh.md)) — the best option for density — or run
real Firecracker inside a Lima VM with nested virtualization on M3+
([`docs/mac-arm64.md`](../mac-arm64.md)).

## What you need

| | Why |
|---|---|
| Apple Silicon | guests are aarch64; there is no `vz` backend on Intel Macs |
| macOS 14 (Sonoma) or newer | freeze/thaw use the VM state save/restore that arrived in 14 |
| `kling-vz` signed with `com.apple.security.virtualization` | the release binary is signed; from source, `make vz` signs it ad hoc |
| e2fsprogs (`brew install e2fsprogs`) | every microVM carries an ext4 overlay; images are inspected with `debugfs` |
| A Linux **arm64** daemon to build images on | macOS cannot build images (root, loop devices, chroot); they are copied over |

## 1. Install

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
brew install e2fsprogs
kling up -check
```

On darwin/arm64 the installer places `kling` and `kling-vz` side by side (the daemon looks
for `kling-vz` next to `kling`, then on the PATH; `KLING_VMM=/path` overrides). `kling up
-check` verifies all of the above and says what to type for each miss. From a checkout:
`make install && make vz`.

## 2. Start the daemon

By hand, in a terminal:

```sh
kling daemon
```

Or at login, with a launchd agent — [`docs/mac.md`](../mac.md#arrancar-el-daemon-al-iniciar-sesión-launchd)
has the plist (absolute paths: launchd expands neither `~` nor `$HOME`):

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.kindling.daemon.plist
kling status -v          # backend: vz · arch: arm64
```

Data lives in `~/Library/Application Support/kindling`, the socket at
`…/kindling/kling.sock`, and the daemon runs as you. The backend is a config key:
`kling config set daemon.vmm vz` (empty means the platform's: `vz` on macOS).

## 3. Bring an image

Images are built on Linux and streamed over, kernel and base layer included; the target
verifies the sha256 and refuses an image of another architecture:

```sh
kling image copy min -from ssh://user@linux-arm64            # the minimal Alpine base
kling image copy toolchain -from ssh://user@linux-arm64      # node, npm, python3, pip (for kling try)
kling image ls
```

If an active context points at the Linux host, say where to put it: `-to
"unix://$HOME/Library/Application Support/kindling/kling.sock"`, or `kling context use -`.
The source can be the Lima VM of [`docs/mac-arm64.md`](../mac-arm64.md), an arm64 server or
any Linux arm64 with KVM.

## 4. Use it

```sh
kling try -- uname -a
kling run -name demo -image min
kling ps                      # running  127.0.0.1:61234  — loopback forwards instead of an IP
kling freeze demo && kling thaw demo
```

On the Mac there are no network namespaces: every guest has the same IP inside its own
user-space network, and the host reaches it through **loopback ports** its helper opens
(`forwards` in `kling inspect`). `exec`, `shell`, the daemon's proxy and the schedulers
already use them. Egress policies (`none`, `internet`, `allowlist`) have the same meaning.

Measured on an M4 ([`docs/vz-mac-prototipo.md`](../vz-mac-prototipo.md)): restores in
121–159 ms, a cold boot in 266–317 ms, and a VON replica answering its first token ~0.8 s
after `run -from` at ~140 tok/s ([`docs/von.md`](../von.md)).

## 5. MCP servers and models on the Mac

```sh
kling plugin install mcp
kling mcp add <server> -bundle           # -bundle: one file instead of 1205, much faster cold start on arm64
kling connect -all -install claude-code
kling mcp serve -keepwarm 2              # keep the two most used services awake: the first call skips the cold start
```

For VON models, build with `-build-only` on the Linux daemon and copy the image; `kling ai
model add` then makes the template here.

## What is different from Linux (and will stay so)

| | Linux (Firecracker) | macOS (vz) |
|---|---|---|
| memory of a restore | shares the golden's `mem.file` copy-on-write | **~350 MiB per VM**: the framework copies the state into memory. Density is much lower |
| building images, `image put`, `mcp refresh-bridge` | yes | **no**: copy them from Linux |
| CPU ceiling (`-cpu-pct`) | cgroup v2 | not applied |
| jailer, privilege drop | yes | no: isolation is Apple's helper process per VM |
| parallel boots | 2 nested, cores/2 on metal | 4 (`KLING_MAX_PARALLEL_BOOT`) |
| admission | PSI memory pressure | `kern.memorystatus_level` below 15 % free → 507 |
| `squeeze` | reclaims what the guest reports free | blind: only on restored machines |
| shared folders (`-share`) | copy or live, 16 MiB/s cap | copy with Homebrew's `mke2fs`; live from APFS, case-insensitive by default |

Good for development and evaluation on the machine you have; for many replicas or real
load, a Linux host with KVM. The full table with the reasons: [`docs/mac.md`](../mac.md#límites-frente-a-linux).
