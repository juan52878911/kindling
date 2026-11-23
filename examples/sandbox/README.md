# Sandbox examples

Five examples for the sandbox gateway and the core's sandboxes. Each has a README and a
`run.sh` whose `--dry-run` prints the commands and runs nothing: no daemon, no gateway, and no
LLM (the only one that ever calls an LLM is `langchain-tool/agent.py`, and only without
`--dry-run`). `./test-dry-run.sh` runs the five dry runs; CI does it on every push.

Start with the [quickstart](../../docs/quickstart-sandbox.md): it installs everything and builds
the `python` template ([`templates/python.json`](templates/python.json)) that most of these use.

| Example | What it shows |
|---|---|
| [`langchain-tool`](langchain-tool/README.md) | an agent tool: LangChain + Claude, with the model's code running in a sandbox |
| [`data-analysis`](data-analysis/README.md) | `/code`: a context that keeps its variables between calls, like a notebook |
| [`code-server`](code-server/README.md) | VS Code in the browser inside a sandbox, behind an endpoint with a token |
| [`fork-variants`](fork-variants/README.md) | fork a sandbox that did the expensive setup into N copies, one variant each |
| [`osb-code-interpreter`](osb-code-interpreter/README.md) | OpenSandbox's code-interpreter example against the gateway (unverified, says so) |

All the data in them is invented. None has been run end to end in this repository's CI, which has
no KVM; what CI checks is that the commands print and the files exist.
