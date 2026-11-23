# Data analysis with `/code`

`/code` is the gateway's code interpreter (the same routes as OpenSandbox's execd:
`/code/contexts`, `/code`). A **context** keeps its variables between calls, so a script can
work like a notebook: load, aggregate, compute, each step seeing the last one.

`analyze.py` uploads an invented sales CSV, opens a Python context and runs three steps. The
sandbox needs only the standard library, so the `python` template is enough. It calls no LLM.

```sh
./run.sh --dry-run     # prints the commands, runs nothing
./run.sh               # needs the gateway and KLING_SANDBOX_TOKEN, as in the quickstart
```

Expected output (the sums per region, then the best region):

```
6
{'norte': 24.0, 'sur': 22.5, 'este': 10.0}
norte
```

(`24.0 = 120·0.10 + 80·0.15`; the values are computed by hand from the CSV in `analyze.py`.)
With `language="javascript"` the same API works if the template has `node` (the `toolchain`
image does); with `bash` it keeps variables, functions and the current directory.

Needs the gateway and the `python` template from the [quickstart](../../../docs/quickstart-sandbox.md).
