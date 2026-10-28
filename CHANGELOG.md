# Changelog

Notable changes to kindling: the core, `kling-vz` and the extensions in `ext/`.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), one line per change.
Binaries for every release are on the [Releases](https://github.com/juan52878911/kindling/releases) page.

## [Unreleased]

### Added

- `kling volume clone <src> <dst> [-copy]`: instant copy-on-write volume clones (reflink/clonefile), with a real clone probe in `kling status -v`; refuses on filesystems that cannot clone unless `-copy`
- `kling image import` stores an image whose layers extend an already imported one (same diff_id prefix, agent and arch) as a small layer over it instead of a second full ext4; a base with layers on top cannot be removed
- `kling template pool <name> -n N` keeps N awake copies of a template; `run -from` claims one in ~10-20 ms instead of restoring (off by default)
- `kling image import -warm [-step CMD]...` goes from a registry ref to a ready template in one command; `kling db golden -from-oci REF build` builds a database golden from a Docker/OCI image (e.g. pgvector/pgvector:pg16) without compiling extensions
- `kling image import -slim` trims Postgres and Timescale images by family rules (LLVM/JIT, bitcode, headers, old extension versions, `jit=off`): postgres:17-alpine 297→91 MiB, postgres:17 457→167 MiB, Timescale 1.6 GiB→187 MiB; `kling save -audit` reports files the running service never opened, without trimming
- `kling image import -ulimit` and `-sysctl`: the guest agent applies them before the service starts (OpenSearch and ClickHouse need nofile and vm.max_map_count)
- `kling db` templates of ClickHouse (`-engine clickhouse`), with the password stored as a SHA-256 computed on the host (#118)
- `kling db` templates that speak the MongoDB protocol through FerretDB (`-engine ferretdb`); copies get a `mongodb://` DSN and a rotated password like Postgres (#117)
- `kling db` templates of Oracle MySQL 8.4 and 9 from the Docker image, with `caching_sha2_password` hashes computed on the host (#114)
- Every copy of a template regenerates its SSH host keys when the image has `ssh-keygen`, and sshd serves the new ones (#176)
- `kling db golden image|build -engine valkey`: Valkey templates, served like Redis (#113)
- `docs/linux-arm64.md`: which arm64 hosts can run kindling (Graviton .metal, Ampere, OCI BM A1, Pi 5 with the 4K kernel), setup, tuning, and why goldens don't move between arm64 CPU models

### Changed

- `kling image import` refuses images with no command and no shell, or with s6-overlay as init, with a clear error; a wrong-architecture image says which side is which; `scripts/bateria-imagenes.sh` checks 10 popular images end to end
- `kling rm` answers once the VM is dead and cleans up in the background; the name is free at once and an interrupted cleanup finishes on daemon start (p50 ~65 → ~37 ms)
- `kling up` on arm64: the missing-`/dev/kvm` hint no longer says `modprobe kvm_intel` (KVM is built in there), and a host page size other than 4 KiB is a warning (Raspberry Pi 5 boots a 16K kernel by default)
- Guest kernel: the 6.1.140 tarball sha256 is pinned, so `scripts/builders/kernel/build.sh` runs; arm64 adds errata 1319367, 1530923 and 2067961 and drops 2645198, which 6.1 doesn't have
- `scripts/30-fetch-artifacts.sh` recommends `KERNEL_SOURCE=build` on aarch64 when it isn't set
- Release binaries are built with Go 1.27.3 (12 reachable stdlib CVEs fixed); the minimum Go in `go.mod` stays 1.24
- The release workflow runs the full test suite before building, uses a read-only token except to publish, and pins actions by SHA
- `kling` only runs `kling-*` extensions owned by you or root with no group/other write on the file or its directory, and ignores relative `PATH` entries
- Sizes above 1 PiB (`-mem 1e30G`) are rejected instead of saturating
- Live shares move up to 1 MiB per operation when agent and daemon are both new (negotiated, 128 KiB otherwise), ro shares keep the guest page cache for files unchanged since the last open, and listing a large directory no longer reopens it for every page
- macOS: the CPU ceiling charges time on efficiency cores at what it is worth on a performance core; under launchd a 25 % ceiling slowed a CPU-bound guest ×12–15 instead of ×4 (now ×4–5)
- macOS: while the guest boots, the CPU ceiling lets all its vCPUs run (up to the host's cores), as on Linux
- macOS: read-only drives use the host cache, and a drive marked `kling_disposable` (the overlay of a non-persistent machine) skips host flushes: 300 guest `sync` 1.8 → 0.7 s
- macOS: bigger TCP buffers in the user-space network (8 MiB receive, 6 MiB send) and at most 512 live TCP connections per machine

### Fixed

- Egress `none` and `allowlist` no longer let conntrack-INVALID packets (e.g. RST/FIN with payload) through the netns FORWARD; the host also drops veth-to-veth forwarding, spoofed sources and 172.16/12 outside 172.30/16
- `kling rm|stop|freeze|thaw|pause a b c` keeps going after a failure, prints each error and exits 1 at the end, like docker
- Daemon status codes: removing an image or snapshot answers 400 for a bad name, 404 if it does not exist and 409 if it is in use; snapshot lookups decide 404 by error type, not by message
- Request bodies over the limit answer 413 instead of 400 "unexpected EOF" (exec, shell, resize, mmds, credentials); store and annotations answer 413 only for size
- `kling events` no longer stops on an event line over 64 KiB
- `kling freeze`, `thaw` and `start` no longer fail after 60 s waiting for the daemon's answer
- `run -from` no longer pays a ~320 ms resync retry (up to 2 s) on templates saved while the guest agent was not listening: the mark is kept in the signed snapshot meta and the freeze seal
- macOS on M4: OpenSSL no longer dies with `Illegal instruction` in the guest; the agent sets `OPENSSL_armcap` when the guest kernel reports SVE2 without SVE (#183)
- Guest `/exec`: a background grandchild (`sh -c "sleep 600 & echo ok"`) no longer holds the reply until it exits, and cancelling the request kills the whole process group
- Guest agent: setting the MMDS route no longer logs a spurious "no child processes"
- Live shares no longer hand absolute host symlink targets to the guest: targets inside the share become relative, the rest get EACCES
- MCP gateway: after a restart, tools of a service whose instance was still running work again; the gateway clears its old sessions and no longer leaks one per catalog fetch (#187)
- Image import: a file name over 255 bytes or with a NUL is rejected instead of writing a corrupt directory entry; a file alternating data and zero blocks no longer fails with "file too fragmented"; xattrs in unknown namespaces or too large in `user.*` are dropped with a warning instead of failing the image
- Reading an ext4 with a corrupt superblock (0 inodes or blocks per group) returns an error instead of panicking
- Extension manifest, hook and `-h` output is capped at 1 MiB; the old cap was bypassed by `io.Copy`
- Concurrent writes of the same state file no longer share a temp file
- A panic while keeping a service warm no longer kills the gateway
- `scripts/release.sh` refuses a tag that already exists on origin and fetches before comparing main
- `kling-bridge`: client replies to server requests (sampling, elicitation, roots) are accepted at once instead of hanging until `-request-timeout`; sessions with an open SSE stream or a call in flight are never reaped or recycled; starting a session no longer holds the bridge lock
- MCP gateway: only a 400 that says the session is unknown triggers a new handshake and retry (a bad request returns its real error); the `/mcp/_all` endpoints cap the request body at 8 MiB
- MCP gateway: re-adopting one of its own running instances after a failed liveness probe no longer resets its bridge and wipes live sessions
- `kling-sandbox`: a prewarmed sandbox whose renew fails after it was labelled for a tenant is removed instead of left counting against that tenant; with a quota set, creating fails closed (503) while a host does not answer
- macOS: in allowlist mode an IP resolved with TTL 0 stayed allowed forever; it now expires after 60 s like any other
- macOS: if the CPU brake process dies with the VM's helper stopped, `kling-vz` exits instead of keeping a machine that never runs again; a SIGTERM to it resumes the helper first
- macOS: a failed VM start no longer leaks the VM and its console pipe
- macOS: a slow CPU reading no longer blocks the `kling-vz` API

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

[Unreleased]: https://github.com/juan52878911/kindling/compare/v0.18.0...HEAD
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
