# Safe sandboxes for AI agents

An agent writes a script and needs to run it without touching your machine. kindling gives
it a throwaway microVM: no network unless asked, its own kernel, streaming output, a
lifetime after which it destroys itself. Templates make the next one start in ~300 ms;
several can run in parallel.

Design and the `allow_exec` gate: [`docs/exec-sandbox.md`](../exec-sandbox.md).

## 1. One sandbox, by hand

```sh
kling image toolchain                             # once per daemon: node, npm, python3, pip
kling sandbox create -image toolchain -name sb    # cold boot, a few seconds
kling cp ./analysis.py sb:/tmp/
kling exec sb -- python3 /tmp/analysis.py         # output arrives as it is produced
kling cp sb:/tmp/result.json .
kling sandbox rm sb
```

`exec` exits with the remote command's exit code, keeps stdout and stderr apart, takes
stdin with `-i`, sets a working directory with `-w` and environment with `-e`, and kills the
whole process group on `-timeout` (5 m by default, 1 h max). `kling shell sb` opens a real
terminal inside: history, `vim`, Ctrl-C reaching the program instead of the session.

The shortest form of all of the above is `kling try`:

```sh
kling try -- python3 -c 'print(2**64)'            # create, run, exit code, delete
kling try -image toolchain                        # a shell, deleted on exit; -keep keeps it
```

## 2. Network: none by default

```sh
kling sandbox create -image toolchain -name sb                       # egress none
kling sandbox create -image toolchain -name sb -egress internet      # out, never to private ranges
kling sandbox create -image toolchain -name sb -egress allowlist -allow pypi.org,files.pythonhosted.org
```

`allowlist` is fail-closed: only those domains, resolved dynamically, and nothing else.
An unknown egress value is an error, not a fall-through.

## 3. Templates: prepare once, restore in ~300 ms

Installing dependencies in every sandbox is slow. Prepare one machine with exec allowed,
save it, and make sandboxes from it:

```sh
kling run -image toolchain -name tmpl -allow-exec -egress internet -mem 1G
kling exec tmpl -- npm install -g typescript
kling save tmpl ts
kling sandbox create -from ts -name sb                 # ~300 ms, tsc already inside
kling cp ./a.ts sb:/tmp/
kling exec sb -- sh -c 'cd /tmp && tsc a.ts && node a.js'
```

Exec is decided at boot (`-allow-exec` on `run`; `sandbox create` always sets it), travels
in the kernel command line only the host writes, and is frozen with the memory: a template
without it cannot produce sandboxes (`409`), and a service microVM never has it. Each
restored sandbox wakes with the host's clock and fresh entropy, not the template's.

## 4. Parallel sandboxes

Every sandbox from a template shares its memory copy-on-write, so N of them cost far less
than N cold boots. Five in parallel from one template took 0.57 s in the lab
([`docs/exec-sandbox.md`](../exec-sandbox.md)):

```sh
for i in 1 2 3 4 5; do
  kling sandbox create -from ts -name sb$i -q &
done; wait
for i in 1 2 3 4 5; do
  kling exec sb$i -- sh -c "echo variant $i; node -e 'console.log(process.pid)'" &
done; wait
kling sandbox rm -f sb1 sb2 sb3 sb4 sb5
```

`-q` prints only the id. Use it to run test variants, evaluate N candidate patches, or give
each agent turn its own machine.

## 5. Your repository inside

```sh
kling sandbox create -image toolchain -name sb -share ./repo:/work       # a read-only copy, uploaded
kling exec sb -w /work -- python3 -m pytest -q
```

`copy` (the default) works with any daemon, local or over SSH. `ro`/`rw` serve a folder that
lives on the daemon host, live, under `daemon.share_roots`; the guest speaks FUSE and the
daemon serves each operation through `os.Root`, so nothing escapes the folder. A sandbox
made with `-from` cannot take shares (a template cannot carry them). Details:
[`docs/compartir.md`](../compartir.md).

## 6. Lifetime

A sandbox lives `-ttl` (10 m by default, 24 h max) and is **destroyed** when it runs out,
unless `-on-ttl freeze`, which sleeps it at zero cost and lets the next `exec` wake it in
milliseconds. `kling sandbox renew sb -ttl 30m` extends it; `kling sandbox ls` shows what is
alive and for how long.

## 7. Many agents, many hosts: the sandbox frontal

When more than one tenant or more than one host is involved, the `sandbox` extension puts
an authenticated HTTP frontal in front of the daemon(s): tenants with tokens and quotas,
templates as a recipe (rebuilt on any host after a reboot invalidates its snapshot), and a
pool of prewarmed sandboxes claimed in **16 ms** instead of 683 ms to create
([`ext/sandbox/CHANGELOG.md`](../../ext/sandbox/CHANGELOG.md)):

```sh
kling plugin install sandbox
cat > node.json <<'JSON'
{"name":"node","image":"toolchain","build_egress":"internet","mem_mib":1024,
 "steps":[{"cmd":["npm","install","-g","typescript"]}],"pool":1}
JSON
kling sbx template apply -f node.json
kling sbx new -template node -ttl 30m
kling sbx ls
kling sbx exec <id> -- tsc --version
kling sbx shell <id>
kling sbx rm <id>
```

The frontal runs as a service on the daemon host (`cd ext/sandbox && make deploy
HOST=ssh://…`, or the `kindling-sandbox-host.tar.gz` of each release); tenants are declared
with `KLING_SANDBOX_TENANTS="ana:tok1:10:5,bob:tok2:5"` before `kling sbx gateway`. It is
accounting and spreading, **not** isolation between tenants: they share daemon and host.
Guide: [`ext/sandbox/README.md`](../../ext/sandbox/README.md); from Kubernetes:
[Kubernetes operator](kubernetes-operator.md).

## What a sandbox cannot do

- Reach your LAN: private ranges are blocked even with `-egress internet`.
- Grow past its disk: overlays are 512 MiB logical; a volume (`-volume`) gives it more.
- Keep files after `rm`: the overlay dies with it. Use `kling cp` out, or a volume.
- Escalate through the exec route: it does not exist on machines booted without
  `-allow-exec`.
