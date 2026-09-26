package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// parseInterspersed parsea fs dejando que los flags vayan antes o después de
// los argumentos: `kling plugins install mcp -from URL` es lo que teclea la
// gente, y flag.Parse se detiene en el primer argumento que no es un flag.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, nil
		}
		if args[0] == "--" {
			return append(pos, args[1:]...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// pluginsInstall es `kling plugins install <nombre>[@vX.Y.Z]`.
func pluginsInstall(args []string) error {
	fs := flag.NewFlagSet("plugins install", flag.ExitOnError)
	from := fs.String("from", "", "https URL of the extension binary (its SHA256SUMS is looked up next to it)")
	file := fs.String("file", "", "install a local binary instead of downloading one")
	sum := fs.String("sha256", "", "expected sha256 of the binary")
	dir := fs.String("dir", "", "install into this directory instead of the extensions directory")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: kling plugins install <name>[@vX.Y.Z] [-from URL] [-file PATH] [-sha256 H] [-dir DIR]")
		fs.PrintDefaults()
	}
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		fs.Usage()
		return errors.New("plugins install needs exactly one extension name")
	}
	name, tag, _ := strings.Cut(pos[0], "@")
	name = strings.TrimPrefix(name, "kling-")
	if !plugin.ValidName(name) {
		return fmt.Errorf("invalid extension name %q", name)
	}
	if tag != "" && (*from != "" || *file != "") {
		return errors.New("@tag picks a release; it does not go with -from or -file")
	}
	if tag != "" && !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	if p := extensions().Lookup(name); p != nil && p.Builtin != nil {
		return fmt.Errorf("%s is built into kling: there is nothing to install", name)
	}

	// Ctrl-C a media descarga no deja nada: lo que hay en disco son
	// temporales que Install borra al volver.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := plugin.Install(ctx, plugin.InstallOptions{
		Name:        name,
		Tag:         tag,
		From:        *from,
		File:        *file,
		SHA256:      *sum,
		Dir:         *dir,
		CoreVersion: strings.TrimPrefix(Version, "v"),
	})
	if err != nil {
		return err
	}

	verb := "installed"
	if res.Replaced != nil {
		verb = fmt.Sprintf("updated (was %s)", res.Replaced.Version)
	}
	fmt.Printf("%s %s %s: %s\n", name, res.Manifest.Version, verb, res.Path)
	fmt.Printf("  from    %s\n", res.Sidecar.URL)
	fmt.Printf("  sha256  %s\n", res.Sidecar.SHA256)
	for _, c := range res.Companions {
		fmt.Printf("  with    %s\n", c)
	}
	core := map[string]bool{}
	for _, c := range coreCommands {
		core[c] = true
	}
	var cmds, taken []string
	for _, c := range res.Manifest.Commands {
		if core[c.Name] {
			taken = append(taken, c.Name)
		} else {
			cmds = append(cmds, "kling "+c.Name)
		}
	}
	if len(cmds) > 0 {
		fmt.Printf("  adds    %s\n", strings.Join(cmds, ", "))
	}
	if len(taken) > 0 {
		fmt.Printf("  ignored %s (core commands always win)\n", strings.Join(taken, " "))
	}
	if !inSearchPath(filepath.Dir(res.Path)) {
		fmt.Printf("\nnote: %s is not where kling looks for extensions; add it to $KLING_PLUGIN_PATH\n", filepath.Dir(res.Path))
	}
	if c, err := config.Load(); err == nil && c.PluginDisabled(name) {
		fmt.Printf("\nnote: %s is disabled; turn it on with: kling plugins enable %s\n", name, name)
	}
	fmt.Println("\nReload shell completion to pick up the new commands:  source <(kling completion zsh)")
	return nil
}

func inSearchPath(dir string) bool {
	want, _ := filepath.Abs(dir)
	for _, d := range plugin.SearchPath() {
		if a, _ := filepath.Abs(d); a == want {
			return true
		}
	}
	return false
}

// pluginsRm es `kling plugins rm <nombre>`: solo borra lo que está en el
// directorio de extensiones.
func pluginsRm(args []string) error {
	fs := flag.NewFlagSet("plugins rm", flag.ExitOnError)
	dir := fs.String("dir", "", "extensions directory to remove it from")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: kling plugins rm <name> [-dir DIR]")
	}
	name := strings.TrimPrefix(pos[0], "kling-")
	if !plugin.ValidName(name) {
		return fmt.Errorf("invalid extension name %q", name)
	}
	for _, p := range extensions().Plugins {
		if p.Name == name && p.Builtin != nil {
			return fmt.Errorf("%s is built into kling and cannot be removed; turn it off with: kling plugins disable %s", name, name)
		}
	}
	d := *dir
	if d == "" {
		if d, err = plugin.InstallDir(); err != nil {
			return err
		}
	}
	removed, err := plugin.Remove(d, name)
	if err != nil {
		return err
	}
	for _, p := range removed {
		fmt.Println("removed", p)
	}
	return nil
}

// pluginsEnable es `kling plugins enable|disable <nombre>`: edita
// plugins.disabled en config.json. Desactivar es la única forma de quitar de
// en medio una incorporada, y la forma rápida de probar sin una externa.
func pluginsEnable(args []string, enable bool) error {
	verb := "disable"
	if enable {
		verb = "enable"
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: kling plugins %s <name>", verb)
	}
	name := strings.TrimPrefix(args[0], "kling-")
	if !plugin.ValidName(name) {
		return fmt.Errorf("invalid extension name %q", name)
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	known := false
	for _, p := range extensions().Plugins {
		if p.Name == name {
			known = true
		}
	}
	if !c.SetPluginDisabled(name, !enable) {
		fmt.Printf("%s is already %sd\n", name, verb)
		return nil
	}
	if err := c.Save(); err != nil {
		return err
	}
	if enable {
		fmt.Printf("%s enabled\n", name)
	} else {
		fmt.Printf("%s disabled: its commands and hooks are off until `kling plugins enable %s`\n", name, name)
	}
	if !known && !enable {
		fmt.Printf("note: no extension named %s is installed right now; the setting is kept for when it is\n", name)
	}
	return nil
}
