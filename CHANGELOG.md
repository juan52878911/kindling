# Changelog

Notable changes to kindling: the core, `kling-vz` and the extensions in `ext/`.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), one line per change.
Binaries for every release are on the [Releases](https://github.com/juan52878911/kindling/releases) page.

## [Unreleased]

### Added

- Per-volume sync mode on macOS: `-volume name:/mount:sync=barrier|none` (API `VolumeAttachment.sync`). `full` (default) keeps `F_FULLFSYNC` on every guest flush; `barrier` uses `F_BARRIERFSYNC` (kling-vz: `fsync`), 300 guest fsyncs ~1.7 s → ~0.1 s; `none` skips flushes (~10 ms) and can leave the volume's ext4 broken if the Mac goes down. Writable volumes only; accepted and ignored on Firecracker.
- kling-hv: virtio-blk `DISCARD` and `WRITE_ZEROES` on writable disks (`F_PUNCHHOLE`, unaligned edges written as zeros): `fstrim` in the guest gives the space back to the host (216 → 16 MiB after deleting 200 MiB).
- kling-hv: os_signpost intervals (subsystem `dev.kindling.hv`) for the cold boot, restore and freeze phases, plus events for the first guest instruction, the agent's first vsock answer and slow vCPU exits; free when nobody traces. `KLING_HV_TRACE=1` also logs a one-line breakdown per boot, restore and freeze (`docs/backend-hv.md`).
- The daemon reacts to host memory pressure before jetsam (or the OOM killer) kills VMMs: on warn it squeezes idle machines, on critical it also freezes the largest idle one, rate-limited and with hysteresis. macOS reads `kern.memorystatus_vm_pressure_level`, Linux PSI. `daemon.memory_pressure=off` (or `KLING_MEMORY_PRESSURE=off`) turns it off; `/metrics` gains `kling_memory_pressure_level`, `kling_memory_pressure_actions_total` and `kling_memory_pressure_reclaimed_mib_total` (`docs/mac.md#presión-de-memoria`).
- macOS: `kling run -host-port PORT|HOST:GUEST` lets the guest reach a TCP port of the Mac's loopback (Ollama, `llama-server` with Metal) at `host.kindling.internal` / `172.16.0.1`; only the listed ports, also with `-egress none`, set only from the host and kept across freeze/thaw (`host_ports` in `POST /machines`, capability `host-ports`). Each connection is dialled by the daemon through the link broker and audited. Linux rejects it. A `state.json` with such a machine is written as schema 4, which older klings refuse (`docs/mac.md#servicios-del-mac-desde-el-invitado--host-port`).
- Image snapshots: `kling run -image X` restores a snapshot of `X` instead of booting cold. The first run with given parameters boots cold and the daemon saves one in the background from a separate machine that carries nothing of any run (no env, command, volumes, shares, secrets or network) and whose agent waits before starting the service; each copy is resynced and then gets its own env and command, so its service starts fresh. Measured on kling-hv (M4, small Alpine image): ~150 → ~45 ms until the agent answers. `-fresh` and `KLING_IMAGE_SNAPSHOTS=0` opt out; skipped with volumes, shares, kling-vz and agents that cannot wait. LRU-pruned under `KLING_IMAGE_SNAPSHOT_MIB` (1 GiB on macOS, 4 GiB on Linux), removed with `kling image rm`; `kling image snapshots [ls|prune]`, a line in `kling status` and `kling image ls`, `GET`/`DELETE /image-snapshots` (`docs/snapshot-imagen.md`).
- kling-hv: live shares can go over virtio-fs, opt-in per share: `-share SRC:DST:ro|rw:virtiofs` (API `ShareSpec.transport`). The guest kernel talks FUSE to the same Go server the agent used, now inside kling-hv, with the same `internal/share` session and `os.Root` in the daemon. On an M4: metadata 3-5x faster (1000 creates 1.5 s → 0.3 s), sequential I/O ~2x (reads ~0.7 → 1.45 GB/s, writes ~0.5-0.6 → 1.2-1.4 GB/s); freeze/thaw keeps open files and pending writes. The trade-off: host changes are seen after the 1 s cache and don't wake watchers in the guest (`node --watch`, vite), so shares stay on FUSE unless asked. `KLING_HV_VIRTIOFS=1` makes virtio-fs the daemon default (`:fuse` opts a share out), `=0` forbids it. Templates (`-share-wait`, `compose up`), kling-vz, older agents and kernels without virtio-fs use FUSE; Linux rejects `:virtiofs`. `docs/compartir.md`.
- arm64 guest kernel: `CONFIG_VIRTIO_FS=y` (required by `scripts/check-kernel-config.sh`).

### Changed

- kling-hv: machines can have up to 64 vCPUs (what Hypervisor.framework allows, 64 on an M4), never more than the host's cores; the daemon rejects more vCPUs than cores on macOS before launching the VMM. Snapshots of machines with more than 8 vCPUs use memory-file state version 5; machines with up to 8 are still saved as version 4, which older kling-hv builds restore.
- `kling run -nested` (kling-hv only, Apple M3 or later with macOS 15+): the guest boots at EL2 with a separate KVM kernel (`images/vmlinux-nested`, built with `VARIANT=nested TARGET_ARCH=arm64 scripts/builders/kernel/build.sh`) and can run Firecracker inside. Nested machines can't be frozen, saved or forked; kling-vz and Firecracker refuse the option.
- kling-hv memory file state v6 (v4, or v5 with more than 8 vCPUs, plus a virtio-fs section) and snapshot format `kling_vz` 4 (virtio-fs server state), written only by machines with virtio-fs shares; everything else still writes v4/v5 and `kling_vz` ≤ 3.
- kling-hv: a virtqueue whose rings lack the alignment virtio requires is not serviced (it was undefined behaviour in C, found by fuzzing under UBSan).
- Linux: `-egress allowlist` boots faster. Seed domains resolve in parallel within 500 ms in total, the rules go in one `iptables-restore`, and the DNS resolver adds IPs to the ipset over netlink instead of forking `ip netns exec ipset` per IP. Measured on the lab (CT 108, `kling run` of node22): 5 domains 485-501 → 148-151 ms, 1 domain 196 → 143-150 ms, one domain whose DNS never answers 6.4 s → 0.59 s.

### Security

- Linux: a machine only reaches the host-side services (allowlist DNS, credential, Postgres and MySQL proxies, graph edges) on its own veth (`-m addrtype --limit-iface-in`); a process in another machine's netns, like a compromised Firecracker without the jailer, reached them through that machine's HostIP. The old INPUT rules are removed on startup.
- Linux: under the jailer, Firecracker runs in its own PID namespace (`--new-pid-ns`); the daemon tracks its host PID from the jailer's pid file and watches it by pidfd.
- Linux: ownership and modes of trees the VMM can write (snapshot directories) are set by descriptor inside an `os.Root`, so a symlink swapped in between the check and the `chmod` can't redirect it. `EnsureWritable`, unused, is gone.
- Linux: network setup refuses a `kl-<id>` netns whose veth has another address (another daemon's machine with the same id prefix) instead of tearing it down.

### Fixed

- `kling stop` no longer reports a machine as stopped while its VMM survives `SIGKILL` (e.g. stuck in D state): it returns an error and keeps the PID and network, so `kling start` can't launch a second VMM on the same overlay.
- A failed `state.json` write is no longer dropped: the snapshot is kept and retried with backoff, and `run`, `run -from` and `start` don't launch a VMM until its machine is on disk.
- A thawed machine's memory dump is invalidated as soon as the guest resumes, so a daemon or host crash can't thaw that stale memory over the disk it has since written.
- The machine cap counts machines that are still being created, so concurrent `run`s can't overshoot it.
- Smaller fixes: `fondos.json` is written outside the pool lock, the last-squeeze time is forgotten when a machine is removed, each Firecracker API ping while waiting for the socket has a 200 ms timeout, reconcile scans for orphan VMMs without holding the global lock, `run` validates the image name first, and Firecracker errors without `fault_message` say what it answered.
- Scheduler: the reaper no longer freezes an idle instance underneath a concurrent acquire that adopts it (502 on its first request); victims are held with the waker's lock and the in-acquire mark until frozen, and one that fails to freeze goes back to the map.
- Scheduler: `ensure` drops a dead primary through the normal removal path and forgets the sessions bound to it when the rebuilt one is a different machine.
- Scheduler: a failed TTL renewal is no longer taken as done; the reaper retries it on its next pass and drops the instance when the daemon rejects it (404/409), and a running machine whose renewal is rejected is not adopted.
- `exec ?wait=1` caps each stream at 8 MiB (`api.ExecWaitMaxOutput`) and marks the result `truncated`; the NDJSON stream still allows 64 MiB.
- Run, sandbox, fork and `PUT /machines/{ref}/labels` reject more than 64 labels or 4 KiB of labels, and run and sandbox more than 256 `allow_domains`, with a 400.

### Fixed

- Blob uploads (`PUT /images/{name}/blob`, `PUT /oci/blobs/{digest}`) are cut after 2 minutes without data; a stalled client kept an FD, a temp file and, for OCI, the cache lock that sweeps and other uploads need.
- Errors seen by non-admin callers of `logs`, `credaudit` and `guest` no longer include host paths or the guest's internal address; the detail goes to the daemon log.
- The daemon removes its socket on exit only if it is still the one it created.
- A sandbox whose agent never answered and that could not be removed is logged, and the error names its ID.

### Fixed

- kling-hv: the network receive thread no longer stops for good when the guest offers an rx chain whose buffers add up to more than 2 GiB (overlapping descriptors); Darwin's `recvmsg` rejected it with `EINVAL` and the guest lost its network. The chain is capped to the largest frame, and an `EINVAL` drops the frame and returns the buffer empty.
- kling-hv: virtqueues keep their own used index instead of reading `used.idx` back from guest RAM, as the virtio spec requires, so what the guest writes there no longer moves the device. Memory-file state keeps its shape and version (the index lives in former padding, flagged in the header): older snapshots restore by taking it from their RAM, and older kling-hv builds still read new files.
- kling-hv: a transient `EAGAIN` from `poll` (Darwin returns it when it cannot allocate its internal structures) no longer stops the network receive thread or the vsock thread for good; they retry after 1 ms. The vsock thread also logs why it stops instead of dying silently.

## [1.0.0] - 2026-11-05

kindling 1.0. From this release on, `docs/compatibilidad.md` is a promise: API version 1 with contract goldens, on-disk formats read from N-1, and signed releases (`kling upgrade` refuses an unsigned one). kling-hv is the stable macOS VMM, with a performance round on the Mac (thaw p90 2 s → 39 ms, `fork -n 3` p90 6.4 s → 0.3 s). Images built before this release keep their old guest agent; rebuild them for the Go init and for `kling run -e` reaching a debian image's `service`. A kling-hv template with 2 or more vCPUs saved by an older build may not restore; rebuild it. Upgrade with `kling upgrade`.

### Added

- `kling compose`: nested interpolation like Docker Compose (`${A:-${B:-x}}`, `-`, `?`, `:+`, `$$`), port ranges (`8000-8002:9000-9002`), long-syntax `host_ip` and `protocol`, and the same host port on several IPs as one publication.
- `kling compose exec -i` sends stdin when it is a terminal, like `kling exec -i`.
- The exit-code section of the guide (`docs/guide.md#exit-codes`).
- `docs/compatibilidad.md`: what 1.x promises, the compatibility matrix and what is left before 1.0; status codes per route and the stable `code` of each in `docs/api.md`, checked against the contract goldens by a test.
- Linux: `kling doctor` and `kling up -check` warn when the host's FORWARD policy is DROP (Docker sets it) with no rule accepting kindling's traffic, which leaves `-egress internet`/`allowlist` machines silently without network, and print the two `iptables -A` rules that fix it; the daemon logs the same once at startup. The firewall rules kindling installs are unchanged.
- Signed releases: `SHA256SUMS.minisig` (ed25519, minisign format) and SLSA build provenance for every asset; `kling upgrade`, `kling up` image downloads and `install.sh` (with `minisign`) check the signature. Unsigned releases are accepted with a warning until v1.0.0 and refused from then on (`docs/firmas.md`).
- Error bodies carry a stable `code` next to `message` (`not_found`, `name_taken`, `machine_limit`, `tenant_quota`, `stale_golden`, `disk_full`, `insufficient_memory`…; capability `error-codes`); `api.IsMachineLimit`, `IsTenantQuota` and friends read it and fall back to the text with older daemons. New `api.IsNameTaken`, `IsStaleGolden`, `CodeOf`.
- Capabilities `pool` (`GET/PUT /snapshots/{name}/pool`) and `egress-blocked` (`GET /machines/{ref}/egress`) for the two v0.19 routes that had none.
- Contract goldens with tests: the daemon's routes and their authz action, the status code of each route, the JSON shape of every `pkg/api` type and the `-json` output of the main CLI commands; removing or changing a line fails unless `api.APIVersion` goes up.
- Documented exit codes (`kling help`): 0 ok, 1 error, 2 usage, 3 daemon too old, 4 stale template, 5 the guest agent lacks a capability, 130 interrupted, N the remote code in `exec`/`shell`/`try`.
- `kling version` and `status -v` show the API version, the VMM and the guest agent versions announced by the machines; `version -json` and `status -json` gain `api`, `daemon_api`, `vmm`/`backend` and `agents`.
- `recipe.json` records the image's guest agent (`agent`: version from the builder, capabilities from the first machine that boots it); `GET /images` returns it, and `kling image ls` and `kling doctor` flag images whose agent is older than v0.18 or more than one minor behind the daemon.

### Changed

- Faster on macOS:
  - kling-hv restores no longer wait for the host's memory level: it maps memory lazily, and on a busy Mac that wait turned ~1 in 3 thaws/forks from ~40 ms into 1-2 s.
  - `fork -n N` restores up to 8 copies at once on kling-hv.
  - `kling db fork` prepares copies in parallel, with 2 execs per copy instead of 4.
  - The first connection to a just-thawed service retries from 5 ms instead of a flat 100 ms.
  - Waits for the agent and for compose templates back off from 25 ms.
  - Disks are cloned with `fclonefileat` instead of exec'ing `cp -c`.
  - A thaw checks the machine's own VMM pid instead of scanning every process.
  - The thaw tail delivers credentials alongside the resync.
  - `compose up` asks the daemon once and publishes in parallel.
  - `kling up` runs `codesign` once per binary.
  - `/metrics` measures machines in parallel.
  - The state store locks per key.
- kling-hv:
  - Freeze is about 2× faster: clean pages are read-advised instead of rewritten.
  - Disk requests use one `preadv`/`pwritev`.
  - vsock tx writes straight from guest buffers and wakes its thread only on change.
  - Network rx and tx have separate locks.
  - Throttle pauses no longer save every register or raise QoS.
- Guests:
  - Debian-built images boot with the Go init, dm-verity included and still fail-closed, instead of a shell script. A verified layer mount that hangs (FEC with a wrong root hash) restarts the guest after 60 s, so the machine is marked failed instead of staying `running` with no agent.
  - The agent starts listening before it reads MMDS.
- kling-hv is stable in 1.0 (VMM and `KLNGHV` snapshot format). Until CI has an Apple Silicon runner, each release runs `92-e2e-mac.sh` and `95-e2e-upgrade-mac.sh` with `VMM=hv` by hand, and `release.sh` refuses a 1.0+ tag without it (the `e2e-mac.yml` job green on the commit, or `KLING_E2E_HV=<sha>`).
- macOS (kling-hv): DNS answers are cached per machine and A/AAAA resolve in parallel (10 lookups 1480 → 80 ms on an M4).
- macOS (kling-hv): the balloon has its statistics queue, so `kling machine squeeze` sees guest memory (136 MiB reclaimed instead of 4 on an idle machine); snapshot state v3, v1 and v2 still restore.
- macOS (kling-hv): free page reporting returns memory the guest frees without a squeeze (100 MB freed: footprint 193 -> 95 MiB in 11 s); balloon releases are batched (~5300 remaps per squeeze -> ~9); snapshot state v4, v1 to v3 still restore.
- macOS (kling-hv): the guest clock stops while a machine is paused (a long pause logged soft lockups and RCU stalls) and is resynced in the background on resume.
- macOS (kling-hv): network and vsock receive straight into guest buffers; closed vsock host ends are seen through kqueue instead of a 1 s probe. Through a published port on an M4 (`nc -l` in a 256 MiB guest, 3 VMs x 3 transfers, timed until `nc` exits), against the VMM just before this change: upload 512 MiB 1408 -> 2115 MiB/s median (905-2660), download 256 MiB 747 -> 803 MiB/s (668-855), same VMM CPU.
- The first exec after a cold boot is ~40-55 ms faster: the agent probe no longer waits out a probe sent before the guest's vsock was up, and the agent sets the MMDS route over netlink instead of running `ip`.
- The guest clock is resynced right after boot (the RTC only has whole seconds, so each boot started up to 0.5 s off; now within a few ms).
- `kling machine squeeze` polls the balloon every 10-40 ms instead of 100 ms.
- `kling doctor` shows the installed kernel and the templates and frozen machines made with another one; `GET /info` adds `kernel_sha256` and `kernel_mismatch`.
- `install.sh` requires minisign from v1.0.0 (`KLING_INSECURE_NO_MINISIGN=1` to skip); releases run the lint and apidiff and refuse a 1.0+ tag without release public keys; an `API-Break:` trailer must name the packages it excuses.
- ext/db: its regexps compile lazily (`--kling-manifest`, also run by its git hook: 0.72 ms and ~20k allocations of init -> 0.02 ms); ext/sandbox requires kling 0.17.0.
- macOS: the daemon reads hv machines' footprint and CPU locally instead of over HTTP.
- Go packages that are not part of the public API moved from `pkg/` to `internal/` (`aigw`, `chispa`, `codificador`, `digest`, `durable`, `esquema`, `intent`, `transport`, `von`; `pkg/share` is now `internal/shareproto`). `pkg/api`, `pkg/plugin` and `pkg/config` are the 1.x Go API; what other modules of the repo import (`guest`, `credproxy`, `scheduler`, `linkbroker`, `panico`, `units`) and `lazyre` (the type of `api.KeyPattern`) stay in `pkg/`.
- The upgrade e2e starts from the previous release's real assets, checked against its `SHA256SUMS`, instead of piping `install.sh`.
- `docs/compatibilidad.md`: the 1.0 checklist is down to what is still open, and the guest-agent requirements (`env` v0.18, `cmd` and `volume-wait` v0.19) are documented.
- Graphs: the 64-edge limit is per node (each link needs its own proxy port), with 1024 edges per graph; `kling compose` keeps the full project network and only cuts back services that would go over 64.
- New volumes are empty, without `lost+found`, like Docker's: Postgres `initdb` works on a volume root.
- Extensions exit with the core's codes (2 usage, 3 daemon too old, 4 stale golden, 128+signal when killed); `kling mcp heal` reports "still unhealthy" with 6 instead of 3.
- Gateway and scheduler errors (429 tenant quota, 401, 502) are JSON with a stable `code`.
- `kling ai retrain` takes `-cpus` and sizes for `-mem`; `-vcpus` exits 2.
- `kling try` checks its image exists before creating anything and suggests `kling image toolchain` or `-image min`; `kling save` skips the serving wait when the image declares no service.
- `kling exec`, `try` and `shell` say when a failed command was blocked by the egress policy.
- macOS: `topo`, `status` and `up` show hv/vz instead of KVM text, and the build 501 lists every rootless builder.
- `kling help` lists every flag each command defines (checked by a test).
- `POST /machines/{ref}/commit` and `POST /machines/{ref}/publish` answer 500 to host failures (they gave 400 to everything they did not recognise); commit gives 409 when the machine is not in a committable state, has session secrets, or the name exists
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

- kling-hv guests ran without KASLR. The FDT now carries `kaslr-seed` and `rng-seed` on every cold boot, and the guest kernel trusts the bootloader seed. Clones never boot again, so they don't reuse a seed.
- kling-hv with `KLING_VZ_MTU=65535` dropped every full-size frame and TCP stalled.
- The OCI builder now fsyncs the ext4 before renaming it into place.
- The `service` of a debian-built image didn't get `kling run -e` variables (`exec` did): the shell `/entrypoint` started it before the agent read them. The agent now supervises it from `/etc/kindling/service.json`, like a Docker image's, so its output is in `kling logs -service`. Rebuild an image to pick this up.
- A VMM that died right after launch cost 5 s of waiting for its socket and an error that said nothing. Start, thaw and restore now fail at once, quoting the last line of its console.

- `release.sh` only read a single-line `require`, so it saw no kindling version in `vz/go.mod` (a `require ( … )` block) and would refuse every tag.
- macOS (kling-hv, kling-vz): the VMM API socket only accepts the daemon's user.
- macOS (kling-hv): a failed balloon remap no longer ends the VM.
- A daemon that loses a guest's vsock (an older agent's listener died) goes straight to TCP after three failures within 30 s instead of paying a failed vsock connect on every connection, and tries vsock again once a minute.
- `exec` and `shell` answer 429 `guest_busy` with `Retry-After` when the guest agent is at its limit (64 commands, 8 shells), instead of 502 `bad_gateway`.
- kling-hv: a pause that times out wakes the device threads again (they could stay asleep with the VM running); device quiesce has the same 5 s deadline; CPU-ceiling pauses no longer quiesce devices; a snapshot right after a resume keeps the guest clock adjustment.
- A daemon restart no longer re-adopts the VMM a freeze left alive (the machine kept `warm` is no longer marked `failed`); a thaw that re-adopts a VMM watches it; a re-adopted VMM's death says why; a failed `start` of a `failed` machine keeps it `failed` with its dump when nothing was booted.
- The background clock resync takes the machine's lifecycle lock and skips a machine that was frozen, stopped or rebooted meanwhile; a freeze waits for it at most ~100 ms.
- The reason a VMM ended (e.g. the guest rebooted) is in the failure event; `kling start` clears the previous thaw timings; a freeze right after boot waits for the agent before asking it to flush its volumes.
- kling-bridge: `-session-idle 0` no longer panics, a failed listener cleans up its HTTP child, and the proxy session table is capped.
- Linux: removing a credential edge removes its DNAT/ACCEPT rules; a graph fork writes its fork mark before the template meta, like a machine fork.
- Bounded maps: the share watcher's echo marks, the MCP gateway usage memory, phone screenshots and sandbox in-flight counters.
- Guest agent: an `EINTR` or `ECONNABORTED` from the vsock `accept` killed its vsock listener until the machine restarted (seconds into any exec load on hv and vz); with the guest network up the daemon silently fell back to TCP, without it every exec failed. Images need the new agent to get the fix.
- kling-hv: restoring a 2+ vCPU snapshot taken while a vCPU was in the timer handler could kill the VMM (SIGTRAP from Hypervisor.framework, seen as `EOF` on `kling db fork`); vCPUs are now created in index order so each gets its own GIC redistributor back. Templates saved by older builds with 2+ vCPUs may need rebuilding. A VMM that dies during a restore is now reported as such, with its last console line.
- kling-hv: taking a vCPU offline and back online (CPU hotplug) crashed the guest kernel; CPU_ON now resets the vCPU's system registers.
- `exec` interrupted by a freeze, stop, removal or VMM death says so ("the machine was frozen while the command ran") instead of a raw `EOF` or a generic cut; before the headers or with `?wait=1` it is a 409.
- `exec` of a command that prints nothing for more than 5 min no longer fails with "timeout awaiting response headers": the daemon waits for the guest's headers as long as for the body (the command's timeout plus 30 s).
- kling-hv: the guest no longer sees SVE/SME on an M4 (it advertised sve2 without sve and OpenSSL died with SIGILL, #183 on hv).
- kling-hv: a freeze during a pending CPU_ON left that vCPU dead after restore; the balloon no longer writes guest RAM during a snapshot; a pause that cannot stop every vCPU gives up after 5 s with an error instead of hanging freeze/stop.
- kling-hv: a stalled console reader can no longer block a vCPU (output is dropped and counted); a timed-out vsock dial frees its slot; the network RX thread is stopped before its socket is closed; each snapshot state version must have its own header length.
- kling-hv: pause errors and the reason the VM ended (e.g. a guest reboot) are reported instead of "the VM has stopped".
- A dead VMM is noticed at once (it took 22-30 s), also for VMMs re-adopted after a daemon restart; `start` cold-boots a `failed` machine with its disk and `thaw` restores one whose dump is still valid; `start` no longer claims a cold boot it did not do.
- macOS and Linux: any process naming a machine's API socket (a `curl --unix-socket` polling it) was taken for its VMM, so a thaw could re-adopt it and the machine ended `failed`; only `--api-sock` processes count now, and a thaw kills a VMM its freeze left behind.
- A freeze whose seal could not be written left a `warm` machine that could never be thawed; it now fails the machine and keeps its disk.
- Two `commit`s with the same name from different machines no longer overwrite each other; the second gets a conflict.
- A crash between a fork's snapshot meta and its `fork-of` mark left an orphan fork nobody swept.
- `stop` of a machine removed while waiting for its lock reports not found instead of acting on a stale copy.
- Machine ids never share the 8-hex prefix that names their netns and cgroup with a live machine (a collision tore down the other machine's network).
- Linux: changing a graph removes the link DNAT/ACCEPT rules of edges that are gone or moved slot.
- DNS resolver handlers are bounded before they start and waited for on stop, so nothing seeds after teardown.
- kling-hv: a truncated or inconsistent `mem.file` is rejected with an error instead of crashing the VMM on restore; the GIC state size is capped.
- Guest agent: an unknown share event from a newer daemon is skipped instead of dropping the share session; `/exec` and `/exec/stream` are capped at 64 concurrent requests (429), and the 8-shell cap now actually applies; write-behind keeps at most 16 chunks in flight per file; the machine env and command are read from MMDS in one retry cycle (worst case ~12 s instead of ~23 s before `/healthz`).
- `kling status -json` exits 1 when the daemon does not answer, like the text output.
- `kling plugins` and every command no longer run `kling-hv` and `kling-unpack` as if they were extensions.
- `kling ai`: classify/decide and the guest pool have timeouts.
- Performance: an `exec` no longer rewrites the whole state on every TTL refresh (25-220 µs and up to 940 KB per exec with 100-500 machines, now 59 ns); the credential proxy redactor copies only when a secret matches and precomputes its variants (256 KiB body: 2.9-10.8 MB to 33 KB allocated); the MCP gateway probes a sticky instance once per request instead of twice; instance transports drop idle connections after 30 s and close them when the instance goes away.
- Tests: the contract now has a golden of every command's flags (`testdata/contrato/flags.txt`), the exit-code 5 wiring, the agent `cmd`/`volume-wait` capabilities, and the snapshot format constants against the vz sources.
- kling-hv: a short block write was reported as success to the guest; virtio queue sizes must be a power of two; a network RX chain shorter than the frame gets nothing instead of a cut header; the device-tree builder is bounds-checked; the `mem.file` fallback is 0600; PSCI CPU_ON reads registers under the lock.
- kling-hv vsock: connections the host closed fully kept their slot, so a guest could exhaust the 64 slots and stop `exec`.
- macOS: kling-hv and kling-vz start with a minimal environment instead of inheriting the daemon's (tokens included).
- A kling binary asked for its manifest no longer discovers extensions itself: a kling copy named `kling-*` in PATH made every command spawn hundreds of processes.
- Release signing keys made with `minisign -G -W` (unencrypted, all-zero checksum) were rejected; a test now signs and verifies against the real `minisign` both ways, and CI installs it.
- A `KLNGHV03` kling-hv snapshot was taken for a kling-vz one, so the refreeze check let it through for an older kling-hv.
- macOS: the daemon no longer warns about missing memory/pids cgroup controllers at every start.
- The arm64 `kling-bridge` binary is no longer tracked in git.
- Docs: no more pre-1.0 version banners, removed aliases or `-vcpus`; live shares are capped at 256 MiB/s, not 16; image builds work on macOS; stale paths and links; SECURITY.md describes DNS tunnelling through an allowlisted domain.
- e2fsck runs with `LC_ALL=C`, so its summary-only check matches on localised hosts.
- CI: the macOS job runs the whole `internal/machine` and `internal/daemon` suites, not only the clone tests.
- The swap admission error pointed to `docs/estabilidad.md`, now `docs/auditoria-estabilidad-2026-08.md`.
- CI: `e2e-linux.yml` installs the `base` builder for its private daemon, so the toolchain image builds and `90-e2e.sh` and `94-e2e-upgrade.sh` stop falling back to `min`; `94-e2e-upgrade.sh` and `95-e2e-upgrade-mac.sh` only expect a refreeze on `-rollback` when `state.json` was migrated (otherwise the machine stays `running`).

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

[Unreleased]: https://github.com/juan52878911/kindling/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/juan52878911/kindling/compare/v0.20.1...v1.0.0
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
