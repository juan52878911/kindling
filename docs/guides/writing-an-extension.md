# Writing an extension

`kling` is the only command anyone types. The core brings the microVMs; everything else is
an **extension**: an executable named `kling-<name>` that declares which subcommands it
adds. `kling` discovers it, shows it in `kling help` and in shell completion, and hands over
control (exit codes, signals and terminal included) when you type one of its commands. The
official ones, `mcp` and `sandbox`, are written this way and live in [`ext/`](../../ext).

The reference is [`docs/extensions.md`](../extensions.md) (Spanish): the manifest field by
field, hooks, config keys, companions, the alias plan. This page is the ten-minute path.

## 1. The code (2 minutes)

[`examples/hello-extension/main.go`](../../examples/hello-extension/main.go), trimmed:

```go
package main

import (
	"flag"
	"fmt"
	"io"

	"github.com/juan52878911/kindling/pkg/plugin"
)

var manifest = plugin.Manifest{
	Name:     "hello",
	Version:  "0.1.0",
	MinKling: "0.13.0",
	Summary:  "the smallest kling extension",
	Commands: []plugin.Command{{
		Name:    "hello",
		Group:   "EXAMPLES",
		Summary: "prints a greeting",
		Usage:   "  hello [-name N]                    prints a greeting (hello.greeting changes it)\n",
	}},
	Config: []plugin.ConfigKey{
		{Key: "greeting", Type: "string", Help: "word used by `kling hello` (default: hello)"},
	},
	Hooks: []string{plugin.HookStatus},
}

func main() {
	plugin.Main(manifest, map[string]func([]string) error{
		"hello": cmdHello,
	}, map[string]func([]string, io.Writer) error{
		plugin.HookStatus: hookStatus,
	})
}
```

`plugin.Main` implements the whole protocol: it answers `--kling-manifest`, runs
`--kling-hook <hook>`, dispatches the manifest's commands and exits with the right code.
It depends only on the standard library and on `pkg/plugin` and `pkg/config` of the core.

To start yours outside this repository:

```sh
mkdir kling-hola && cd kling-hola
go mod init example.com/kling-hola
cp <kindling>/examples/hello-extension/main.go .
go get github.com/juan52878911/kindling@v0.14.0
```

## 2. Build and install (1 minute)

An installed extension is an executable in the **extensions directory**: the first
directory of `$KLING_PLUGIN_PATH` if set, else `$XDG_DATA_HOME/kling/plugins`, else
`~/.local/share/kling/plugins`.

```sh
go build -o ~/.local/share/kling/plugins/kling-hello ./examples/hello-extension
```

Nothing to register: the file name is the registry.

## 3. Try it (2 minutes)

```sh
kling plugin ls                  # hello  0.1.0  ok  ~/.local/share/kling/plugins/kling-hello
kling hello -name Ada            # hello, Ada!
kling config set hello.greeting hola
kling hello                      # hola, world!
kling help hello                 # the usage block from the manifest
kling status                     # includes the line from the status hook
source <(kling completion zsh)   # reload completion: it now offers "hello"
kling plugin disable hello       # off without uninstalling; kling plugin enable hello
```

`kling doctor` checks it too: an invalid manifest or a `min_kling` above the core's shows
as ✗ with the reason. The extension also runs on its own (`kling-hello hello`).

## 4. Where the commands live

Since 0.14 an extension's commands live **under its name**: `kling mcp add`, `kling sbx
ls`. An extension whose only command is called like itself (`kling hello`) is top-level
without saying anything; promoting one otherwise (`top_level`, as `connect` does) is
explicit and should be rare. `group` says in which section of `kling help all` it appears;
`hidden` keeps it out of the help.

## 5. Talking to the daemon

An extension only uses the daemon's HTTP API ([`docs/api.md`](../api.md)), never its
internals. What the core offers so extensions do not have to invent storage: snapshot
annotations, a key-value store, named image builders (`kling image build -builder …`),
files inside images (`kling image put`), the proxy to the guest, and typed configuration
keys. Go extensions get it all from `pkg/plugin`, `pkg/api`, `pkg/config`, `pkg/guest` and
`pkg/scheduler`.

Hooks let an extension take part in `kling status` and `kling up` (its systemd units), and
`companions` lists the helper executables it ships (`kling-bridge` for `mcp`), which `kling
plugin install` downloads and verifies alongside it.

## 6. Publishing it

Ship `kling-<name>-<os>-<arch>` binaries in a release with a `SHA256SUMS`; then
`kling plugin install <name> -from https://…` (or `-file PATH -sha256 H`) installs it with
the same verification as the official ones. Test the manifest the way
[`main_test.go`](../../examples/hello-extension/main_test.go) does: a broken manifest does
not break `kling`, it just makes your extension vanish from the help.
