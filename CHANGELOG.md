# Changelog

Notable changes to kindling: the core, `kling-vz` and the extensions in `ext/`.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), one line per change.
Binaries for every release are on the [Releases](https://github.com/juan52878911/kindling/releases) page.

## [Unreleased]

### Added

- macOS: `kling doctor` warns when the kernel or `min.ext4` that `kling up` downloaded come from another release, since `kling up` never replaces them.
- `kling-mcp gateway -max-sessions` caps the MCP sessions a tenant has open at once (default 256; without named tokens every client shares the `default` tenant).
- `kling sandbox fork -name` (and `name` in `POST /sandboxes/{ref}/fork`) names the copy; only with `-n 1`, and a taken name fails before the original is paused.
- Homebrew tap: `brew install juan52878911/kindling/kling`. Each final release updates it (release candidates don't); the release workflow needs a `HOMEBREW_TAP_TOKEN` secret for that.
- `kling machine credential -type sigv4` and `kling template credential -type sigv4`: hand an S3 (or R2, GCS) key to the credential proxy, which signs the guest's requests again with it; the guest only sees a placeholder access key id. Also on macOS, with a kling-vz or kling-hv from this release.

### Fixed

- MCP gateway: pruning session owners no longer counts as use, so direct sessions expire and their microVMs freeze again; a tenant could end up stuck at its session cap.
- MCP gateway `/mcp/_all`: with the primary full, a conversation keeps using the replica it scaled out to instead of starting a new one (and losing its state) on every `tools/call`.
- MCP gateway: closing or reaping a `/mcp/_all` conversation closes its bridge sessions instead of leaving an orphaned MCP process per service.
- MCP gateway: a 404 from the bridge to a session's POST forgets the session, so it no longer counts against its instance and forces replicas.
- MCP gateway with several hosts: `/mcp/_all` applies the tenant's in-flight request quota.
- kling-bridge: requests a stdio MCP server sends mid-call (sampling, elicitation, roots) now reach the client over the call's SSE response or the session's GET stream, instead of being dropped or returned as the call's result.
- kling-bridge: a session whose MCP server died answers 404 and is forgotten, instead of a 502 on every request.
- kling-bridge: JSON bodies with line breaks are compacted before being written to a stdio server.
- kling-bridge: the shared HTTP MCP server and the shared Chromium are restarted when they die, instead of failing until `/reset`; in http mode the server now gets the MMDS secrets.
- Linux: after a firewall reload the daemon also restores the FORWARD rules and the `KL-ACCESO` rejects, waits for the xtables lock, and no longer takes a failed check for a missing rule.
- Gateway scale-out bursts: when a replica that sessions were waiting for fails to start (including a network timeout), they get its error instead of each retrying and starting another failing replica; a panic while building no longer leaves the replica counted forever.
- Postgres credential proxy: after a server rejects protocol 3.2, it is remembered per upstream for 10 minutes, so later connections dial once with 3.0 instead of twice.
- kling-hv: a snapshot whose vCPU registers could not all be read now fails instead of saving a machine that restores wrong.
- kling-hv: the frozen RAM file is cloned in a private directory and moved into place already 0600, so it is never readable with the golden image's mode.
- kling-hv: a snapshot that fails after writing the memory file no longer leaves that file behind.
- kling-hv: an interrupt line the GIC rejects is retried on the next vCPU exit, instead of waiting for another access from a device the guest is no longer touching.
- kling-hv: the vsock thread no longer spins at 100% when the host has data and the guest has no receive buffers.
- kling-hv: a vsock connection the host closed while the guest gave it no credit gives up its slot, so 64 of them no longer leave the VM without exec.
- kling-hv: a FLUSH to a read-only disk no longer issues `F_FULLFSYNC`.
- kling-hv: releasing balloon pages the guest inflates is rate-limited like the free page report, so one notify can no longer hold the balloon lock long enough to make a pause time out.
- Thaw: at most 4 memory preloads run at once, and a failed thaw stops its preload; in a storm of 16 thaws, preloads now read 256 MiB from disk instead of 1.4 GiB.
- Daemon restart: re-adopting a paused VMM resumes it and checks its dump seal without holding the manager lock.
- Daemon restart: a machine re-adopted in the middle of a thaw now gets the clock and entropy resync the thaw never sent.
- Linux: thawing a warm machine no longer scans all of `/proc`; it checks the known PID and the VMM socket first (0.64 ms to 0.02 ms per thaw with ~110 processes).
- `run -from` and fork: a copy whose guest agent fails the resync is retried once and then not handed over, instead of running with the snapshot's clock and random state.
- The memory of snapshots known to have no guest agent drops expired entries instead of growing with every fork.
- An extension that prints its manifest and exits 0 is no longer rejected (`WaitDelay expired`) because another process still held its output open, such as a child kling was starting at the same moment.
- `kling db branch` names a branch copy forked from its parent like one started from the golden (`db-<repo>-<branch>`), so two runs at once can't leave two copies; with an older kling it falls back to the daemon's name.
- Postgres credential proxy: a guest asking for protocol 3.2 now connects through pgbouncer older than 1.25, which rejects 3.2 with `08P01` instead of negotiating; the proxy retries once with 3.0. On macOS, with a kling-vz or kling-hv from this release.
- The kindling-operator container image builds again; it was not published for v1.0.0-rc.2.
- Built with Go 1.27.4, which fixes GO-2026-6859 (an HTTP/1 server crash in `net/http`).
- arm64 guest kernel: on an Apple M4 under kling-vz it no longer advertises `sve2`, `svei8mm` and `svebf16` without `sve`, so OpenSSL (and Python's `cryptography`) no longer die with `Illegal instruction`; SVE stays on hosts that have it. The kernel builder now applies `scripts/builders/kernel/patches/` (#183).
- `scripts/e2e-mac-release.sh` publishes `failure`, not an earlier run's `success`, when a test fails.
- The e2e-mac workflow fails when a manual run's `parts` names no test, instead of passing.
- MCP gateway: a burst of simultaneous sessions starts ceil(M / sessions per replica) replicas and waits for them, instead of one replica per session up to the cap and 503 for the rest (#318).
- `-type sigv4`: temporary STS keys (`ASIA…`) are rejected with a clear error instead of failing on every request.
- Credential proxy, SigV4: objects stored with a `Content-Encoding` (gzip, br…) reach the guest byte for byte instead of decompressed or as a 502.
- Credential proxy, SigV4: a body over 1 MiB without `x-amz-content-sha256` gets a 413 that says why.
- Credential proxy, SigV4: a presigned URL only signs the headers that are still sent, and an aws-chunked trailer is capped at 32 lines.
- A commit cut by a daemon restart no longer gets a machine started or thawed afterwards killed on the next restart.
- A pause that times out and whose resume the VMM rejects leaves the machine running instead of failing it ten minutes later.
- Pre-release versions compare as semver (`1.0.0-rc.2` < `1.0.0`, `rc.2` < `rc.10`): a release candidate no longer meets `min_kling 1.0.0`, and `kling plugin install` without `@tag` works on one.
- `kling upgrade` without `-tag` on a release candidate no longer downloads an older latest stable release only to refuse it; it says so and points to `-tag`.
- `go install …@v1.0.0-rc.N` reports its version instead of `dev`.
- MCP gateway: one SSE stream per session; the stream GET no longer wakes an instance or keeps an isolated session alive.
- MCP gateway: `/mcp/_all` only opens a session on `initialize`, and unknown service names no longer enter the saved popularity.
- kling-hv: a vsock connection whose control packet waited behind a busy one no longer stalls until something else wakes the device thread (a new dial could time out).
- kling-hv: when a net or vsock device thread stops, `/kling/stats` reports it in `devices_dead` instead of the VM staying `running` silently.
- kling-hv: every interrupt line update checks the GIC result; the UART no longer records a line as raised when it was not, and failures are logged.
- A restored copy of a golden snapshot frees the slots of commands and shells left running in it, even when their connection never closes.
- `kling db clone` reports how many text, JSON, XML, bytea or array columns without a rule were copied unchecked (`-strict` blocks them).
- `kling db rotate -resume` finishes a rotation that died after changing the database; `connect` and `sql` warn about it.
- `kling image refresh` and `kling image put` refuse (409) to change an image a template was saved from, and say how to rebuild it; images already up to date still report so.
- `kling mcp import -force` (and `kling mcp heal`) replace the template instead of deleting it first, so its template credentials survive and a failed import keeps the old one.
- Without a copy-on-write store, a machine's disk file cap is recorded once instead of being re-measured after each thaw or resume, and the watcher only stops the run it measured.
- The usage of read-write shares is saved every 2 s while the machine runs, so a daemon killed without draining no longer hands that quota back.
- `install.sh --skip-kling`: a prerelease kling (1.0.0-rc.2) no longer meets an extension's `min_kling` 1.0.0.
- On Linux, a failed probe of the e2fsprogs jail is retried after a minute instead of leaving e2fsck without a chroot until the daemon restarts.
- The example operator Deployment runs as non-root with a read-only root filesystem, no capabilities and no privilege escalation, and its image's base images are pinned by digest.
- A template is verified again on its next restore if one of its files changed while its integrity was being checked.
- The signature migration at daemon start retries on the next start when a template's meta.json could not be read, instead of marking the host as migrated.
- `kling volume rm` refuses (409) a volume that a stopped machine still has attached.
- The overlay watcher no longer wakes every second when no machine is running.
- rw shares: saving a file atomically (write a temporary file, rename it over the old one) gives the replaced file's bytes back to the quota, so editors, `git` and `npm` no longer end up in a false `ENOSPC`.
- rw shares: creating a file that already exists without `O_EXCL` no longer changes its mode and owner.
- Linux: each machine keeps at most 4096 open connections, so one guest can no longer fill the host's conntrack table; the daemon warns when `nf_conntrack_max` is too low for a few machines at that limit.
- Linux: listing the host's addresses (three times per create, once under the network lock) takes one netlink dump instead of one per interface: 155 ms to 6.7 ms with 500 interfaces.
- macOS: thawing a machine with credentials launches `kling-credproxy` while the VMM starts and no longer repeats the whole hand-over after loading; on an M4 the daemon's thaw goes from 47–49 to 41 ms (median), and its credential part has its own `creds` wake phase.
- macOS: a thaw whose credentials can't be handed over before loading (an `-upstream` name that does not resolve, a missing `kling-credproxy`) no longer fails; the machine wakes up without them and the credential domains fail closed, as when the later hand-over fails.
- Daemon shutdown cancels an image snapshot preparation and waits for it to remove its `kling-imgprep-*` machine, which used to survive with its VMM; it also logs how many machines keep their VMM running.
- macOS: `credproxy.log` is rotated like the console log.
- macOS: replacing a `kling-credproxy` that refuses this daemon no longer leaves the old one running with its keys when it closes the connection before the daemon can tell who it is.
- Removing a template forgets its integrity verdict.
- Shares: a FIFO on the host where a directory is expected no longer hangs listing, renaming or watching it, nor stopping or freezing the machine.
- Shares: when watching by polling, a directory without a snapshot is reported only when it changes, not every 2 s.
- Shares on Linux: two names of the same file are both watched; unwatching one no longer silences the other.
- Shares: change notices to a guest that does not read no longer pile up goroutines (at most one write waits), and on macOS one session watches at most 1024 paths so it cannot use up the daemon's 4096.
- Shares: a size close to the 64-bit maximum no longer overflows the quota check; it gets ENOSPC.
- kling compose: `up` no longer removes a machine of another project whose name collides (project `a` service `b-web` vs project `a-b` service `web`), and `up` and `down -v` refuse to go on when names collide.
- Graph health checks wait at least 100 ms between attempts, so a tiny `start_interval_ms` no longer floods the guest with execs.
- kling compose: a healthcheck over the daemon limits (Docker allows `retries: 120`) is capped with a warning instead of failing the whole compose.
- kling compose up: images are imported before the running project is removed, so a failed pull leaves it up; only images that need `-replace` wait until after.
- kling compose up: at most 8 imports and 4 template machines at a time, and the first failure stops the rest.

### Security

- `install.sh` requires a signed release when it picks the latest one, even a 0.x; an unsigned 0.x installs only when asked for with `--tag`. It runs only once fully downloaded, and curl only talks https.
- Release workflow: the release job only downloads the asset artifacts by name, and the Homebrew tap job checks the `SHA256SUMS` signature before trusting it.
- Linux: the host drop of new connections to a machine's agent from outside the host (LAN, bridged containers) is now in mangle PREROUTING, ahead of the FORWARD accepts Docker, ufw or libvirt insert.
- Linux: if the host firewall rules can't be installed (for example an iptables without `--suppl-groups`), machines don't get a network until they are; the daemon retries before each start instead of carrying on with a warning. Without `--suppl-groups`, only the primary group is allowed.
- Linux: a root daemon no longer lets members of group 0 reach the machines' agent, and drops its own stale permissions from `KL-ACCESO` when the socket owner changes.
- macOS: the credential proxy runs in its own sandboxed, hardened-runtime process per machine (`kling-credproxy`, installed next to `kling-vz`), so the real keys are no longer in the process that runs the guest's network stack; with an older `kling-vz` or `kling-hv` the daemon keeps the previous model (#80).
- With an authz policy, `host_ports` and `nested` are admin-only for new machines, like volumes and shares.
- With an authz policy, tenants can no longer set `upstream`, `upstream_tls` or `tls_server_name` on a credential.
- MCP gateway: `-tls-cert`/`-tls-key` serve HTTPS, and plain HTTP outside loopback is refused unless `-insecure-listen` is given. The packaged systemd unit passes `-insecure-listen` to keep working.
- The guest agent's host-only routes accept TCP only from the host side (the default gateway or the default route); a container on a bridge network inside the guest gets 403.
- `kling db`: with a password set, the Postgres client refuses a server that accepts the login without asking for it.
- `kling db clone` and `kling db slice` stop before masking when the source has triggers or rules set to `ENABLE ALWAYS` or `ENABLE REPLICA`.
- State backups (`.vN.bak`) are written 0600, and the state root, `machines/` and `images/` are no longer readable by other users (0750).
- `kling template rm` deletes the template's credentials; a new template with the same name no longer inherits them.
- macOS: connections to MMDS and to the credential proxy on the gateway count toward the guest's 512 TCP flows; past that they get a RST instead of holding VMM memory without a limit.
- macOS: DNS queries forwarded upstream carry a random transaction ID instead of the guest's, as on Linux.
- Rotating or removing a credential also rewrites the credential store's `.vN.bak` copies, so `kling upgrade -rollback` no longer brings back the old one.
- An interactive shell whose client stops reading is cut after 2 minutes, in the daemon and in the guest agent, instead of holding one of the guest's 8 shell slots.
- `kling-phone view -open` hands the browser a private redirect file instead of putting the wall's secret URL in the opener's argv.
- With an authz policy, publishing ports (`publish` on a new machine and `POST /machines/{ref}/publish`) is admin-only.
- An image builder that leaves a background process holding its output, or hits the 15-minute limit, no longer holds the build locks until the daemon restarts: its whole process group is killed, and only the last MiB of its output is kept.
- `GET /events` is capped at 256 streams (16 per tenant, 429 beyond) and cuts a client that stops reading after a minute.
- With an authz policy, sandbox, graph node and fork requests classify every field like `RunRequest` does; an unclassified one is admin-only, and tenants can't set a node's `machine_id` or `state`.
- `daemon.require_e2fs_jail on` makes e2fsck, resize2fs and debugfs fail instead of running without their full jail (default off).
- macOS: the daemon only launches `kling-credproxy` next to its own VMM binary, not next to whatever program answers on the machine's VMM socket.
- kling compose: variables are interpolated after parsing, per value, so a value with ` #`, `: ` or a newline no longer gets cut or adds keys; `image:` must be an image reference.
- kling compose up: service environment values reach `kling image import` through a temporary `-env-file` instead of its command line.
- The `.xz` decoder (Debian indexes and `.deb` packages) no longer panics on malformed filter properties or out-of-window distances, caps the dictionary at 64 MiB and allocates the window as it fills; a decompressed `Packages.xz` is capped at 1 GiB.
- OCI pulls reject manifests with more than 1024 layers or whose declared layer sizes overflow the size limit, before downloading any blob; the unpacked-size check in the oci builder no longer overflows either.
- Image layers with sparse files (`GNU.sparse.*`) are rejected when building the ext4 tree, as the reader of local `docker save` and OCI layout archives already did.

### Changed

- MCP gateway `/mcp/_all`: up to 16 idle connections per guest (was 2), so concurrent tool calls stop redialing, and tool results are written without being re-encoded.
- Linux: setting up a machine's network runs 11 processes instead of 23 (`ip -batch`, one `sysctl`, no teardown when there is nothing): 46 to 20 ms in the lab.
- `kling plugin install` streams the asset to disk while hashing it instead of holding it (up to 512 MiB) in memory.
- kling-hv: `KLING_HV_AVISOS=1` also logs vCPU exits, timer exits and trapped WFIs (measured on an M4: none trapped while idle, so the 2 ms WFI fallback never runs).
- Templates record and sign the digest of the image (and base layer) they were frozen over, and do not restore if it changed (`meta.json` schema 4; older klings refuse these templates).
- A machine's overlay size cap is set when its disk is created, and a machine from an older daemon gets it from its disk size or its template instead of from what its overlay measures at the watcher's first look.
- Image file uploads from the daemon's lib directory (`from_host`) copy the file opened once and checked, instead of checking a path and reopening it.
- Live shares: the directory listings kept to report host changes are capped per session and shared between names of the same directory, so a guest can no longer grow the daemon's heap through a rw share.
- Machines that mount the same host folder rw (graph `share` edges, or two `-share` of one directory) now spend one quota instead of one each.
- The Firecracker balloon statistics are read with a 64 KiB limit.
- Linux: only root, the daemon and whoever can use its socket can open connections from the host to a machine's guest agent (port 8080) or to the host side of its link; other local users get a refusal, and the agent is closed to forwarded traffic from outside the host.
- Sandbox gateway: a sandbox can't ask for more `mem_mib` or `vcpus` than its tenant's limit (`KLING_SANDBOX_TENANTS` fields 5 and 6; otherwise `-max-mem-mib` 4096 and `-max-vcpus` 4).
- `kling sbx` and kindling-operator no longer send their Bearer token along a redirect to another origin.
- Sandbox gateway: concurrent creations can no longer exceed a tenant's `max_sandboxes` when one finishes while another is counting.
- kindling-operator: a create whose answer is lost no longer leaves a second sandbox; each one carries the object's UID (`k8s-uid`) and is adopted, and deleting an object whose status was never written removes its sandbox.
- kindling-operator: the projected ServiceAccount token is re-read every minute and after a 401, so a rotated token no longer means 401s until the pod restarts.
- `kling db ask`: a number glued to a keyword (`1INTO`) is rejected instead of hiding the keyword from the filter.
- MCP gateway: `/mcp/<service>` (and the multi-host router's `/mcp/_all`) only opens a session on `initialize`, and a tenant can hold at most 256 open sessions (429 beyond that).
- MCP gateway with `-no-auth`: requests whose `Host` or `Origin` is not loopback get a 403, so a web page can't reach it through DNS rebinding or a cross-origin POST.
- MCP gateway: the guest's answer to the DELETE of an isolated session is capped at 64 KiB and 30 s.
- MCP gateway: a slow `initialize` in one `/mcp/_all` conversation no longer blocks every `tools/call` to that service from other conversations (the lock is per conversation now), and connecting to a guest times out after 3 s.
- rw shares with a quota: writes that grow different files no longer wait for each other; only writes that grow the same file are serialized.
- Machines with shares started through the API (gateway, sandbox) get at least 256 MiB/s of network, as `kling run -share` already did.

## [1.0.0-rc.2] - 2026-11-10

### Upgrading

- Upgrade with `kling upgrade`; from this release on it only installs signed releases.
- Run `kling image refresh` (or rebuild) so images get the new guest agent and its fixes.
- kling-hv templates with 2+ vCPUs saved by older builds may not restore; rebuild them.
- The daemon re-signs v1 templates at startup; unsigned ones must be recreated, and goldens written now don't load in older klings (`docs/actualizar.md`).
- Linux with the jailer: create the `kindling` user in the `kvm` group first, or machines won't start.

### Added

- Signed releases: minisign `SHA256SUMS.minisig` and SLSA provenance, checked by `upgrade`, `plugin install` and `install.sh`.
- Stable API v1: error bodies with a stable `code`, contract goldens, documented exit codes, `docs/compatibilidad.md`.
- `kling version` and `status -v` show API, VMM and guest agent versions; `kling image ls` flags old agents.
- Image snapshots: `kling run -image` restores a saved snapshot instead of booting cold (`-fresh` opts out).
- macOS: `kling run -host-port` lets a guest reach chosen ports on the Mac's loopback.
- kling-hv: opt-in virtio-fs shares (`:virtiofs`), `-nested` on M3+, `fstrim` support and up to 64 vCPUs.
- The daemon squeezes and freezes idle machines under host memory pressure (`daemon.memory_pressure`).
- `/metrics` gains per-machine and daemon resource series; admins get `/debug/pprof/`.
- `kling db up` without a template, `kling db sql`, `kling try -image <docker ref>` and `try -json`.
- `-json` on lifecycle commands; `kling compose` nested interpolation, port ranges and `exec -i`; macOS volume `sync=` modes.

### Changed

- Templates must be signed: unsigned ones no longer restore (`daemon.require_signed off` opts out).
- Signed releases are required from 1.0 (release candidates included): `upgrade` (also `-from-dir`), `plugin install` and `install.sh` refuse unsigned ones without `-insecure`.
- `kling sbx gateway` listens on `127.0.0.1:8090` by default; off loopback it needs TLS or `-insecure-listen`.
- kling-sandbox reserves `kling.*` labels and `/v1/hosts` needs an admin; `kling phone view` serves under a random path.
- Admission: Linux jailer without a `kindling` user refuses machines; macOS caps declared memory at 4× RAM (`KLING_MEM_OVERCOMMIT`).
- `allow_domains` rejects wildcards, IP addresses and non-punycode names (`a.com` already covers subdomains).
- Linux: per-machine caps of 1000 new connections/s and 300 packets/s per destination IP (needs `xt_hashlimit`).
- `kling db clone`: the source must be reachable from where it runs; dumps capped by `-max-dump` (8G) and one hour.
- New limits: rw shares 32 GiB / 1M entries (`daemon.share_max_*`), 64 labels or 4 KiB, 256 `allow_domains`.
- `kling plugin install x@vOLD` refuses a downgrade; `kling mcp import/add/link` refuse dotted service names.
- API: status codes normalised before the freeze; 404/405 and gateway errors are JSON with `code`; `POST /graphs` rejects unknown fields.
- CLI: usage errors exit 2, notes go to stderr, `doctor -json` has `checks[]` instead of `runtime_report`, `KLING_SOCKET` ranks after contexts.
- Guests: Debian images boot the Go init, the agent refuses callers inside the guest, new volumes have no `lost+found`.
- 1.x surface: Go API is `pkg/api`, `pkg/plugin`, `pkg/config` (the rest of `pkg/` is internal); on-disk formats are versioned.
- kling-hv is stable, and much faster on the Mac (thaw p90 2 s → 39 ms).

### Removed

- CLI aliases deprecated since v0.18 (`rmi`, `images`, `volumes`, `info`, kling-mcp `add`, `gateway`…); they exit 2 naming the new command.
- `kling run -cpu` (use `-cpu-pct`) and `-vcpus` in `kling ai chispa deploy` and `kling ai retrain` (use `-cpus`).
- `-token` on `kling mcp connect` and `migrate`: use `KLING_GATEWAY_TOKEN`, `-token-file` or `gateway.token`.

### Security

- Linux: a machine reaches only its own host-side services; host IPs, floods and conntrack exhaustion are blocked.
- Linux allowlist DNS hardened against spoofed answers; seeded IPs expire; shared-hosting domains get a warning.
- Linux jailer hardened: own PID namespace, verified API socket peer, read-only volumes really read-only.
- `e2fsck`, `resize2fs` and `debugfs` run confined, never with the daemon's privileges.
- macOS: tighter VMM sandbox profiles, port forwards bound to their machine, minimal VMM environment.
- Snapshot signature v2 also covers labels, limits and the memory dump.
- Live shares: swapped symlinks and kindling's own data root can no longer be shared; rw shares have quotas.
- A guest can no longer redirect daemon or gateway requests, send terminal escapes, or call its agent from inside.
- kling-hv: a guest can no longer stall freeze, commit or squeeze, or crash the VMM with malformed device input.
- Multi-tenant: kling-mcp sessions bound to their tenant; kling-sandbox claims race-free, metrics and host errors scoped.
- Internal: `kling db` clone/auth checks, root `kling upgrade` and extension discovery hardened; SECURITY.md documents known limits.

### Fixed

- Freeze, thaw and daemon restart no longer leave stale dumps, double VMMs or machines stuck `warm`/`failed`.
- A dead VMM is noticed at once and reported with its last console line.
- kling-hv: multi-vCPU restores, CPU hotplug, SVE on M4, KASLR and network/vsock stalls.
- Guest agent: the vsock listener survives `EINTR`; a busy agent answers 429 `guest_busy` (needs the new agent).
- `kling run -e` variables reach a debian image's `service` (rebuild the image).
- `exec` silent for over 5 minutes no longer times out; an interrupted `exec` says why.
- MCP gateway and scheduler: no leaked or prematurely frozen instances; sessions survive in-flight calls.
- kling-sandbox: one hung host no longer serialises claims; failed prewarmed sandboxes are replaced.
- OCI cache, builds, `state.json` and `kling upgrade` are crash- and disk-full-safe.
- Compose/graph YAML block scalars work; the credential proxy no longer passes truncated streams as complete.
- The release upgrade e2e handles prerelease tags and signed 1.x releases; v1.0.0-rc.1 was tagged but never published.
- Internal: docs commands checked by a test, wider macOS CI, `release.sh` fixes, fewer allocations and wakeups.

## [0.20.1] - 2026-10-28

The v0.20.0 tag never became a release: the new job that builds the guest kernel and the `min` image failed. This is the same code, so the release notes are those of 0.20.0.

### Fixed

- Release: `scripts/70-build-minimal-image.sh` and `packaging/homebrew/formula.sh` (and a few other scripts) were stored without the executable bit, so the release could not build the `min` image nor the Homebrew formula; CI now fails on any tracked `.sh` that is not executable (except sourced libraries).
- `kling upgrade` downloads from `KLING_UPGRADE_REPO` when set (a fork or a local mirror; https only, still checked against the release's `SHA256SUMS`).
- CI: `e2e-linux.yml` runs `90-e2e.sh`, `e2e-diff-freeze.sh` and `94-e2e-upgrade.sh` with real microVMs on KVM runners (nightly, on demand and on PRs that touch them), and a `lint` job runs govulncheck, staticcheck, shellcheck and `scripts/apidiff.sh` (the public Go API against the previous tag).
- `94-e2e-upgrade.sh` and `95-e2e-upgrade-mac.sh` upgrade from the previous published release (no longer pinned to v0.17), accept any state schema, and 94 downloads the new release over https from a local server instead of `-from-dir -insecure`; 95 covers kling-hv (`VMM=hv`).

### Changed

- `kling upgrade -rollback` refuses, before stopping anything, when the previous kling could not read what stays on disk (a format the new one wrote without a migration copy); `-force` rolls back anyway.
- `kling-hv -snapshot-formats` also prints its memory-file state version (`KLNGHV`), and a rollback on the Mac refuses to leave a frozen machine the previous kling-hv could not wake.
- Release tags with a hyphen (`v1.0.0-rc.1`) are published as prereleases, so `kling upgrade` and `install.sh` do not take them as the latest, and they do not move the operator image's `:latest`.
- `scripts/release.sh` tags the commit it checked, requires green CI on it and waits for that tag's own workflow run; release notes link the tag's `install.sh`, not main's.

## [0.20.0] - 2026-10-28

`compose up -d` with ports published by the daemon and idle projects that freeze on their own, live shares about 4x faster on the Mac, and fixes from a security and durability audit. Images and templates built before this release keep their old guest agent and today's share speed; rebuild them (`compose up -fresh`, `image import -replace`) to get the faster shares. Upgrade with `kling upgrade`.

### Added

- Releases ship the guest kernel and the `min` image for amd64 and arm64 (`vmlinux-<arch>.zst`, `min-<arch>.ext4.zst`), and `kling up` on a Mac downloads them when missing, checked against the release's `SHA256SUMS`: no Linux host needed for a first `kling run -image min`.
- Homebrew: each release attaches a `kling.rb` formula (darwin-arm64 and linux, depends on e2fsprogs, with a service that runs the daemon); `kling upgrade` refuses binaries in Homebrew's Cellar and points to `brew upgrade kling`.
- `/metrics`: `kling_machine_footprint_mib` (macOS), `kling_machine_cpu_seconds_total{core="p|e"}` (Apple Silicon; `all` on Linux), `kling_machine_energy_joules_total` (macOS), `kling_machine_state_seconds` and `kling_host_mem_available_mib`.
- `kling top` on macOS shows each machine's memory and %CPU (and the share on P-cores) instead of "no /proc".
- `kling doctor` warns about machines stuck mid-operation: a freeze that could not resume, or a removed machine whose VMM survived SIGKILL.
- `scripts/94-energia-mac.sh`: a reproducible energy bench on the Mac (no sudo), JSON in `docs/bench-data/`.
- The daemon publishes ports on 127.0.0.1 (`POST/DELETE /machines/{ref}/publish`, `GET /publish`, `RunRequest.Publish`, capability `publish`): they survive the CLI and a daemon restart, wake a frozen machine on the first connection, and go away with the machine. `kling run -p` and `kling compose up` use it; `kling ps` shows `PORTS`. UDP on Linux only.
- `kling compose up -d`, `compose ps`, `logs [-f] [service]`, `exec`, `stop`/`start` (freeze/thaw), `restart`, `down -v`; `-f`/`-p` before the subcommand and Docker's long flags (`--detach`, `--volumes`, `--follow`...).
- `daemon.idle_freeze` (default 10m on macOS, 0 = never): a project with published ports and no connections for that long is frozen; the next connection wakes it. `kling.idle=keep` opts out.
- Compose reads `.env` next to the file, `$VAR` without braces, and fails on `${VAR:?msg}` like Docker. `ports: "80"` takes a free host port.
- `docs/docker-a-kindling.md`: Docker and Compose commands and keys, one by one; compose warnings link to it.

### Changed

- The per-microVM network limit is no longer a fixed 16 MiB/s: `daemon.net_mibps` (or `KLING_NET_MIBPS`) sets the default and `RunRequest.net_mibps` asks for another; `kling run -share` and compose ask for 256. Reading a shared folder in the lab went from 21 to ~167 MiB/s (p50, n=5); writes stay at ~12 MiB/s (not the limiter).
- Live shares are faster: the agent acknowledges writes once it has the data and sends contiguous ones together (errors come back on the next write, `fsync` or `close`), reads ahead 4 MiB instead of 128 KiB, caches symlink targets, and the daemon allows 8 large operations in flight per session instead of 4. An older agent or daemon keeps the previous behaviour.

### Security

- Live shares: two relative symlinks that were each valid could add up to point outside the folder (`x -> a/up/../..` plus `a/up -> ..`). Targets must now be canonical (`..` only at the start), the link's directory may not be named through another link, and a concurrent `rename` can no longer race the check.
- Credential proxy: a 502 on a presigned SigV4 URL no longer echoes the outgoing URL (real access key id and signature); the real access key id and the signature the proxy emitted are redacted in S3 errors and `Authorization` echoes.

### Fixed

- `kling upgrade -from-dir` refuses a directory without `SHA256SUMS` unless you pass `-insecure`, and fsyncs the directory after swapping each binary.
- `internal/share` builds on 32-bit Linux (`linux/arm`, `386`): stat times are widened to int64.
- A dump or a golden is stamped with the VMM that launched the machine, not the daemon's: a `kling-vz` machine readopted by a daemon set to `hv` no longer looks restorable by `kling-hv`.
- Ctrl-C on `kling freeze` no longer kills the machine: the dump finishes, and a resume that fails is retried by the daemon instead of a SIGKILL.
- A thaw whose client left after the memory was loaded still resyncs the guest's clock and RNG.
- `kling rm` keeps the network and disk of a machine whose VMM survives SIGKILL until it exits.
- Leftover commit dumps (`.commit-mem.file`, the size of the RAM) are removed on daemon start and on stop.
- `POST /machines` answers 4xx to request errors (unknown image or snapshot, bad `on_ttl`, `disk_mib`, `mem_max_mib`) and lifecycle calls answer 500, not 400, to host errors.
- Battery mode resumes a machine when a request came in while it was being paused.
- A paused machine resumed after the Mac slept gets its clock resynced.
- On battery the daemon's watcher wakes less when no machine runs, on a fixed grid; dot-names in the root no longer count as `kling-vz` dumps for `daemon.vmm: auto`.
- A graph edge to a service that is not listening yet gets a reset at once (kling-vz/kling-hv) instead of an accepted connection that closes: on hv the test compose app answers in 0.25 s instead of 2.24 s (p50).
- Compose: an interrupted template prep no longer blocks the next `up` ("machine name is taken"); `up`/`down` refuse a project that belongs to another compose file, and two `up` of one project don't run at once.
- With `VolumeWait`, `NODE_PATH`/`PYTHONPATH` are computed after the volumes are mounted, for the service and for `exec`.
- Healthchecks run as the service's `user:`, not root (`ExecRequest.User`); `healthcheck: disable: true` wins over `test`.
- An exec, shell or file call that lands while the machine is being frozen (`daemon.idle_freeze`, or a freeze from another terminal) waits for the freeze and thaws it, instead of a 504 after 30 s or an EOF (hv, 15/15 raced execs answered; before, 1 in 3 failed).
- Compose: `PIN: 0123` keeps its leading zero; `0.0.0.0:` ports warn that they are published on 127.0.0.1 only.
- Live shares: recreating a file the host had just deleted (while the guest still had it cached) failed with `ENOENT`; the agent now answers `ESTALE` so the kernel looks it up again and creates it.
- Live shares: when the host's inotify queue overflows the daemon rescans every watched path and reports what changed, instead of dropping those changes; hitting the watch limit is logged once.
- kling-hv: pausing boosts the vCPU threads' QoS and re-kicks any vCPU that has not stopped after 1 ms (khv_pause p99 with 4 idle vCPUs on a loaded Mac: up to 104 ms before, 0.19 ms after).
- Durable writes on macOS: one F_FULLFSYNC per write instead of two (the temp file and the frozen state get an F_BARRIERFSYNC); Go's File.Sync already was F_FULLFSYNC.
- A freeze made by v0.8.0 or later with neither the in-progress mark nor the seal is no longer thawed as if it were a pre-seal freeze.
- kling-hv: a guest FLUSH on a persistent disk is now F_FULLFSYNC, like kling-vz's full sync mode (a plain fsync on macOS stays in the SSD cache).
- kling-hv: the disk thread works on its own copy of the queue, and queue size and addresses are ignored while the queue is ready (a hostile guest could write past guest RAM).

## [0.19.0] - 2026-10-28

macOS gets a second VMM, `kling-hv`, picked by the new default `daemon.vmm: auto`. A Mac that already has machines or goldens frozen with `kling-vz` stays on `kling-vz` until you switch with `daemon.vmm: hv`. Guest kernel changed: old goldens still restore, with a warning. Upgrade with `kling upgrade`.

### Added

- `kling-hv`: a macOS 15.2+ backend on Hypervisor.framework with its own VMM. Guest RAM is a mapped file, so copies share pages.
  - On an M4 (p50): thaw + exec 36 ms, `run -from` 40 ms, ~16 MiB per copy, 50 copies in 784 MiB. With `kling-vz` the same takes 176 ms and 218 ms and ~350 MiB per copy.
  - Devices: vsock, balloon, MTU 16K and EVENT_IDX.
  - CPU and energy: per-thread QoS, P/E-core CPU accounting and its own sandbox profile.
- `daemon.vmm: auto` (macOS default): the daemon picks hv when macOS is 15.2+ and `kling-hv` is signed and passes `--probe`; otherwise vz. `kling status -v` and `/info` say which and why.
- Releases ship `kling-hv-darwin-arm64`; `install.sh` and `kling upgrade` install it; `kling up -install` writes the launchd agent.
- `kling compose up [-f compose.yaml]`: a dev stack in the foreground.
  - Bind mounts become live shares, `ports:` are published on 127.0.0.1 and the services' output is joined.
  - Each service keeps a warm template, named volumes included: a repeated up of postgres with `pgdata` takes 0.36 s on hv.
  - Every service reaches every service on its networks; `service_healthy` runs the real healthcheck.
  - `command:`/`entrypoint:` apply per machine without re-importing; `platform:` and `logs -t` are supported.
- `kling run -p HOST:GUEST`, `run -platform`, and `RunRequest.Cmd`/`Entrypoint`/`VolumeWait`.
- amd64-only images on arm64 hosts: `image import -arch amd64` uses a pinned static qemu-user registered in binfmt_misc (opt-in, ~16x slower on CPU).
- Live shares see host edits at once (kqueue/inotify), accept relative symlinks inside the folder (`npm install` works), and move up to 1 MiB per operation.
- macOS energy:
  - battery mode (`daemon.battery_mode`): background QoS and pausing of idle machines;
  - pause before the Mac sleeps, and a clock resync after it wakes;
  - `kling-vz` idles at ~1 wakeup/s instead of ~65.
- Images and templates:
  - `image import -warm`, `-slim`, `-isolate`, `-init image`, `-ulimit`/`-sysctl`, and layered imports over an existing base;
  - `template pool -n N`;
  - `volume clone`.
- `kling db` engines: Valkey, MySQL 8.4/9, FerretDB (MongoDB), ClickHouse and Garage (S3); the credential proxy re-signs SigV4.
- Egress-blocked machines report what they tried to reach; `docs/linux-arm64.md`.

### Changed

- macOS:
  - the VMM sandbox profiles have no per-port rules (`sandbox_init` 53-69 → 9 ms in vz), and the host loopback is closed to the VMM;
  - the daemon publishes snapshots itself (no keeper process);
  - the vz CPU brake only runs with a ceiling.
- macOS networking:
  - MTU 16384 (×3 throughput) and a vsock agent channel;
  - bigger user-space TCP buffers.
- macOS CPU:
  - the CPU ceiling charges efficiency-core time at its performance-core worth (×12 → ×4-5 slowdown at 25 %);
  - boot runs uncapped.
- Shares may be mounted under `/etc/*` (except `/etc`, `/etc/kindling`, `/etc/kling`); imported images resolve bare graph names (`search graph`).
- Guest kernel: `BINFMT_MISC`, pinned 6.1.140 tarball, arm64 errata; guests boot with lower timer churn.
- A wrong-architecture image lists the platforms it has.
- `kling rm` answers once the VM is dead.
- Release binaries are built with Go 1.27.3; the minimum Go stays 1.24. The release workflow runs the full test suite and pins actions.
- `kling-vz` targets macOS 14.0 and `kling-hv` 15.2 (both were stamped with the SDK version, 26.0).

### Fixed

- Egress `none`/`allowlist` let conntrack-INVALID packets through the netns FORWARD and the graph/DNS/credential accepts.
- macOS:
  - `kling-vz` died with SIGSEGV after balloon use;
  - a restored copy whose vsock went silent hung every agent request; it now closes every connection and falls back to TCP, and a short caller deadline is no longer taken for a silent channel;
  - two concurrent `kling-hv` vsock dials could swap connections;
  - the first `exec` after a cold boot waited ~1.2 s for a lost SYN;
  - OpenSSL died with `Illegal instruction` on M4 guests.
- Live shares:
  - freezing no longer gives EIO to an in-flight write;
  - vsock-attached shares were refused;
  - absolute host symlink targets no longer reach the guest.
- `compose`/`from-compose`: `POSTGRES_USER: app` no longer becomes `app.graph`; `-fresh` really reimports; a failed earlier up no longer blocks the name.
- Durability:
  - freeze and save fsync before sealing;
  - a full CoW store no longer cuts freeze/thaw at 5 s;
  - concurrent volume creates and state writes no longer clash.
- Guest `/exec` no longer waits for background grandchildren.
- MCP gateway and `kling-bridge`: session and retry fixes (#187).
- `kling-sandbox`: tenant quota fixes.
- Daemon: status codes (400/404/409/413), `events` lines over 64 KiB, and 60 s timeouts on freeze, thaw and start.
- Image import:
  - long or NUL file names are rejected;
  - fragmented files no longer fail;
  - a corrupt ext4 superblock no longer panics.
- `kling rm|stop|freeze|thaw|pause a b c` keeps going after a failure.

## [0.18.0] - 2026-10-11

On-disk formats changed: upgrade with `kling upgrade`, which keeps a backup and rolls back on failure.

### Added

- Run Docker images: `kling image import` and `kling run -image`, without Docker or root (#167, #171)
- Private registries: `kling registry login` (#171)
- `kling upgrade`: update kindling in place, with automatic rollback (#138, #171)
- `kling start` boots a stopped machine again (#170)
- `kling run -e` gives a machine its environment at boot (#168)
- Template copies get their own random state and machine-id: `image import -on-restore` (#180, #184)
- Per-tenant quotas (#170)
- `kling db`: Postgres goldens with extensions, faster branches (#125, #143–#150)
- `kling phone`: Android phones in microVMs (#119, #120)
- `debian` image builder, also on macOS (#157)

### Changed

- Daemon state, templates and credential stores are versioned (#140, #141)
- macOS: older `kling-vz` can't restore new snapshots (#141)
- Frozen copies of a template thaw faster and use less disk (#167)
- Machines return unused memory to the host (#167, #170)
- Images build and unpack 2–3× faster (#172, #173)
- `kling stop` keeps the disk and drops the memory (#170)
- Short README; the full guide is `docs/guide.md` (#152)

### Fixed

- `kling run -ttl 0` means never; `run -from` warns about ignored flags (#182)
- Copies of a template reach the template's credentials (#182)
- `kling doctor` lists templates that can't be restored (#182)
- Freezes, saves and thaws check disk and memory first (#170)
- Docker images behave as they do in Docker (#170)
- Shared folders survive restarts and freezes (#142)
- Several race conditions in images, the gateway and networking (#170)

### Security

- Images are built by an unprivileged user (#168)
- Image downloads are verified and size-capped (#167, #171)
- The credential proxy never sends keys to another host (#133)
- macOS: each machine's `kling-vz` runs sandboxed (#131)
- No symlinks followed in jails or sockets; downloads pinned by sha256 (#129, #134, #139)
- Secrets stay out of the host process list (#155, #159)

## [0.17.0] - 2026-09-29

### Added

- `kling db`: disposable Postgres databases, one per microVM or git branch (#51)
- `kling db clone`, `diff`, `ask`, `rehearse`, `tenant-check`, `class` and `report` (#51, #99, #105, #109)
- MySQL, MariaDB, Redis and SQLite in `kling db` (#105, #109)
- `kling graph`: several microVMs with `link`, `depends`, `share` and `credential` edges (#51, #100, #109)
- Copy-on-write disk store: `run -from` no longer copies the whole golden disk (#51)
- Credential proxy for HTTP APIs and Postgres: keys never enter the guest (#47, #48, #49, #50)
- Per-session isolation for MCP services: one microVM per session (#52)
- Per-operation authorization on the daemon socket (#105)
- Android phones on kindling: K1 kernel, image-defined readiness, optional virtio-gpu (#101, #102, #103, #104, #106)
- Volume snapshots and rollback with `kling volume snapshot` (#49)

### Changed

- The daemon and `kling-vz` must be upgraded together (#100)
- The credential audit log moves out of the VMM's reach (#109)
- macOS: CPU cap uses short SIGSTOP pauses instead of pausing the VM (#108)
- Internal: CI triage example tweaks, e2e sections for `kling db` and graphs (#46, #100)

### Fixed

- The MCP gateway has a per-service replica cap (#48)
- `kling run -from` inherits the template's egress (#48)
- Two daemons on one host no longer wipe each other's network or subnet (#109)

### Security

- IPv6 is closed in every egress mode (#48)
- Per-credential method and path rules with `-allow-request` (#48)
- Per-session MMDS secrets removed; the session id was not a secret (#48)
- `kling image put` on Linux no longer writes outside the image (#105)
- Copy-on-write store: each jail sees only its own instance directory (#51)

## [0.16.0] - 2026-09-27

### Added

- macOS: `-cpu-pct` now applies with the `vz` backend (#45)

### Security

- A guest can no longer reach host services through the host's public IP (#45)
- `kling save` and `fork` refuse machines with injected secrets (#45)
- macOS: only your user reaches a sandbox's agent (#45)
- macOS: `kling-vz` runs inside a sandbox profile (#45)

## [0.15.0] - 2026-09-27

### Added

- `kling sandbox fork`: branch a running sandbox into N copies (#42)
- Minimal custom kernel and faster boot (#42)
- `LICENSE` (Apache-2.0) and `docs/benchmarks.md` (#42)

### Changed

- Jailer is mandatory by default on Linux (#42)
- `kling logs` returns at most 4 MiB (#42)
- `rm`/`stop`/`freeze` wait for a machine that is still starting (#42)

### Fixed

- Machine lifecycle, `kling save` locking and Firecracker dump/restore timeouts (#42)
- Bounded serial console, allowlist DNS resolver and shared-folder descriptors (#42)

### Security

- Path traversal through escaped URL names (#42)
- Frozen machines' memory is no longer world-readable (#42)
- A compromised VMM can no longer rewrite kernels, images or goldens (#42)

## [0.14.0] - 2026-09-26

### Added

- `kling` with no arguments shows where to start; one help system for every command (#41)
- Size and duration units: `-mem 512M`, `-ttl 10m` (#41)
- `-q` on every `ls`; `next:` hints after creating something (#41)
- Generic intent tasks in the AI gateway (#41)

### Changed

- New names: `save` (was `commit`), `template ls|inspect|rm`; old names stay as aliases (#41)
- Extensions get a namespace (manifest v2); MCP commands live under `kling mcp` (#41)
- `frozen` state shown in `ps`, `inspect`, `top` and the API (#41)
- The home automation demo is a separate program, `kindling-domotica` (#41)

## [0.13.0] - 2026-09-26

### Added

- `kling plugins install|ls|rm|enable|disable` (#40)
- `-json` on `sandbox ls`, `context ls`, `config show` and `version` (#40)
- Errors suggest the next step when it is known (#40)
- `scripts/install.sh --with mcp,sandbox` installs extensions (#40)

### Changed

- One repo: kling-mcp and kling-sandbox move to `ext/` and ship in the same release (#40)
- `ai`, `chispa` and `models` become built-in extensions (#40)

## [0.12.0] - 2026-09-25

### Added

- `kling ai serve`: AI gateway with Chispa and small LLMs, scaling to zero (#26)
- Chispa: tiny linear classifier, same bits on amd64 and arm64 (#22)
- `kling models`: small on-demand LLMs in microVMs (#25)
- Serverless Chispa: one task per frozen microVM (#31)
- `kling ai retrain`: Chispa learns from what slower layers resolved (#34)
- `kling pause`: paused level wakes in about 2 ms (#35)
- CI failure triage example (`examples/ci-triage`) (#36)
- Home automation demo (`examples/domotica`) (#27, #29, #33)

### Changed

- Waking a frozen machine drops from 152 to 27 ms (#35)

### Fixed

- The daemon no longer freezes a gateway instance mid-request (#23, #24)
- `max_replicas` is a hard limit; shrinking a layer no longer corrupts it (#37)

## [0.10.0] - 2026-09-23

### Added

- Shared folders: `kling run -share` with copy, read-only and live read-write modes (#21)

### Fixed

- The systemd daemon reads root's configuration (#21)

## [0.9.1] - 2026-09-23

### Fixed

- Guests resync clock and entropy after every restore (#20)
- Jailed machines are re-adopted after a daemon restart (#20)
- Scheduler `Bind` no longer overwrites a session pinned elsewhere (#20)

## [0.9.0] - 2026-09-23

### Added

- Native macOS backend (`kling-vz`), selected with `daemon.vmm vz`, no root (#18)
- `kling images copy` moves an image between daemons (#18)

## [0.8.0] - 2026-09-23

### Added

- Elastic memory with `kling run -mem-max` (#16)
- Admission by real memory pressure and load-based scaling (#16)

### Fixed

- Half-written freeze dumps and interrupted commits are detected and cleaned (#16)

### Security

- Snapshots are signed with a host key (#16)
- The guest proxy only reaches the agent port and declared ports (#16)
- Jailer is used by default when installed (#16)

## [0.7.0] - 2026-09-23

### Added

- `kling shell`: interactive terminal inside a microVM (#14)
- `kling sandbox create|ls|renew|rm` (#13)
- `kling exec` with streaming output and `kling cp` for files (#13)
- `-on-ttl freeze` keeps an expired sandbox frozen (#14)

### Fixed

- No leftover Firecracker processes after a failed start (#14)
- TTL no longer resets on wake (#14)

## [0.6.0] - 2026-09-23

### Added

- `base` image builder in the core (#12)
- Extensions can declare units started by `kling up` (#12)

### Changed

- MCP commands and `kling-bridge` move out of the core into kindling-mcp (#12)
- `kling images refresh` becomes `kling mcp refresh-bridge` (#12)

## [0.5.0] - 2026-09-23

### Added

- `kling` extensions: any `kling-<name>` on the `PATH` (#11)
- Named image builders, and `kling images cat|put` (#11)
- `kling-guest`, a generic guest agent (#11)

### Fixed

- `-bundle` copies `package.json` next to the bundle (#9)

## [0.4.0] - 2026-08-28

### Added

- `kling mcp heal` with a systemd timer (#5)
- `kling images rm` (#6)
- glibc base image with `chrome-headless-shell` (#5)

### Changed

- Waking drops from 4.3 s to about 0.2 s (#5)

### Fixed

- Permanent 502 from the gateway after a dead instance (#5)
- 32 silent failures: panics, locks, volumes, durable writes (#5, #6)

### Security

- The gateway no longer forwards its own token (#5)
- Images are no longer world-readable (#5)
- The bridge no longer listens on the network by default (#5)

## [0.3.0] - 2026-08-13

### Added

- Per-service replicas: the same tool in parallel (#2)
- `kling migrate` moves an MCP server to kindling, keeping its name (#2)
- Per-session secrets via MMDS (#2)
- Domain allowlist egress mode (#2)
- `kling squeeze` returns RAM to the host (#2)
- HTTP/SSE proxy mode in the bridge (#2)
- Apple Silicon support, limited (#2)

### Fixed

- `GET` without a session returns 405, not 404 (#2)

## 0.2.0 - 2026-08-11

### Added

- `kling up` and `kling status` replace manual install scripts
- The gateway requires a token
- `kling search` and `kling add` against the official MCP registry
- Persistent volumes and `kling volume populate`
- `connect -install` supports 7 AI clients

## [0.1.0] - 2026-08-08

### Added

- One-line install from releases, with SHA256 verification
- Multi-platform release binaries

### Fixed

- Stateful snapshots no longer break restores

[Unreleased]: https://github.com/juan52878911/kindling/compare/v1.0.0-rc.2...HEAD
[1.0.0-rc.2]: https://github.com/juan52878911/kindling/releases/tag/v1.0.0-rc.2
[0.20.1]: https://github.com/juan52878911/kindling/releases/tag/v0.20.1
[0.20.0]: https://github.com/juan52878911/kindling/releases/tag/v0.20.0
[0.19.0]: https://github.com/juan52878911/kindling/releases/tag/v0.19.0
[0.18.0]: https://github.com/juan52878911/kindling/releases/tag/v0.18.0
[0.17.0]: https://github.com/juan52878911/kindling/releases/tag/v0.17.0
[0.16.0]: https://github.com/juan52878911/kindling/releases/tag/v0.16.0
[0.15.0]: https://github.com/juan52878911/kindling/releases/tag/v0.15.0
[0.14.0]: https://github.com/juan52878911/kindling/releases/tag/v0.14.0
[0.13.0]: https://github.com/juan52878911/kindling/releases/tag/v0.13.0
[0.12.0]: https://github.com/juan52878911/kindling/releases/tag/v0.12.0
[0.10.0]: https://github.com/juan52878911/kindling/releases/tag/v0.10.0
[0.9.1]: https://github.com/juan52878911/kindling/releases/tag/v0.9.1
[0.9.0]: https://github.com/juan52878911/kindling/releases/tag/v0.9.0
[0.8.0]: https://github.com/juan52878911/kindling/releases/tag/v0.8.0
[0.7.0]: https://github.com/juan52878911/kindling/releases/tag/v0.7.0
[0.6.0]: https://github.com/juan52878911/kindling/releases/tag/v0.6.0
[0.5.0]: https://github.com/juan52878911/kindling/releases/tag/v0.5.0
[0.4.0]: https://github.com/juan52878911/kindling/releases/tag/v0.4.0
[0.3.0]: https://github.com/juan52878911/kindling/releases/tag/v0.3.0
[0.1.0]: https://github.com/juan52878911/kindling/releases/tag/v0.1.0
