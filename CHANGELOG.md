# Changelog

Notable changes to kindling: the core, `kling-vz` and the extensions in `ext/`.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), one line per change.
Binaries for every release are on the [Releases](https://github.com/juan52878911/kindling/releases) page.

## [Unreleased]

### Added

- `kling start` (`POST /machines/{ref}/start`) boots a stopped machine again, cold, on its own disk; `-e`/`-env-file` must give its environment again, and a missing key is named; stop flushes the guest's disk and start checks it with `e2fsck` first
- `kling image import <ref>`: Docker/OCI images become kindling images without Docker or root; tags resolve to a digest and every layer is checked by sha256, downloaded 4 at a time and unpacked once
- `kling run -image <docker ref>` imports on first use; `-disk` sizes the writable disk (was a fixed 512 MiB)
- `kling run -e/-env-file` gives the machine its environment at boot via MMDS, not baked into the image: one image per reference; the daemon keeps only the names, but guest RAM (and so a freeze, save or fork) holds the values
- The guest supervises the image's service (`ENTRYPOINT`, `USER`, `WORKDIR`, `STOPSIGNAL`, `HEALTHCHECK`), restarts it and stops it cleanly on `stop`/`rm`
- `kling logs -service`, and `-json` on `kling run` and `kling image import`, for agents
- `debian` image builder: pinned, reproducible Debian images without root, also on macOS (#157)
- `kling db golden build -step/-sql/-init`: a real project's golden in one command (#146)
- `kling db golden build -extension/-preload/-conf`, with file-and-line errors (#145)
- `kling db golden image -ext`: Postgres with TimescaleDB, pgvector, PostGIS, pg_cron, pg_partman (#144)
- `kling db ls` lists copies with engine, golden, state and warnings (#143)
- `kling phone`: Android phones in microVMs, with a web wall and per-session MCP (#120)
- Android image builder in Go, without Lima, debootstrap or e2fsprogs (#119)
- `kling exec`/`kling shell` take `-e KEY` and `-env-file`, keeping secrets out of argv (#135)
- `kling db role -login` lets a migrations role connect from the host (#149)
- `kling db diff` also compares functions, views, triggers, grants, extensions and hypertables (#148)
- `/metrics`: operations by result (run, thaw, freeze, start), admission rejections (409/503/507), boot/restore/thaw/resume/freeze duration histograms, GC evictions, orphan VMMs killed, dropped events, free disk and pending memory
- `GET /events` tells a slow subscriber how many events it lost (`events.dropped`)
- `GET /info` reports the daemon's effective `KLING_*` tuning, and `kling doctor` prints it

### Changed

- `kling stop` drops a machine's memory dump and keeps its disk; stopped service instances from a template are collected after `KLING_STOPPED_RETENTION` (24 h)
- Copies of a golden freeze as diff snapshots mirrored in the copy-on-write store: thaw in 0.1–0.2 s, a sleeping copy costs what it changed (Firecracker)
- `save` and `freeze` squeeze the balloon before dumping, so free memory is not stored
- A cold-booted machine whose image declares a ready probe squeezes its balloon once when the probe passes, returning boot-time page cache to the host: Postgres at rest 167 → 89–99 MiB, nginx 65 → 60 MiB (Firecracker; `KLING_SQUEEZE_ON_READY=0` turns it off)
- Each VMM cgroup also gets `memory.max` (guest RAM + 64 MiB + 1/16), the same `memory.swap.max`, and `pids.max` (128) when those controllers can be delegated (Linux)
- The balloon asks for free page reporting, so pages the guest frees go back to the host without a squeeze (Firecracker 1.14+; falls back without it; `KLING_FREE_PAGE_REPORTING=0` turns it off)
- `save` mirrors the golden memory into the copy-on-write store in the background when the store is mounted, so the first diff thaw of a copy no longer copies it (10 s with Postgres); thaw events show `store`, `mirror` and `memory` phases
- Docker images get a full core per vCPU by default
- Docker images restart their service only when it fails, like `docker run --restart on-failure`; `kling image import -restart` picks `always`, `on-failure` or `no`
- Docker images: the `HEALTHCHECK` timeout bounds each ready probe and its start period extends the boot CPU boost (up to 120 s each)
- Docker images only need `sh`, `mount`, `pivot_root`, `mkdir` and `ln` for the init (no longer `cat` and `grep`)
- A booting machine keeps every vCPU at a full core until its ready probe passes (was: one core until the guest agent answered); `KLING_READY_BOOST=0` restores the old boost, and an explicit `-cpu-pct` gets none (Linux)
- `kling db branch`: `git checkout` switches databases in tens of milliseconds (#125)
- Concurrent `kling db up` calls wait for admission instead of being rejected (#150)
- Daemon API, `state.json`, golden `meta.json` and credential stores are versioned (#138, #140, #141)
- macOS: `kling-vz` snapshots use format 2; older `kling-vz` cannot restore them (#141)
- macOS: vz machines run without a CPU cap unless one is requested (#124)
- Base images provide `/dev/fd`, `/dev/shm` and `/etc/hosts`, as Docker images expect (#156)
- Extension manifests are cached, so extension commands start faster (#127, #128)
- Short README; the full guide moves to `docs/guide.md` (#152)
- Guest kernel ships dm-verity: verity layers boot and verify on Linux and macOS (#161)
- Internal: Android x86_64 ARM translation, phone GPU docs, ANDROID_ID checks (#121, #122, #123, #126)
- Lifecycle operations (`freeze`, `thaw`, `pause`, `stop`, `rm`) answer 404 for an unknown machine and 409 for a wrong state (was 400)
- The watcher scans VMM processes once per round and re-measures disk only for running machines or after a state change

### Fixed

- `kling machine ready` no longer reports ready (exit 0) when the guest agent errors or does not answer; `ReadyResult.detail` says why
- Commit and fork no longer freeze a half-booted guest when the image cannot be inspected (no `debugfs`): they wait for its agent first
- A copy whose resync failed runs its post-restore hooks anyway, instead of inheriting the golden's `done`
- With `-e`, a lost first health check of the guest agent is retried after 15 s instead of holding the environment in MMDS for up to 2 minutes
- A VMM cgroup whose `memory.max` cannot be written still gets its swap and pids limits (Linux)
- A failed freeze no longer asks the resumed guest to remount volumes it never released (up to 50 s holding the machine's lock)
- Two jailed machines booting at once on a fresh root no longer fail with "Failed to canonicalize path .../jails"
- The daemon no longer reuses a kept connection to a stopped or removed machine's address: the next machine on that IP (`kling start` keeps it) answered the first exec or the next stop with a connection reset
- Two identical image builds at once (two `kling run -image` of the same reference) build once instead of replacing the image under the first one's machine
- The daemon builds `oci`, `debian` and `android` images with its own binary, not the `kling` installed on the host (unless `KLING_BUILDERS_DIR` is set)
- Replacing an image or the kernel (`PUT /images/{name}/blob`) can no longer race a machine that is booting from it
- Image layers: a hard link whose target path goes through a directory symlink (`bin/busybox` with `bin -> usr/bin`) no longer fails as "hard link to missing"
- The collectors of failed machines and of disk no longer hold the daemon's lock while they read the disk
- Firecracker starts with a 1 MiB MMDS store (was its 50 KiB default), so a 32 KiB `-e` environment plus session secrets fits; a larger store is refused with the limit (Linux)
- Simultaneous freezes and saves reserve their disk: they no longer all pass a free-space check that only one of them fits
- A full copy-on-write store no longer pauses a machine that is in the middle of freezing
- A network setup that fails halfway no longer leaves its namespace and veth behind until the daemon restarts (Linux)
- `thaw` goes through memory admission like `run`: a storm of thaws is refused with 507 instead of exhausting the host
- `thaw` of a machine removed while it waited fails instead of starting a VMM; re-adopting a live VMM restores its CPU ceiling (Linux)
- Daemon shutdown waits for in-flight lifecycle operations, so their last state is saved
- Paths with a backslash are refused like escaped slashes
- Gateway: freezing an idle paused instance no longer races a request resuming it, which froze the freshly adopted instance (502 on its first call)
- Docker images keep their own `/run`, as in Docker: `mariadb` (whose entrypoint needs `/run/mysqld`) now starts
- Docker images: an unknown `STOPSIGNAL` falls back to SIGTERM with a warning instead of leaving the image without its service; `SIGRTMIN+n` and every Linux signal are understood
- Docker images: a numeric `USER` missing from `/etc/passwd` runs with group 0, as in Docker
- Docker images keep their own `/tmp` on disk, as in Docker, instead of a tmpfs that used guest RAM
- `kling image import` says when an image has no ready probe (only UDP ports), and Docker schema 1 manifests fail with a clear error instead of "has no layers"
- The guest's `/etc/hosts` entries are no longer glued to a last line without a newline
- `kling image import` no longer silently overwrites an image of the same name from another reference (`redis:7` and `ghcr.io/x/redis:7`); `-replace` does, and the same import is not redone
- A new volume takes the owner, mode and (up to 64 MiB) content of the image's directory, as in Docker: non-root services like `grafana` can write to it; volumes with data are never touched, and a volume is seeded only once
- XFS store: memory files and overlays no longer share project ids (a new copy's disk could start over quota)
- A dump that does not fit is refused before pausing, and a failed dump removes what it wrote
- `ext4.Write` no longer panics on layers without data
- A full copy-on-write store pauses machines instead of returning EIO to guests (#143)
- Disk GC no longer deletes frozen `kling db branch` copies (#143)
- `kling db branch -golden` is honoured or fails, never silently ignored (#147)
- Live shared folders survive daemon restarts and freezes (#142)
- Unix sockets with long paths are reached through a short link (#153)
- A lost DNS packet no longer turns a credential proxy request into a 502 (#136)
- `kling up -check` fails when something is missing and finds tools in `/sbin` (#134)
- `kling image build NAME -spec -` works with the name before the flags (#137)
- Internal: `kling db doctor` grouping, `rm`/`class`/`audit` fixes, MCP health and pagination (#135, #149, #151)

### Security

- Diff freeze: the daemon never follows links in a machine's directory, checks the seal's base is the golden's memory, and only freezes as a diff where the filesystem tells holes from zero pages
- `state.json` is written with schema 2 while a copy is frozen as a diff, so an older kling refuses to start instead of loading it as full memory
- `run -disk` must fit in the free disk; `KLING_MAX_DISK_MIB` lowers the maximum
- `run -image` names images by a keyed hash of the whole reference, so another registry's image is never reused
- Docker images: the config blob must declare a size (max 8 MiB); tar entries are capped while reading layers
- Docker images: registries never redirect to plain http, and only `localhost`/`127.0.0.1`/`::1` are spoken to over http
- Docker images: the `HEALTHCHECK` runs as the image's `USER`; arguments given after `--` are not echoed
- Docker images: the `oci` builder runs as its own unprivileged user (`-build-as`, default `kindling-build`), not root; the daemon checks and moves what it leaves
- `-env` build variables move out of the world-readable `/entrypoint` into a 0600 file (#155)
- Credential proxy: keys never go to another host; substitution only where declared (#133)
- macOS: per-machine `kling-vz` sandbox, stricter peer checks, DNS limits, public DNS (#131)
- `kling db clone` masks credential columns and detects more personal data (#135)
- `kindling-sandbox` never hands out another tenant's machines or graphs (#135)
- MCP gateway token stays out of stdout and argv; `kling logs` escapes control codes (#135)
- Unique machine names; the daemon no longer follows symlinks in jails or sockets (#129, #134)
- Downloads of Firecracker, Alpine, Node, Chrome and esbuild are pinned by sha256 (#134, #135, #139)
- `kling add -env` values stay out of the host process list while building (#159)
- Pinned Debian base picks up openssl and pcre2 security updates (deb13u3) (#160)
- Internal: scheduler races, silent state losses and `make deploy` without fixed `/tmp` paths (#130, #132, #139)

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

[Unreleased]: https://github.com/juan52878911/kindling/compare/v0.17.0...HEAD
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
