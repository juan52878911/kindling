# Contributing to kindling

Thanks for looking. kindling is small, measured and opinionated; contributions that keep
it that way are welcome.

## Before you start

- Read [SECURITY.md](SECURITY.md): the guest is assumed hostile, and every change is judged
  against that.
- Read the [handbook](docs/handbook.md) section for the piece you are touching. Most
  design decisions are written down with the number that justified them.
- Open an issue first for anything that changes behaviour, the CLI or the daemon API. The
  CLI's source of truth is [`cmd/kling/tree.go`](cmd/kling/tree.go); every command in the
  docs must exist there or in an extension's manifest.

## Building and testing

Go 1.26 with a workspace: the core at the root, the extensions in `ext/mcp` and
`ext/sandbox`, and the macOS backend in `vz/` (a separate cgo module).

```sh
make install                    # kling on your PATH, no sudo
go build ./... && go vet ./...
gofmt -l .                      # must print nothing
go test -race ./...             # the core; also in ext/mcp and ext/sandbox
make vz                         # macOS only: builds and signs kling-vz
```

Tests that need a Linux host with KVM are skipped elsewhere; the end-to-end scripts
(`scripts/90-e2e.sh`, `scripts/92-e2e-mac.sh`) are how a branch is verified against a real
daemon before merging. If your change affects a measured number, re-run the script that
produced it and update the document that quotes it.

## Style

- Go: standard `gofmt`, no new dependencies in the core (it has none). Errors that a user
  can act on end with a `try:` line naming the next command.
- CLI strings and help are in English. Documentation under `docs/` is in Spanish unless a
  file already exists in English (`docs/guides/`, `docs/handbook.md`, `docs/compare.md`,
  `docs/resources.md`); the README is bilingual and both versions change together.
- Numbers in docs are measured, never estimated. Say where and on what hardware.
- Commits: short, in Spanish, one topic each (`daemon: …`, `kling-mcp: …`, `docs: …`), no
  footers or signatures.

## Pull requests

- Branch from `main`. Keep the diff to one topic; split otherwise.
- Update `CHANGELOG.md` under the unreleased section when behaviour changes.
- CI runs build, vet, tests and cross-compilation for every platform in a release.

## License

By contributing you agree that your contributions are licensed under the
[Apache License 2.0](LICENSE), like the rest of the project. There is no CLA.
