package plugin

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Main es el main de una extensión escrita en Go. Atiende --kling-manifest,
// --kling-hook, la ayuda y los comandos del manifiesto, y sale con el código
// que corresponda. No vuelve.
//
//	func main() {
//		plugin.Main(manifest, map[string]func([]string) error{
//			"add": cmdAdd, "import": cmdImport,
//		}, map[string]func([]string, io.Writer) error{
//			plugin.HookStatus: hookStatus,
//		})
//	}
//
// La extensión se puede usar también a mano: `kling-mcp ls` hace lo mismo que
// `kling mcp ls`. Un comando del mapa que no está en el manifiesto se puede
// teclear igual (alias, internos), pero no sale en la ayuda.
func Main(m Manifest, cmds map[string]func(args []string) error, hooks map[string]func(args []string, w io.Writer) error) {
	os.Exit(serve(m, cmds, hooks, os.Args[1:], os.Stdout, os.Stderr))
}

func serve(m Manifest, cmds map[string]func([]string) error, hooks map[string]func([]string, io.Writer) error,
	args []string, stdout, stderr io.Writer) int {

	if m.ManifestVersion == 0 {
		m.ManifestVersion = ManifestVersion
	}
	prefix := "kling " + m.Name
	if len(args) == 0 || IsHelpArg(args[0]) {
		if len(args) > 1 && !IsHelpArg(args[1]) {
			// `kling-mcp help add`
			return commandHelp(m, args[1], stdout, stderr)
		}
		WriteNamespace(stdout, prefix, m)
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
		fmt.Fprintf(stderr, "error: unknown command %q\ntry: %s help\n", prefix+" "+args[0], prefix)
		return 2
	}
	// `kling-mcp add -h`: la ayuda uniforme, salvo que sea el proceso que
	// imprime los flags para ella (ver FlagHelp).
	if len(args) == 2 && IsHelpArg(args[1]) && os.Getenv(FlagHelpEnv) == "" && m.Command(args[0]) != nil {
		return commandHelp(m, args[0], stdout, stderr)
	}
	if err := f(args[1:]); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return ExitCode(err)
	}
	return 0
}

// commandHelp imprime la ayuda de un comando del manifiesto, con sus flags.
func commandHelp(m Manifest, name string, stdout, stderr io.Writer) int {
	c := m.Command(name)
	if c == nil {
		fmt.Fprintf(stderr, "error: unknown command %q\ntry: kling %s help\n", name, m.Name)
		return 2
	}
	exe, _ := os.Executable()
	full, prefix := "kling "+m.Name+" "+name, m.Name+" "
	if m.IsTopLevel(c) {
		full, prefix = "kling "+name, ""
	}
	WriteCommand(stdout, full, prefix, *c, FlagHelp(exe, name), "kling help "+m.Name)
	return 0
}
