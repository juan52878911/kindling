package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

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
		// Una configuración ilegible no puede dejar a kling sin extensiones:
		// sin ella no hay ninguna desactivada, y el error ya lo dará quien
		// la necesite de verdad.
		var disabled []string
		if c, err := config.Load(); err == nil {
			disabled = c.Plugins.Disabled
		}
		extReg = plugin.Discover(context.Background(), plugin.Options{
			Core:     coreCommands,
			Version:  strings.TrimPrefix(Version, "v"),
			Disabled: disabled,
			Builtins: builtinExtensions(),
		})
	})
	return extReg
}

// cmdPlugins gestiona las extensiones: listarlas, instalarlas desde una
// release, quitarlas y apagarlas o encenderlas.
func cmdPlugins(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "ls", "list":
			return pluginsLs(args[1:])
		case "install":
			return pluginsInstall(args[1:])
		case "rm", "remove", "uninstall":
			return pluginsRm(args[1:])
		case "enable":
			return pluginsEnable(args[1:], true)
		case "disable":
			return pluginsEnable(args[1:], false)
		}
		if !strings.HasPrefix(args[0], "-") {
			return fmt.Errorf("unknown plugins subcommand %q: use ls, install, rm, enable or disable", args[0])
		}
	}
	return pluginsLs(args)
}

// pluginRow es una extensión tal como la enseña `plugins ls`.
type pluginRow struct {
	Name      string   `json:"name"`
	Version   string   `json:"version,omitempty"`
	Status    string   `json:"status"` // ok | disabled | error: ...
	Path      string   `json:"path,omitempty"`
	Builtin   bool     `json:"builtin"`
	Disabled  bool     `json:"disabled"`
	Commands  []string `json:"commands,omitempty"`
	Hooks     []string `json:"hooks,omitempty"`
	Shadowed  []string `json:"shadowed,omitempty"`
	Error     string   `json:"error,omitempty"`
	SHA256    string   `json:"sha256,omitempty"`
	URL       string   `json:"url,omitempty"`
	Installed string   `json:"installed,omitempty"`
	// Modified es que el binario ya no tiene el sha256 con el que se instaló.
	Modified bool `json:"modified,omitempty"`
}

func pluginRows(reg *plugin.Registry) []pluginRow {
	out := []pluginRow{}
	for _, p := range reg.Plugins {
		r := pluginRow{Name: p.Name, Path: p.Path, Builtin: p.Builtin != nil, Disabled: p.Disabled,
			Shadowed: p.Shadowed, Status: "ok"}
		if p.Manifest != nil {
			r.Version, r.Hooks = p.Manifest.Version, p.Manifest.Hooks
			for _, c := range p.Manifest.Commands {
				if reg.Lookup(c.Name) == p {
					r.Commands = append(r.Commands, c.Name)
				}
			}
		}
		switch {
		case p.Disabled:
			r.Status = "disabled"
		case p.Err != nil:
			r.Status, r.Error = "error: "+p.Err.Error(), p.Err.Error()
		}
		// El .json dice de dónde salió; el hash se recalcula para que lo que
		// se enseña sea lo que hay en disco y no lo que hubo.
		if p.Path != "" {
			if sc, err := plugin.ReadSidecar(p.Path); err == nil {
				r.SHA256, r.URL = sc.SHA256, sc.URL
				if !sc.Installed.IsZero() {
					r.Installed = sc.Installed.Format(time.RFC3339)
				}
				if h, err := plugin.FileSHA256(p.Path); err == nil && h != sc.SHA256 {
					r.SHA256, r.Modified = h, true
				}
			}
		}
		out = append(out, r)
	}
	return out
}

// pluginsLs lista las extensiones: qué aportan, de dónde salen y por qué no se
// pueden usar si es el caso.
func pluginsLs(args []string) error {
	fs := flag.NewFlagSet("plugin ls", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rows := pluginRows(extensions())
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(rows)
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tVERSION\tSTATUS\tCOMMANDS\tSOURCE")
	for _, r := range rows {
		src := r.Path
		switch {
		case r.Builtin:
			src = "built in"
		case r.Modified:
			src += fmt.Sprintf(" (sha256 %s, changed since install)", shortSHA(r.SHA256))
		case r.SHA256 != "":
			src += fmt.Sprintf(" (sha256 %s)", shortSHA(r.SHA256))
		}
		version := r.Version
		if version == "" {
			version = "—"
		}
		line := strings.Join(r.Commands, " ")
		if len(r.Shadowed) > 0 {
			line += fmt.Sprintf("  (ignored, already taken: %s)", strings.Join(r.Shadowed, " "))
		}
		if line == "" {
			line = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Name, version, r.Status, line, src)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println("\nAn extension is any executable named kling-<name> on your PATH or in")
	fmt.Println("$KLING_PLUGIN_PATH. Install one from this release:  kling plugin install <name>")
	fmt.Println("After installing one, reload completion:  " + reloadHint(""))
	return nil
}

func shortSHA(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
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
