# Changelog

Notable changes to kindling: the core, `kling-vz` and the extensions in `ext/`.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), one line per change.
Binaries for every release are on the [Releases](https://github.com/juan52878911/kindling/releases) page.

## [Unreleased]

### Added

- `docs/compatibilidad.md`: what 1.x promises, the compatibility matrix and what is left before 1.0; status codes per route and the stable `code` of each in `docs/api.md`, checked against the contract goldens by a test.
- Signed releases: `SHA256SUMS.minisig` (ed25519, minisign format) and SLSA build provenance for every asset; `kling upgrade`, `kling up` image downloads and `install.sh` (with `minisign`) check the signature. Unsigned releases are accepted with a warning until v1.0.0 and refused from then on (`docs/firmas.md`).
- Error bodies carry a stable `code` next to `message` (`not_found`, `name_taken`, `machine_limit`, `tenant_quota`, `stale_golden`, `disk_full`, `insufficient_memory`…; capability `error-codes`); `api.IsMachineLimit`, `IsTenantQuota` and friends read it and fall back to the text with older daemons. New `api.IsNameTaken`, `IsStaleGolden`, `CodeOf`.
- Capabilities `pool` (`GET/PUT /snapshots/{name}/pool`) and `egress-blocked` (`GET /machines/{ref}/egress`) for the two v0.19 routes that had none.
- Contract goldens with tests: the daemon's routes and their authz action, the status code of each route, the JSON shape of every `pkg/api` type and the `-json` output of the main CLI commands; removing or changing a line fails unless `api.APIVersion` goes up.
- Documented exit codes (`kling help`): 0 ok, 1 error, 2 usage, 3 daemon too old, 4 stale template, 5 the guest agent lacks a capability, 130 interrupted, N the remote code in `exec`/`shell`/`try`.
- `kling version` and `status -v` show the API version, the VMM and the guest agent versions announced by the machines; `version -json` and `status -json` gain `api`, `daemon_api`, `vmm`/`backend` and `agents`.
- `recipe.json` records the image's guest agent (`agent`: version from the builder, capabilities from the first machine that boots it); `GET /images` returns it, and `kling image ls` and `kling doctor` flag images whose agent is older than v0.18 or more than one minor behind the daemon.

### Changed

- Status codes made consistent before 1.0 freezes them (`internal/daemon/testdata/contrato/codigos.txt`): 404 instead of 400 for a missing machine in `PUT /machines/{ref}/labels` and `POST …/commit`, a missing image in `GET/PUT /images/{name}/files` and a missing source in `POST /volumes/{name}/clone`.
- Host failures are 500 instead of 400 in resize, snapshots and annotations, pools, template credentials, graphs, the store, `POST /volumes`, populate and `POST /cow/store/grow` (501 where the platform has no store); renew gives 500 instead of 502.
- `DELETE /volumes/{name}` answers 400/404/409/500 by cause instead of 409 for everything; `DELETE /sandboxes/{ref}` like `DELETE /machines/{ref}` instead of 500 for everything; a fork whose copy's agent never answers is 504, like `POST /sandboxes`.
- 201 instead of 200 for `POST /volumes`, volume clones and snapshots, `POST /graphs/{ref}/snapshot` and `POST /registries` (the Go client accepts any 2xx). `POST /images` stays 200: a build reused from a concurrent request answers the same.
- Disk full is 503 everywhere: volume copies and the OCI blob cache answered 507, which makes a gateway freeze machines to make room.
- `PUT /images/{name}/files`: a cut body is 400 (413 only over the limit), and a missing file without `create` sets `skipped` (still 200, `error` kept for older clients).
- `GET …/logs` and `GET /images/{name}/recipe` give 404 only when there is nothing, 500 when reading fails; `DELETE …/credentials/{env}` decides its 404 by sentinel, not by the text.
- Unknown routes (404) and wrong methods (405) answer in JSON with `message` and `code` instead of plain text; the status code is the same.
- The `kling compose` help lists `ps`, `logs`, `exec`, `stop`, `start` and `restart` (they already worked).
- Versioned on-disk formats: `publish.json` and `fondos.json` get `schema: 1` (with a `.v0.bak` and durable writes), graphs `format: 1`, the CLI config `schema: 1` (and `Save` keeps keys it does not know), the freeze seal `schema: 1`, and MMDS `kling_mmds: 1`. A file from a newer kling is left untouched and the error says to update kling.
- `state.json` moves to schema 3 only when a machine has `cmd`, `entrypoint`, `share_wait`, `volume_wait` or `net_mibps` (an older kling would drop them), and every schema bump of `state.json` (also 1 -> 2) leaves a `.v<N>.bak`.
- Credential stores with a sigv4 credential are sealed as `KLCS 2`: kling v0.18 would use them as HTTP credentials and drop their fields.
- `POST /graphs` rejects fields the daemon does not know (400) instead of dropping them.
- A golden or frozen machine is stale when it was made on another architecture or with a kling-vz/kling-hv snapshot format this one does not read; kling-hv restore errors are explained like kling-vz ones.

### Removed

- The CLI aliases that warned since v0.18: `rmi`, `images`, `volumes`, `sandboxes`, `info`, `resize`, `squeeze`, `mmds`, `models`, `chispa`, and the kling-mcp verbs `add`, `search`, `gateway`, `export`, `memory`, `migrate`; they exit 2 naming the new command. `commit`, `snapshots` and `plugins` stay.
- `kling run -cpu` (use `-cpu-pct`) and `kling ai chispa deploy -vcpus` (use `-cpus`): they exit 2 naming the new flag.

### Fixed

- The swap admission error pointed to `docs/estabilidad.md`, now `docs/auditoria-estabilidad-2026-08.md`.
- CI: `e2e-linux.yml` installs the `base` builder for its private daemon, so the toolchain image builds and `90-e2e.sh` and `94-e2e-upgrade.sh` stop falling back to `min`; `94-e2e-upgrade.sh` only expects a refreeze on `-rollback` when `state.json` was migrated.

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
