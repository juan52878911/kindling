---
name: doctor
description: Diagnoses a kindling installation from Claude Code — runs kling doctor, kling status and kling version, explains each failure and applies or hands over the exact fix. Use when kling fails, the kindling MCP server does not connect, a sandbox or service will not start, or the user asks whether kindling is healthy.
argument-hint: "[-v]"
allowed-tools: Bash(kling doctor*) Bash(kling status*) Bash(kling version*) Bash(kling context ls*) Bash(kling plugin ls*) Bash(kling mcp ls*) Bash(kling mcp health*) Bash(kling ps*) Bash(kling top*)
---

# kindling doctor

Speak the user's language. Run, in this order, and show the output:

```sh
kling doctor            # runtime, daemon, CLI vs daemon versions, extensions, completion; a fix: line per ✗
kling status -v         # which piece answers: daemon, gateway, agents (exit 1: no daemon)
kling version           # CLI and daemon versions must match at minor level
kling context ls        # which daemon the CLI talks to (local socket or ssh://)
```

If `kling` is not on PATH, try `~/.local/bin/kling`; if it is not there
either, the fix is `/kindling:setup`.

Then, only if relevant:

- MCP problems: `kling plugin ls` (is `mcp` there and `ok`?), `kling mcp ls`,
  `kling mcp health` (probes; wakes machines). A `kindling` server that fails
  to connect in Claude Code is almost always the gateway (not running, wrong
  `gateway.url`, or token mismatch): `kling connect -all` probes it with the
  stored token and says which. Never print the token; the fix for a mismatch
  is the user running `kling config set gateway.token …` themselves, copying
  it from the gateway host (`/etc/kling/gateway.env` on a deployed host).
- Machines that will not start: `kling ps -a`, `kling logs <ref>`,
  `kling top` (memory), `kling events`.

Apply the `fix:` lines that need neither sudo nor editing a managed rc; print
the others verbatim for the user to run. Finish with a three-line verdict:
what works, what is broken, and the one command to run next.
