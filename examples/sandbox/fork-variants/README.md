# Fork a sandbox to try N variants

`kling sandbox fork` takes a snapshot of a running sandbox (disk **and** memory) and starts N
copies from it in milliseconds. The expensive part (clone, install, warm caches, load a model)
happens once, in the base; each copy tries one variant, and none sees what another did. The
snapshot goes away with the last copy.

The script builds a toy project in `base`, forks it three times and runs the same function
with a different `FACTOR` in each copy:

```sh
./run.sh --dry-run     # prints the commands, runs nothing
./run.sh               # needs the daemon (kling up); no gateway
```

Expected: `FACTOR=2 20`, `FACTOR=3 30`, `FACTOR=4 40`, one per copy.

This uses the core's `kling sandbox` (a host-local sandbox), not the multi-tenant gateway: the
gateway's `/v1` API has no fork (the E2B shim answers `501` to it). A copy of a copy works the
same way. Limits and the `-ttl`/`-on-ttl` flags: `kling sandbox fork -h`; background in
[`docs/exec-sandbox.md`](../../../docs/exec-sandbox.md) and [`docs/cow.md`](../../../docs/cow.md).

This example has not been run end to end in this repository's CI (it needs KVM).
