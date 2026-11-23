# code-server behind an endpoint

VS Code in the browser, running inside a sandbox, reachable through an **endpoint** of the
gateway: the port of the sandbox is published as `<port>-<id>.<domain>`, with HTTP and
WebSocket, and protected by a token.

| File | What it is |
|---|---|
| `code-server.json` | the template: `toolchain` plus code-server (installed at build time, with internet) |
| `serve.py` | creates the sandbox, starts code-server as a background process, publishes port 8080 |
| `run.sh` | the commands, in order |

The gateway has to serve endpoints, which needs a domain with wildcard DNS and TLS
([`docs/sandbox-api.md`](../../../docs/sandbox-api.md), "Entrada HTTP a puertos del sandbox"):

```sh
kling sbx gateway -ingress-domain sbx.example.com -tls-cert wild.pem -tls-key wild.key
./run.sh --dry-run     # prints the commands, runs nothing
./run.sh               # builds the template (a few minutes, the first time), then serves
```

`serve.py` prints the endpoint, whose `url` and `token` open the editor in a browser
(`?kling_token=…` once; then a cookie). `--auth none` is deliberate: the endpoint token is the
protection, so do not publish the port with `public=True`.

What this example does not promise: the template installs code-server from its public install
script at build time, without a pinned version or checksum; pin a release in the recipe before
using it for anything that matters. It has not been run end to end in this repository.
