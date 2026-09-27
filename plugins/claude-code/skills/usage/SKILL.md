---
name: usage
description: When and how to use kindling from Claude Code — a throwaway microVM for risky or untrusted commands (kling try, kling sandbox), parallel isolated environments from templates, MCP servers hosted behind the kindling gateway (find_tools / describe_tool / call_tool), and status/doctor when something fails. Use whenever a task involves running untrusted code, installing packages to test something, isolating parallel agents, calling a tool the kindling MCP server exposes, or when kling or the kindling MCP server misbehaves.
---

# Using kindling

kindling runs code in Firecracker/vz microVMs that freeze to disk (0 RAM, no
process) and thaw in ~30 ms. `kling` is the CLI; the daemon may be local or
behind `ssh://` (`kling context ls`). Check once per session that it answers:
`kling status` (exit 1 means no daemon: see "When it fails").

## Reach for a sandbox when

- The command is **risky or untrusted**: a script from the internet, a
  `curl | sh`, a package's postinstall, a test suite that deletes or writes
  broadly, anything you would not run on the user's machine.
- You need a **clean environment**: "does it build from scratch", a different
  distro, a tool the host lacks, or throwaway installs.
- Several agents or steps need **isolated copies** of the same setup.

Do **not** use it for ordinary file edits, greps or builds in the user's
checkout: native is faster and the result lands where they work.

```sh
kling try -- <cmd>                      # boot, run, delete; exit code is the command's (19 ms end to end)
kling try -image toolchain -- sh -c '…' # image with npm and pip
kling try -share .:/src:copy -- make    # the working dir inside, as a read-only copy (ro | rw for live)
kling try -egress internet -- <cmd>     # network is off by default; or -egress allowlist -allow example.com
kling try -keep -- <cmd>                # keep it to inspect: kling ps, kling shell <ref>, kling rm <ref>
```

Longer-lived sandboxes and parallel environments:

```sh
kling run -allow-exec -name t -image toolchain     # prepare once…
kling exec t -- npm ci                             # …install what the task needs
kling save t deps-node                             # a template: starts in ~300 ms with it inside
kling sandbox create -from deps-node -ttl 10m      # five in parallel took 0.57 s
kling exec <sb> -w /src -- npm test                # stdout/stderr separated, remote exit code
kling cp ./patch.diff <sb>:/src/  ·  kling cp <sb>:/src/out.log -
kling sandbox ls · renew · rm
```

Memory is not the lever: a frozen sandbox is a file. If the pool of
prewarmed sandboxes is served by `kling sbx gateway`, claiming one costs
16 ms instead of 683 ms creating it (ext/sandbox/README.md).

## Hosted MCP servers (the `kindling` MCP server)

If Claude Code lists an MCP server named `kindling`, it is kindling's gateway
`_all` endpoint: every hosted server behind three meta-tools. The
`initialize` instructions already list every tool grouped by service, so:

1. `call_tool` with `service.tool` (e.g. `filesystem.read_text_file`) when
   you know the name and its arguments.
2. `describe_tool` first when you do not know the arguments.
3. `find_tools` only to search by keyword.

Each call thaws the microVM if needed (~30 ms) and freezes it when idle; a
service marked `[remembers between calls]` keeps state. To add a server:
`kling mcp search <term>`, `kling mcp add <id>`, then it appears in the
inventory at the next session (`/mcp` reconnects). `kling mcp ls` shows tool
counts and health; `kling mcp health` probes them.

## AI gateway (optional)

`kling ai` serves tiny local decisions: Chispa classifiers in 1.5–6 µs,
small LLMs on demand. `kling ai ls` lists tasks; `kling ai test <task>
"text"` classifies one; `kling ai up` serves `/v1/classify` and
`/v1/decide`. Use it when the user has trained a task; do not invent one.

## The `kindling-dev` agent

For anything longer than one command, delegate to `@kindling:kindling-dev`:
it knows the playbook — risk before editing (chrono hotspots/coupling/owners
+ codegraph callers), untrusted code in `kling try`, tests and N variants in
parallel sandboxes from a template, hosted MCP through the gateway's
meta-tools, Chispa classifiers trained from chrono data with the evaluation
gate, and the UltraMemory layers (codegraph, graphify, engram, vault,
sesiones, chrono) when the gateway hosts them. chrono (≥ 0.2.0) answers for
any repo with `repo: "name@branch"` and accepts codegraph ids
(`path#symbol`); `chrono export-dataset` feeds `kling ai chispa train`. It
says when native is faster and cites the docs for every number.

## When it fails

- `kling status` (exit 1 = no daemon), `kling status -v`, `kling doctor`
  (a `fix:` line per ✗), `kling version` (CLI vs daemon), `kling context ls`.
- A `try:` line under any error is the next command to run.
- Machines: `kling ps -a`, `kling logs -f <ref>`, `kling inspect <ref>`,
  `kling events`. Memory: `kling top`.
- MCP: `kling mcp health`, `kling mcp heal`, `kling connect <service>` (it
  probes the gateway with the real token and prints why it fails).
- `/kindling:doctor` runs the checks and explains them; `/kindling:setup`
  installs or repairs.
