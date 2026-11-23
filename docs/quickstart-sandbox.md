# Quickstart: your first sandbox

From nothing to a code agent's sandbox in about ten minutes, on a Linux host with KVM
(amd64 or arm64). You will install kindling, start the sandbox gateway, create a sandbox from a
template, and run code in it with `curl`, the Python SDK, the TypeScript SDK and an E2B SDK.

The commands below are the same, line by line, as `scripts/quickstart-sandbox.sh --dry-run`
(a test keeps them equal). `--dry-run` prints them and runs nothing; `--run` runs them.

> **What this guide has and has not been checked on.** The commands and the files they use are
> checked by tests that need no VM (this document equals the script, the template parses, the
> links resolve). Running the whole path end to end needs a KVM host and has not been done for
> this version of the guide: if a step fails, the output of `kling doctor` says what is missing.

<!-- quickstart:begin -->

## 1. Install

Install kindling and the `sandbox` extension from the same release, start the daemon (as a
systemd service, with Firecracker, the jailer and the `kindling` users), and build the
`toolchain` image (Node, npm, Python 3 and pip) that the template starts from:

```sh
# 1. Install, start the daemon, build the image the template starts from
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sudo sh -s -- --tag v1.0.0-rc.3 --with sandbox
sudo kling up -install
kling image toolchain
```

`kling doctor` checks the daemon and the extension and gives the fix for each failure.

## 2. A token and the gateway

The daemon listens only on a Unix socket and is root on its host, so it gets a gateway in
front of it that checks a token. A token is a tenant: it only sees its own sandboxes.

```sh
# 2. A token and the gateway
export KLING_SANDBOX_TOKEN=$(openssl rand -hex 24)
kling sbx gateway &
```

The gateway listens on `127.0.0.1:8090` over plain HTTP, which is allowed only on loopback. To
serve a network, give it a certificate (`-tls-cert`, `-tls-key`); see
[`ext/sandbox`](../ext/sandbox/README.md). Every client below reads the same two variables,
`KLING_SANDBOX_URL` (default `http://127.0.0.1:8090`) and `KLING_SANDBOX_TOKEN`.

## 3. The template

A template is a recipe: which image to start from and what to install. From it kindling takes a
golden snapshot, and every sandbox is born from that snapshot in milliseconds. The recipe for
`python` is in this repository:

```sh
# 3. The template (this repository ships the recipe)
git clone --depth 1 https://github.com/juan52878911/kindling
cd kindling
kling sbx template apply -f examples/sandbox/templates/python.json
```

`kling sbx template ls` shows it once built. The recipe is `examples/sandbox/templates/python.json`:
the `toolchain` image, 512 MiB, one warm instance ready in the pool.

## 4. Your first sandbox, with curl

```sh
# 4. First sandbox, with curl
ID=$(curl -s -H "Authorization: Bearer $KLING_SANDBOX_TOKEN" -d '{"template":"python","ttl_seconds":600}' http://127.0.0.1:8090/v1/sandboxes | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
curl -s -H "Authorization: Bearer $KLING_SANDBOX_TOKEN" -d '{"cmd":["python3","-c","print(6*7)"]}' "http://127.0.0.1:8090/v1/sandboxes/$ID/exec?wait=1" | python3 -c 'import base64,json,sys; print(base64.b64decode(json.load(sys.stdin)["stdout"]).decode(), end="")'
curl -s -X DELETE -H "Authorization: Bearer $KLING_SANDBOX_TOKEN" "http://127.0.0.1:8090/v1/sandboxes/$ID"
```

The second command prints `42`. In the JSON reply `stdout` and `stderr` are base64, which is
why it is decoded. The sandbox id is `host/machine`; the slash goes into the URL as it is. The
whole API is in [`sandbox-api.md`](sandbox-api.md).

## 5. With the Python SDK

```sh
# 5. With the Python SDK
pip install ./sdks/python
python3 -c 'from kindling_sandbox import Sandbox; sb = Sandbox.create(template="python", ttl=600); print(sb.exec(["python3", "-c", "print(6*7)"]).stdout, end=""); sb.delete()'
```

The package is not on PyPI yet, so it is installed from the checkout. Standard library only,
Python 3.9 or later. More in [`sdks/python/README.md`](../sdks/python/README.md).

## 6. With the TypeScript SDK

```sh
# 6. With the TypeScript SDK
export KINDLING=$PWD
cd "$(mktemp -d)"
npm install "$KINDLING/sdks/typescript"
node --input-type=module -e 'import { Sandbox } from "@kindling/sandbox"; const sb = await Sandbox.create({ template: "python", ttl: 600 }); const r = await sb.exec(["python3", "-c", "print(6*7)"]); console.log(r.stdout); await sb.delete();'
cd "$KINDLING"
```

Also installed from the checkout for now; no dependencies, Node 18 or later. More in
[`sdks/typescript/README.md`](../sdks/typescript/README.md).

## 7. With an E2B SDK, through the shim

`kling sbx e2b` speaks E2B's protocol and translates it to the same gateway, so code written
for an E2B SDK runs against kindling by changing environment variables. The E2B API key is the
tenant's token:

```sh
# 7. With an E2B SDK, through the shim
pip install e2b
kling sbx e2b -listen 127.0.0.1:3000 &
E2B_API_KEY=$KLING_SANDBOX_TOKEN E2B_API_URL=http://127.0.0.1:3000 E2B_SANDBOX_URL=http://127.0.0.1:3000 python3 -c 'from e2b import Sandbox; sb = Sandbox.create("python"); print(sb.commands.run("python3 -c \"print(6*7)\"").stdout, end=""); sb.kill()'
```

What the shim supports and what answers `501` (pause, fork and templates are managed with
`kling sbx`, not through E2B): [`e2b.md`](e2b.md).

<!-- quickstart:end -->

## Coming from OpenSandbox

The gateway's `/v1` API follows OpenSandbox's execd for the code interpreter: `/code` and
`/code/contexts`, with Python, bash and JavaScript. The shim in this repository speaks **E2B**,
not OpenSandbox's lifecycle API, so the OpenSandbox SDK's `Sandbox.create` is not what the
shim translates. [`examples/sandbox/osb-code-interpreter`](../examples/sandbox/osb-code-interpreter/README.md)
runs OpenSandbox's code-interpreter example against the gateway and says exactly what has and
has not been verified. For the measured differences read [`benchmarks.md`](benchmarks.md),
including where OpenSandbox is ahead.

## What to try in the fifth minute

Two things OpenSandbox does not have, both in [`examples/sandbox`](../examples/sandbox/README.md):

- **Fork** a sandbox that already did the expensive setup into N copies and try a variant in
  each: [`fork-variants`](../examples/sandbox/fork-variants/README.md).
- **Pause with memory**: `sb.pause()` and `sb.resume()` keep the processes and the memory, not
  only the disk.

The other examples: an agent tool with LangChain
([`langchain-tool`](../examples/sandbox/langchain-tool/README.md)), data analysis with `/code`
([`data-analysis`](../examples/sandbox/data-analysis/README.md)) and code-server behind an
endpoint ([`code-server`](../examples/sandbox/code-server/README.md)).

## Clean up

`fg` brings each background service back to stop it with Ctrl-C (or `kill %1 %2`). Sandboxes
expire by themselves: `ttl` is inactivity time and, when it runs out, the sandbox is frozen
(`on_ttl`). `kling sbx ls` and `kling sbx rm <id>` show and delete them.
