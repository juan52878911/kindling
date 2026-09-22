package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
)

// Main es el main de una extensión escrita en Go. Atiende --kling-manifest,
// --kling-hook y los comandos del manifiesto, y sale con el código que
// corresponda. No vuelve.
//
//	func main() {
//		plugin.Main(manifest, map[string]func([]string) error{
//			"mcp": cmdMCP, "add": cmdAdd,
//		}, map[string]func([]string, io.Writer) error{
//			plugin.HookStatus: hookStatus,
//		})
//	}
//
// La extensión se puede usar también a mano: `kling-mcp mcp list` hace lo mismo
// que `kling mcp list`.
func Main(m Manifest, cmds map[string]func(args []string) error, hooks map[string]func(args []string, w io.Writer) error) {
	os.Exit(serve(m, cmds, hooks, os.Args[1:], os.Stdout, os.Stderr))
}

func serve(m Manifest, cmds map[string]func([]string) error, hooks map[string]func([]string, io.Writer) error,
	args []string, stdout, stderr io.Writer) int {

	if m.ManifestVersion == 0 {
		m.ManifestVersion = ManifestVersion
	}
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		usage(m, stdout)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	switch args[0] {
	case "--kling-manifest":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(m); err != nil {
			return 1
		}
		return 0
	case "--kling-hook":
		if len(args) < 2 {
			fmt.Fprintln(stderr, "usage: --kling-hook <hook> [args...]")
			return 2
		}
		f := hooks[args[1]]
		if f == nil {
			fmt.Fprintf(stderr, "%s: no hook %q\n", m.Name, args[1])
			return 2
		}
		if err := f(args[2:], stdout); err != nil {
			fmt.Fprintf(stderr, "%s: %v\n", m.Name, err)
			return ExitCode(err)
		}
		return 0
	}
	f := cmds[args[0]]
	if f == nil {
		fmt.Fprintf(stderr, "%s: unknown command %q\n\n", m.Name, args[0])
		usage(m, stderr)
		return 2
	}
	if err := f(args[1:]); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitCode(err)
	}
	return 0
}

func usage(m Manifest, w io.Writer) {
	fmt.Fprintf(w, "kling-%s %s", m.Name, m.Version)
	if m.Summary != "" {
		fmt.Fprintf(w, " — %s", m.Summary)
	}
	fmt.Fprintf(w, "\n\nA kling extension: its commands are also available as `kling <command>`.\n\n")
	WriteHelp(w, m.Commands)
}

// WriteHelp imprime los comandos agrupados por sección, como `kling help`.
func WriteHelp(w io.Writer, cmds []Command) {
	groups := map[string][]Command{}
	var order []string
	for _, c := range cmds {
		g := c.Group
		if g == "" {
			g = "EXTENSIONS"
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], c)
	}
	sort.Strings(order)
	for _, g := range order {
		fmt.Fprintln(w, g)
		for _, c := range groups[g] {
			if c.Usage != "" {
				fmt.Fprint(w, c.Usage)
				if c.Usage[len(c.Usage)-1] != '\n' {
					fmt.Fprintln(w)
				}
				continue
			}
			fmt.Fprintf(w, "  %-48s %s\n", c.Name, c.Summary)
		}
		fmt.Fprintln(w)
	}
}
