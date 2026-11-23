# OpenSandbox's code-interpreter example, against the gateway

The gateway implements the code interpreter of OpenSandbox's execd: `/code` and
`/code/contexts`, with Python, bash and JavaScript, contexts that keep state, and the same
event stream (`stdout`, `stderr`, `result`, `error`, `done`). This example runs OpenSandbox's
own `code-interpreter` example, unmodified, with its SDK pointed at the gateway through
environment variables only.

```sh
./run.sh --dry-run     # prints the commands, runs nothing
./run.sh               # clones OpenSandbox, installs its SDKs, runs its example
```

## Status: not verified

Be careful with this one. This repository's development machine has no network, so:

- the **variable names** (`SANDBOX_DOMAIN`, `SANDBOX_API_KEY`) and the **path** of the example
  inside the OpenSandbox repository (`examples/code-interpreter/main.py`) are written from
  memory of that project and have to be checked against its README before you rely on them;
- the OpenSandbox SDK creates its sandbox through OpenSandbox's lifecycle API. The gateway's
  `/v1/sandboxes` is similar in spirit but is not promised to be wire-compatible; what the
  gateway does promise is the execd routes above, which its contract tests cover with its own
  SDKs. Whether the unmodified OpenSandbox SDK completes `create` against it has **not** been
  tried. If it does not, the part that matters, `/code`, can be called directly (see
  [`../data-analysis`](../data-analysis/README.md)), and the E2B SDKs do work through
  `kling sbx e2b` ([`docs/e2b.md`](../../../docs/e2b.md)).

When someone runs it with the real SDK, the result (works, or the first call that fails)
belongs in this README.
