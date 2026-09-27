# Resources

Everything around kindling that is not a guide: where the numbers come from, what to read
next, what to try, and where to ask.

## Documentation

- [README](../README.md) ([Español](../README.es.md)) — the short version.
- [`handbook.md`](handbook.md) ([es](handbook.es.md)) — the long version, with transcripts.
- [`guides/`](README.md#qué-guía-necesito) — task-oriented, copy-pasteable.
- [`api.md`](api.md) — the daemon's HTTP API. [`extensions.md`](extensions.md) — the
  extension protocol.
- [`compare.md`](compare.md) — kindling next to E2B, Daytona, Morph, Docker Sandboxes,
  microsandbox, ToolHive, Kata, llama-swap… with sources (September 2026).

## Benchmarks and audits

Every number in the README links to the document that measured it. The scripts that
reproduce them:

| What | Script | Numbers in |
|---|---|---|
| cold boot, snapshot, restore | [`scripts/40-bench-boot.sh`](../scripts/40-bench-boot.sh) | [`handbook.md`](handbook.md#measured-numbers) |
| wake-up, phase by phase (frozen and paused) | [`scripts/99-thaw-bench.sh`](../scripts/99-thaw-bench.sh) | [`despertar.md`](despertar.md) |
| density and stability under load, 142 microVMs | [`scripts/95-stress.sh`](../scripts/95-stress.sh), [`scripts/60-stress.sh`](../scripts/60-stress.sh) | [`estabilidad.md`](estabilidad.md) |
| VON: cold vs thaw to first token, tokens/s, memory of N replicas | [`scripts/96-von-bench.sh`](../scripts/96-von-bench.sh), [`scripts/97-von-cpu-bench.sh`](../scripts/97-von-cpu-bench.sh) | [`von.md`](von.md), [`von-cpu.md`](von-cpu.md) |
| the sentence encoder | [`scripts/98-encoder-bench.sh`](../scripts/98-encoder-bench.sh) | [`codificador.md`](codificador.md) |
| Chispa on 4,304 real commits | [`scripts/chispa-eval.sh`](../scripts/chispa-eval.sh) | [`CHISPA-EVAL.md`](CHISPA-EVAL.md) |
| layered images, 1300 → 433 MiB | — | [`three-layers.md`](three-layers.md) |
| end to end (Linux, macOS, Kubernetes) | [`scripts/90-e2e.sh`](../scripts/90-e2e.sh), [`scripts/92-e2e-mac.sh`](../scripts/92-e2e-mac.sh) | — |

Hardware behind them: a Proxmox lab on an Intel i7-8700T (nested for the early numbers,
an LXC container with native KVM for `von.md`, `despertar.md` and `chispa-serverless.md`),
and an Apple M4 for the macOS figures. The docs say which one each table used.

## Examples

Programs that *use* kindling, not parts of it:

- [`examples/ci-triage`](../examples/ci-triage) — CI failure triage: Chispa scores every
  line of a failed log and names the category; VON reads only the chunk it is unsure about;
  a local page lets a person confirm and exports labels for retraining. Evaluation:
  [`CI-TRIAGE-EVAL.md`](CI-TRIAGE-EVAL.md).
- [`examples/domotica`](../examples/domotica) — a smart-home room driven by voice commands
  as text, in Spanish or English, through the AI gateway's intent tasks with every layer's
  decision, latency and microVM shown on the page. A standalone program
  (`kindling-domotica`, `make domotica`), **not** a `kling` subcommand. Guide:
  [`demo-domotica.md`](demo-domotica.md).
- [`examples/hello-extension`](../examples/hello-extension) — the smallest extension, with
  a test of its manifest. Guide: [Writing an extension](guides/writing-an-extension.md).
- [`examples/mcp`](../examples/mcp) — an MCP client with a tool-calling loop against a local
  model (`agent.py`), plus the tiny stdio and HTTP servers used in the transcripts.

## Releases

- [GitHub Releases](https://github.com/juan52878911/kindling/releases): `kling` for
  linux/amd64, linux/arm64, darwin/amd64, darwin/arm64; the extensions (`kling-mcp`,
  `kling-sandbox`) and companions for the same platforms; `kling-vz-darwin-arm64`; the host
  tarballs for MCP and sandboxes; the operator manifests; one `SHA256SUMS`.
- The operator image: `ghcr.io/juan52878911/kindling-operator:<version>`.
- [CHANGELOG](../CHANGELOG.md) — every version. [`releases.md`](releases.md) — how a
  release is cut.
- The Claude Code plugin: `/plugin marketplace add juan52878911/kindling`.

## Community

- Questions and bug reports: [GitHub issues](https://github.com/juan52878911/kindling/issues/new/choose)
  (the bug template asks for `kling doctor` output).
- Security: see [SECURITY.md](../SECURITY.md) for how to report privately.
- Contributing: [CONTRIBUTING.md](../CONTRIBUTING.md). License: Apache-2.0.
- Related projects by the same author: [chrono](https://github.com/juan52878911/chrono)
  (git history as instant, bounded JSON; offered by the Claude Code plugin's setup).
