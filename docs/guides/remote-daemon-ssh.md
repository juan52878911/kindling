# A remote daemon over SSH

The daemon runs where KVM is; the CLI runs where you are. The two talk over SSH with the
same trick as `docker context`: no port, no TLS, no extra tool on the far end.

## Why there is no TCP port

Controlling microVMs is equivalent to root on their host: it can mount disks and boot
arbitrary kernels. So `kling daemon` listens **only** on a Unix socket (`/run/kling.sock`),
and remote access is SSH and nothing else. The CLI runs `ssh host kling dial-stdio`, which
bridges the SSH pipe to the local socket. Authentication is SSH's; kindling has no
credentials of its own.

## 1. On the server (Linux with KVM)

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
kling up            # prints the privileged commands; run them, then kling up again
kling doctor
```

From your laptop, with the context of step 2 active, `kling up` checks that host over SSH
and prints exactly what is missing there — or, when everything is in place, the
`ssh -t host 'sudo systemctl enable --now kling …'` line that starts the service (sudo
needs your terminal, so it does not run it for you). Or, from a checkout, build and deploy
in one go:

```sh
make deploy HOST=ssh://user@server                 # linux/amd64
make deploy GOARCH=arm64 HOST=ssh://user@server    # an arm64 host
```

`make deploy` builds the daemon, copies the binary and the systemd unit
([`packaging/kling.service`](../../packaging/kling.service)) and starts the service. The
unit hands the socket to the user you SSH in as, so you do not run the whole client under
sudo, and carries `KillMode=process`: restarting the daemon does not kill the microVMs.

## 2. On your laptop

```sh
curl -fsSL https://raw.githubusercontent.com/juan52878911/kindling/main/scripts/install.sh | sh
kling context add lab ssh://user@server -description "home server"
kling context ls
kling status -v
```

`context add` activates the context. From now on every `kling` command goes to that daemon:

```
    NAME   HOST                 DESCRIPTION
*   lab    ssh://user@server    home server
```

```
endpoint:     ssh://lab
daemon:       0.14.0
root:         /var/lib/kindling
backend:      firecracker
arch:         amd64
KVM:          yes
firecracker:  Firecracker v1.17.0
machines:     4
capabilities: annotations, store, builders, image-files, exec, sandboxes, shell, resize, image-blobs, guest-resync, shares-copy, shares-live, renew, pause
```

An SSH alias in `~/.ssh/config` (`Host lab`) works as well: `ssh://lab`.

**Precedence:** `-H` > `$KLING_HOST` > active context > local socket. The flag always wins,
so a one-off against another daemon does not force you to switch contexts:

```sh
kling -H ssh://user@other ps
export KLING_HOST=ssh://user@server      # the same, per shell
```

## 3. Things that work differently over SSH

- **Everything streams through the pipe**: `exec`, `shell`, `logs -f`, `cp`, `events`.
  `kling shell` opens a real terminal inside the microVM through SSH.
- **`-share ./dir:/work` uploads your folder**: the default `copy` mode tars it locally and
  the daemon builds a read-only ext4 from it, so it works from any machine. Live `ro`/`rw`
  shares serve a folder that is *on the daemon host*, under `daemon.share_roots`.
- **`kling mcp import` probes the guest through the daemon**: guest IPs exist only on the
  host's network, so the daemon proxies the probe (`POST /machines/{ref}/guest`).
- **The gateways listen on the server**. Tell the CLI the address agents should use:
  `kling config set gateway.url http://server:8080`. `kling connect` uses it when it writes
  your agent's configuration.
- **Images move between daemons with `kling image copy <name> -from ssh://…`**, kernel and
  base layer included, verified by sha256 on arrival.

## 4. Several daemons

Add as many contexts as you have hosts and switch with `kling context use`. The MCP gateway
can front several daemons at once (`kling mcp serve -hosts lab=ssh://…,edge=ssh://…`), and
the sandbox frontal spreads sandboxes across hosts by free room
([`ext/sandbox`](../../ext/sandbox/README.md)).

## Troubleshooting

- `kling status` exits 1 and says the daemon does not answer: check `ssh user@server kling
  version` by hand. If that works but the socket is refused, the unit is not running
  (`systemctl status kling`) or your user is not the one the unit hands the socket to.
- CLI and daemon versions differ: `kling doctor` says so; update the older one. All
  binaries of one release are compatible with each other.
- Full list: [Troubleshooting](troubleshooting.md).
