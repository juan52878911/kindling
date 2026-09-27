---
name: setup
description: Installs kindling (the kling CLI) and the pieces the user picks — a local or remote daemon, MCP servers hosted as frozen microVMs and connected to Claude Code, sandboxes, the AI gateway, shell completion — asking first, with the measured benefit and the cost of each option. Use when the user runs /kindling:setup or asks to install, set up or reconfigure kindling.
disable-model-invocation: true
argument-hint: "[ssh://user@host] [all]"
allowed-tools: AskUserQuestion Bash(sh *preflight.sh*) Bash(kling doctor*) Bash(kling status*) Bash(kling version*) Bash(kling plugin ls*) Bash(kling context ls*) Bash(kling mcp ls*) Bash(kling mcp search*) Bash(kling ai ls*) Bash(kling config show*)
---

# kindling setup

You are installing kindling for the person in front of you. kindling runs MCP
servers, sandboxes and small AI models as Firecracker/vz microVMs that freeze
to disk (0 RAM) and wake in milliseconds. Speak the user's language. Never
print, echo or paste a gateway token: it lives in `kling config`
(`gateway.token`) and in `~/.claude.json` only, written by `kling connect`.

## Preflight

This ran before you read this file (if the block is empty, run
`sh "${CLAUDE_PLUGIN_ROOT}/scripts/preflight.sh"` yourself):

!`sh "${CLAUDE_PLUGIN_ROOT}/scripts/preflight.sh"`

Read it before asking anything: do not offer a local daemon on a machine that
cannot run one, do not reinstall what is already there at the same version,
and note whether the shell rc is managed (home-manager / read-only).

Arguments: `$ARGUMENTS`. `ssh://user@host` preselects a remote daemon; `all`
preselects every component. Still confirm with the questions below.

## 1. Ask what to install

Use **AskUserQuestion** (one call, several questions). Keep the descriptions
below: they carry the measured numbers from the repo (README and docs) and the
honest limits. Adapt the runtime options to the preflight (drop the ones that
cannot work here, say why in one line).

**Question 1 — header "Runtime", single choice.** "Where should microVMs run?"

- **Local daemon (this machine)** — "MicroVMs here, no server. macOS: Apple
  Silicon + macOS 14+, the vz backend ships with kling (`kling-vz`), needs
  `brew install e2fsprogs`, no root; but macOS builds no images: the kernel,
  the base image and any MCP image are copied from a Linux arm64 daemon
  (`kling image copy … -from ssh://…`), so without such a host pick Remote.
  Linux: needs `/dev/kvm` and nftables; `kling up` prints the sudo commands
  instead of running them, and builds images itself. Cost: 5–10 min once; a
  frozen machine then costs 0 RAM. Not possible on an Intel Mac or a Linux
  without KVM."
- **Remote daemon over SSH** — "kling talks to a Linux host with KVM through
  `ssh://user@host`, like `docker context`; the daemon never opens a network
  port. Nothing to virtualize here. Cost: a host that already runs `kling
  daemon` (deployed with `make deploy HOST=ssh://…`), and the gateway token
  copied once with `kling config set gateway.token`."
- **CLI only, decide later** — "Installs `kling` now; `kling up` or `kling
  context add` when you have a runtime. `kling ai chispa` works without any
  daemon."

**Question 2 — header "Components", multiSelect.** "Which pieces do you want?"

- **MCP servers (extension `mcp`)** — "Host MCP servers as frozen microVMs
  behind one gateway. A frozen tool thaws in ~30 ms and costs 0 RAM idle, vs
  a node/npx cold start of seconds per session; 142 microVMs fit in 3.9 GB.
  Claude gets one `_all` entry with `find_tools` / `describe_tool` /
  `call_tool`: ≈250 tokens of context instead of thousands (28 tools cost 4327
  expanded; the crossover is ~8 tools). Cost: two binaries (`kling-mcp`,
  `kling-bridge`), each server's image built once (1–3 min), a gateway running
  on the daemon host, its token in kling config."
- **Sandboxes (extension `sandbox`)** — "Throwaway microVMs for risky or
  untrusted code: `kling try -- cmd` boots, runs and deletes (19 ms end to end
  for an ephemeral action; a sandbox from a template starts in ~300 ms, five
  in parallel took 0.57 s). The `sbx` gateway adds tenants, quotas and a
  prewarmed pool: claiming one costs 16 ms vs 683 ms creating it. Cost: one
  binary; simple file edits stay faster natively — use it for what you would
  not run on your machine."
- **AI gateway (`kling ai`)** — "Built into kling, nothing to download.
  Chispa classifiers answer in 1.5–6 µs in-process with zero allocations;
  small LLMs (VON) and serverless Chispa replicas wake on demand and freeze
  when idle. Cost: an `ai.json` with your tasks and labelled data to train
  (`kling ai chispa train`); serving models needs a daemon."
- **Shell completion** — "`kling completion` for zsh/bash/fish, loaded from
  your rc. Cost: one line in your rc; if the rc is managed (home-manager) or
  read-only the installer prints the line instead of editing."

**Question 3 — only if MCP was chosen — header "MCP servers", multiSelect.**
"Which servers should the gateway host first?" Offer at most three concrete
ones plus "Other (tell me)". Before asking, run `kling mcp search filesystem`,
`kling mcp search memory` and `kling mcp search fetch` if `kling` and the
extension are already installed; otherwise offer them by name and resolve the
registry ids after installing. Prefer the official `io.modelcontextprotocol/*`
entry when the search returns one; the README's worked example is
`io.github.domdomegg/filesystem-mcp`. Describe each as "what it gives Claude"
+ "runs in its own microVM, no node on your machine".

If the remote runtime was chosen and no `ssh://` came in the arguments, ask
for the host with a question whose only option is "Other" (free text), or ask
in plain text.

## 2. Do it

Run commands with Bash and show their output. Ask before anything that
installs or writes outside the repo; Claude Code will prompt anyway.

### 2.1 Install kling and the extensions (official installer)

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh -s -- --with <mcp,sandbox as chosen>
```

- Omit `--with` if no extension was chosen. `--prefix DIR` if the user wants
  another directory (default `~/.local/bin`). `--tag vX.Y.Z` pins a release.
- Add `--no-rc` if the user did **not** pick shell completion: the installer
  then touches nothing outside `--prefix`.
- The installer verifies SHA256SUMS before moving anything, writes each
  binary atomically, installs `kling-vz` on Apple Silicon, and ends by
  printing what it could not do. If preflight said the rc is managed or
  read-only, the installer will not edit it: relay the lines it prints and
  tell the user where to add them (home-manager: `programs.zsh.initContent`).
- If `kling` is already installed at the latest release with the chosen
  extensions (`kling plugin ls`), skip this step and say so.
- If `~/.local/bin` is not in PATH, use the full path `~/.local/bin/kling`
  for the rest of this session and show the `export PATH=…` line once.

### 2.2 Runtime

**Local, macOS (Apple Silicon):**

1. `brew install e2fsprogs` if preflight found no e2fsprogs.
2. `kling up -check` — it lists what is missing and the exact fix for each.
3. `kling up`. If it says there is no launchd agent: offer to install one so
   the daemon starts at login. Generate it with
   `sh "${CLAUDE_PLUGIN_ROOT}/scripts/launchd-plist.sh" > ~/Library/LaunchAgents/dev.kindling.daemon.plist`
   (absolute paths, from docs/mac.md), then
   `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.kindling.daemon.plist`
   and `kling up` again. If they prefer not to, `kling daemon` in a terminal
   is enough for now.
4. `kling status` must show the daemon responding (backend vz).
5. Images: `kling up -check` will list the guest kernel and `min.ext4` as
   missing until they are copied from a Linux **arm64** daemon:
   `kling image copy min -from ssh://user@linux-arm64-host` (streams kernel,
   base and recipe daemon to daemon; docs/mac.md). Without such a host the
   Mac daemon cannot run anything yet: say so and offer the Remote runtime.

**Local, Linux with KVM:** `kling up -check`, then `kling up`. It **prints**
the commands that need root (nftables, the `kindling` user, the systemd
unit) instead of running them: show them to the user verbatim, ask them to
run them in their terminal, then run `kling up` again and `kling status`.

**Remote:** `kling context add <name> ssh://user@host` (name it after the
host, e.g. `lab`), then `kling status`. If the daemon there is missing, the
fix is on that host: `make deploy HOST=ssh://user@host` from a clone of the
repo (docs/README). Do not try to deploy from here unless asked.

**CLI only:** nothing; mention `kling up` / `kling context add` for later.

### 2.3 MCP servers

Needs a reachable daemon (`kling status`). Then:

1. Gateway. `kling status` shows whether one answers at `gateway.url`.
   - Remote daemon: the gateway usually runs there (`make -C ext/mcp deploy`
     set it up, unit `kling-gateway.service`). The CLI needs its token: tell
     the user to run, themselves, in their terminal:
     `kling config set gateway.token "$(ssh user@host 'sudo cut -d= -f2 /etc/kling/gateway.env')"`
     You never run that command and never ask them to paste the token.
   - Local daemon: `kling mcp serve -listen 127.0.0.1:8080 -idle 5m` generates
     the token the first time and stores it in `gateway.token`. It must keep
     running: on macOS start it in a terminal (or a second launchd agent like
     the daemon's, with `mcp serve -listen 127.0.0.1:8080` as arguments); on
     Linux `kling up` starts `kling-gateway.service` when it is installed.
2. Add each chosen server: `kling mcp add <registry-id>` (builds the image
   and captures the catalog; 1–3 minutes each, show progress). On Apple
   Silicon add `-bundle` for node servers: it takes a cold `initialize` from
   ~16 s to ~2.5 s. A local macOS daemon cannot build: run `kling mcp add`
   against the Linux arm64 daemon (`-H ssh://…`), then
   `kling image copy <service> -from ssh://…` and `kling mcp import <service>
   -image <service>` on the Mac (docs/mac.md).
3. `kling mcp ls` — every service with its tool count and health; `kling mcp
   health` probes the ones never probed.
4. Connect to Claude Code: `kling connect -all -install claude-code`. It
   registers one HTTP MCP server named `kindling` at user scope
   (`claude mcp add --scope user --transport http … --header "Authorization: Bearer …"`
   when `claude` is on PATH, else it patches `~/.claude.json` with a backup)
   and prints the token savings of the proxy mode for this catalog. Tell the
   user to restart Claude Code or run `/mcp` to see `kindling` with
   `find_tools`, `describe_tool`, `call_tool`.

### 2.4 Sandboxes

The extension is already in place from 2.1 (`kling plugin ls` shows
`sandbox`). With a daemon: `kling try -- uname -a` as the smoke test. Explain
the two shapes: `kling try` / `kling sandbox create` (core, no gateway), and
`kling sbx gateway` for tenants, templates and the prewarmed pool
(ext/sandbox/README.md). Templates with dependencies preinstalled:
`kling run -allow-exec -name t -image toolchain`, install inside, `kling save
t <name>`; then `kling sandbox create -from <name>` starts in ~300 ms.

### 2.5 AI gateway

Nothing to install: `kling ai ls` works once `kling` is there. Point to
docs/ai-gateway.md and docs/chispa.md; the shortest path is `kling ai chispa
train -data d.jsonl -o m.chispa` then `kling ai chispa predict -model m.chispa
-text "…"`. With a daemon, `kling ai up` serves `/v1/classify` and
`/v1/decide` on 127.0.0.1:8080 from `./ai.json` or `~/.config/kling/ai.json`.

### 2.6 Doctor

Always finish with `kling doctor`. It prints a `fix:` line for every ✗; run
the fixes you can (no sudo, no rc edits) and hand the rest to the user as
exact commands. A warning about completion not loaded in this shell is fine.

## 3. Report

End with a short summary: what got installed and where, what runs (daemon,
gateway, services), what the user still has to do by hand (sudo commands, rc
lines, token copy), and the three commands they will use most:
`kling try -- <cmd>`, `kling mcp ls`, `kling doctor`. Mention that
`/kindling:usage` tells Claude when to reach for kindling and
`/kindling:doctor` diagnoses it.
