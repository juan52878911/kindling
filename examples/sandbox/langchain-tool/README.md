# An agent tool with LangChain

A LangChain tool, `run_python`, that runs the model's code in a kindling sandbox, and a
minimal agent (Claude + that tool) that uses it. The code never touches your machine: it runs
in a microVM with its own kernel and, by default, no network.

| File | What it is |
|---|---|
| `tool.py` | the tool; `python3 tool.py` tests it **without any LLM** |
| `agent.py` | the loop: model → tool call → result → model, with a limit of 5 turns |
| `run.sh` | the commands, in order |

Needs the gateway and the `python` template from the [quickstart](../../../docs/quickstart-sandbox.md)
(`KLING_SANDBOX_TOKEN` set and `kling sbx gateway` running).

```sh
./run.sh --dry-run     # prints the commands, runs nothing, calls no LLM
./run.sh               # template, pip install, the tool test, then the agent (ANTHROPIC_API_KEY)
```

Only `agent.py` calls an LLM, and only when you run it without `--dry-run`. The API key stays
on your side: the sandbox never sees it. The model name (`claude-sonnet-5-5`) is in `agent.py`.
This example has not been run against a live gateway in this repository's CI; the tool test
(`python3 tool.py`) is the quickest check.
