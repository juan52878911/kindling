package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"text/tabwriter"

	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// coreCommands son los comandos del núcleo. Ganan siempre: una extensión que
// declare uno de estos no lo recibe.
var coreCommands = []string{
	"up", "status", "run", "ps", "logs", "freeze", "thaw", "stop", "rm", "squeeze",
	"mmds", "commit", "snapshots", "images", "rmi", "topo", "top", "events", "info",
	"context", "config", "volume", "volumes", "daemon", "dial-stdio", "builder", "plugins",
	"exec", "shell", "cp", "sandbox", "sandboxes", "resize", "models",
	"completion", "version", "help",
}

var (
	extOnce sync.Once
	extReg  *plugin.Registry
)

// extensions descubre las extensiones una sola vez por proceso. Solo se llama
// cuando hace falta —un comando que no es del núcleo, la ayuda, el completado,
// `status`, `plugins`—: `kling ps` no ejecuta nada de nadie.
func extensions() *plugin.Registry {
	extOnce.Do(func() {
		plugin.SetCoreVersion(strings.TrimPrefix(Version, "v"))
		extReg = plugin.Discover(context.Background(), plugin.Options{
			Core:    coreCommands,
			Version: strings.TrimPrefix(Version, "v"),
		})
	})
	return extReg
}

// printUsage imprime la ayuda del núcleo con las secciones que aportan las
// extensiones en medio, como si fueran suyas.
func printUsage(w io.Writer) {
	fmt.Fprint(w, usageHead)
	plugin.WriteHelp(w, extensions().Commands())
	fmt.Fprint(w, usageTail)
}

// helpFor es `kling help <comando>`: el de una extensión se lo pregunta a ella.
func helpFor(cmd string) error {
	if p := extensions().Lookup(cmd); p != nil {
		return plugin.Exec(p, cmd, []string{"-h"}, config.Path())
	}
	printUsage(os.Stdout)
	return nil
}

// cmdPlugins lista las extensiones: qué aportan, de dónde salen y por qué no se
// pueden usar si es el caso.
func cmdPlugins(args []string) error {
	if len(args) > 0 && (args[0] == "ls" || args[0] == "list") {
		args = args[1:]
	}
	fs := flag.NewFlagSet("plugins", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	reg := extensions()

	if *asJSON {
		type row struct {
			Name     string   `json:"name"`
			Version  string   `json:"version,omitempty"`
			Path     string   `json:"path,omitempty"`
			Builtin  bool     `json:"builtin"`
			Commands []string `json:"commands,omitempty"`
			Hooks    []string `json:"hooks,omitempty"`
			Shadowed []string `json:"shadowed,omitempty"`
			Error    string   `json:"error,omitempty"`
		}
		out := []row{}
		for _, p := range reg.Plugins {
			r := row{Name: p.Name, Path: p.Path, Builtin: p.Builtin != nil, Shadowed: p.Shadowed}
			if p.Manifest != nil {
				r.Version, r.Hooks = p.Manifest.Version, p.Manifest.Hooks
				for _, c := range p.Manifest.Commands {
					if reg.Lookup(c.Name) == p {
						r.Commands = append(r.Commands, c.Name)
					}
				}
			}
			if p.Err != nil {
				r.Error = p.Err.Error()
			}
			out = append(out, r)
		}
		return json.NewEncoder(os.Stdout).Encode(out)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tCOMMANDS\tSOURCE")
	for _, p := range reg.Plugins {
		src := p.Path
		if p.Builtin != nil {
			src = "built in"
		}
		if p.Err != nil {
			fmt.Fprintf(tw, "%s\t—\t✗ %v\t%s\n", p.Name, p.Err, src)
			continue
		}
		var cmds []string
		for _, c := range p.Manifest.Commands {
			if reg.Lookup(c.Name) == p {
				cmds = append(cmds, c.Name)
			}
		}
		line := strings.Join(cmds, " ")
		if len(p.Shadowed) > 0 {
			line += fmt.Sprintf("  (ignored, already taken: %s)", strings.Join(p.Shadowed, " "))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", p.Name, p.Manifest.Version, line, src)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println("\nAn extension is any executable named kling-<name> on your PATH or in")
	fmt.Println("$KLING_PLUGIN_PATH. After installing one, reload completion:  source <(kling completion zsh)")
	return nil
}

// knownFlags deja en args solo los flags que fs conoce (con su valor), para
// poder parsear lo propio y pasar el resto tal cual a una extensión. Un flag
// desconocido no es un error aquí: puede ser de otra.
func knownFlags(fs *flag.FlagSet, args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			continue
		}
		name := strings.TrimLeft(a, "-")
		hasValue := false
		if k, _, ok := strings.Cut(name, "="); ok {
			name, hasValue = k, true
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		out = append(out, a)
		if hasValue {
			continue
		}
		if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}
