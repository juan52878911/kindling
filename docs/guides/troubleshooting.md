# Troubleshooting with `kling doctor`

Start every diagnosis the same way:

```sh
kling doctor          # runtime, daemon, versions, extensions, completion: ✓ / ! / ✗ with a fix per ✗
kling status -v       # which piece is up, gateway health, agents it can connect
```

`doctor` exits 1 only when something fails; warnings (`!`) do not count. `NO_COLOR=1` gives
ASCII marks. Every failed command also prints a `try:` line with the next command to run.

<p align="center"><img src="../img/demo-doctor.gif" alt="kling doctor and kling status" width="820"></p>

## The runtime block

| Line | What it means | Fix |
|---|---|---|
| `✗ /dev/kvm` | no KVM: not bare metal, or nested virtualization is off on the parent host | enable `cpu: host` / nested virt on the hypervisor; on a Mac use the `vz` backend ([macOS quickstart](quickstart-macos.md)) |
| `✗ firecracker` | the VMM is not installed | `scripts/20-install-firecracker.sh`, or the command `kling up` prints |
| `✗ nft (nftables)` | microVMs would boot **with no network** | install `nftables`; this one costs an afternoon if it slips through |
| `✗ user kindling` | Firecracker would run **as root** | the `useradd` line `kling up` prints |
| `✗ iptables` / `ip` | egress rules and namespaces cannot be set up | install `iptables` and `iproute2` |

`kling up` prints the privileged commands for all of these instead of running them; run
them with sudo and `kling up` again. `kling up -check` only checks.

## The daemon block

- **`✗ daemon` / "does not answer"**: is it running? Locally `systemctl status kling`
  (Linux) or `launchctl print gui/$(id -u)/dev.kindling.daemon` (macOS). Remotely, try
  `ssh host kling version` by hand: if SSH works but the socket is refused, the unit is down
  or your SSH user is not the one it hands the socket to. Check which endpoint you are on:
  `kling context ls`; precedence is `-H` > `$KLING_HOST` > active context > local socket.
- **`✗ versions`**: CLI and daemon differ. All binaries of one release are compatible with
  each other; update the older side (`install.sh --tag vX.Y.Z`, or `make deploy`). A 0.14
  CLI understands a 0.13 daemon, but a 0.13 reader does not know the `frozen` state.
- **`at rest: NOT encrypted`** in `status -v` is information, not a failure: snapshots hold
  guest memory in clear unless `$KLING_ROOT` is on dm-crypt ([`docs/cifrado.md`](../cifrado.md)).

## Extensions and completion

- **`✗ extension <name>`**: the manifest does not validate or asks for a `min_kling` above
  your core. `kling plugin ls` shows the path and sha256; reinstall from the matching
  release (`kling plugin install <name>`).
- **A command "does not exist" after installing an extension**: reload completion and the
  shell (`source <(kling completion zsh)`), and check the extensions directory is the one
  `kling plugin ls` prints (`$KLING_PLUGIN_PATH`, `~/.local/share/kling/plugins`).
- **`! completion not loaded`**: `kling completion install` writes the script and prints the
  line for your rc. Harmless otherwise.

## Machines

- **`kling run` fails with "not enough memory"**: the daemon refuses deterministically
  before an OOM (it keeps a reserve for the host and squeezes idle guests first). Free RAM
  with `kling freeze <ref>` or `kling machine squeeze <ref>`, or lower `-mem`.
- **A machine is `failed`**: the watchdog saw its VMM disappear. `kling logs <ref>` has the
  serial console; `kling events` the daemon's view. `kling rm` it and run again.
- **`save` refuses: "guest is not serving"**: the snapshot would restore and never answer.
  Wait longer (`-wait 3m`), check `kling logs`, or `-force` if you know why.
- **No network inside**: the default egress is `none`. `-egress internet` never reaches
  private ranges; `-egress allowlist -allow a.com,b.org` only those domains.
- **A volume refuses to mount or delete**: one writer or many readers, enforced on the
  boot path; the error names the machine that holds it and the `-volume name:ro` that would
  work.

## MCP services

```sh
kling mcp health                # probes every service and records the result
kling mcp verify <svc> -deep    # exercises it for real: a tool call and the guest's DNS
kling mcp heal -dry-run         # what a host reboot (TSC) invalidated; heal rebuilds only that
```

- **A service works for hours and then times out after a reboot**: golden snapshots are
  tied to the host's TSC; `kling mcp heal` rebuilds exactly those.
- **The agent shows the tool but calls fail with "expected array, received object"**: type
  repair handles the known shapes and logs every repair; if a server still rejects the
  arguments, the gateway logs what it sent.
- **`kling connect` succeeds but the agent cannot reach the gateway**: `gateway.url` must be
  the address *agents* use, not the listen address: `kling config set gateway.url http://host:8080`.
- **First call on a Mac takes ~16 s**: nested cold start. `kling mcp add -bundle`,
  `-cpu-pct 100` on import and `kling mcp serve -keepwarm N` bring it down ([`docs/mac-arm64.md`](../mac-arm64.md)).

## macOS (vz)

- **`kling up -check` says `kling-vz` is missing or unsigned**: `make vz` builds and signs
  it (ad-hoc signature with the `com.apple.security.virtualization` entitlement); the release
  ships `kling-vz-darwin-arm64` signed. It must sit next to `kling` or on the PATH.
- **`mkfs.ext4`/`debugfs` not found**: `brew install e2fsprogs` (keg-only is fine; kling
  looks in `/opt/homebrew/opt/e2fsprogs`).
- **`kling image build` returns 501**: images are not built on macOS. Build on a Linux
  arm64 daemon and `kling image copy <name> -from ssh://…`. The source must be arm64.
- **Restores fail under load**: the host's memory pressure (jetsam). Fewer parallel boots
  (`KLING_MAX_PARALLEL_BOOT`), fewer replicas; each restore costs ~350 MiB.

## Still stuck

Open an issue with the output of `kling doctor` and `kling status -v`, the command you ran
and what it printed: [new issue](https://github.com/juan52878911/kindling/issues/new/choose).
The field notes in [`docs/hallazgos.md`](../hallazgos.md) collect the things that took hours
to figure out once.
