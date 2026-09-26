package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/config"
)

// Autocompletado de shell, escrito a mano.
//
// kling no usa cobra (dependencia cero, dispatcher propio), así que tampoco hay
// generador de completado. Estos scripts son estáticos salvo por un detalle que
// SÍ es dinámico y el que más se agradece: los comandos que reciben una máquina
// (logs, freeze, thaw…) completan con los IDs vivos preguntándole a `kling ps -q`
// —que existe justo para esto: IDs pelados, un por línea, sin jq ni awk—.
//
// Uso: `source <(kling completion bash)`, `source <(kling completion zsh)`,
// `kling completion fish | source`, o `kling completion install` para dejarlo
// en un fichero.
//
// Cada script exporta _KLING_COMPLETION=1: es como `kling doctor` sabe si el
// completado está cargado en la shell desde la que se le llama.

// coreSubcommands son los subcomandos del núcleo que se completan.
var coreSubcommands = [][2]string{
	{"volume|volumes", "create ls rm populate"},
	{"images", "ls toolchain recipe rm build cat put copy"},
	{"snapshots", "ls rm inspect"},
	{"context", "ls use add rm"},
	{"config", "show path set"},
	{"plugins", "ls"},
	{"sandbox|sandboxes", "create ls renew rm"},
	{"completion", "bash zsh fish install"},
	{"help", ""}, // se rellena con los comandos: ver completionScript
	{"models", "ls add ask rm"},
	{"chispa", "train eval predict inspect deploy ls rm"},
	{"domotica", "decide eval train-slots templates eval-llm"},
	{"ai", "serve ls test generate eval calibrate reload prime review feedback retrain rollback"},
}

// cliCommands son comandos del núcleo que también han de estar en
// coreCommands (para que ninguna extensión los tome); completionCommands quita
// duplicados, así que da igual si ya están allí.
var cliCommands = []string{"doctor", "try"}

// machineArgs son los comandos del núcleo que reciben un id de máquina.
const machineArgs = "logs|freeze|thaw|pause|stop|rm|top|commit|mmds|squeeze|exec|shell|resize|inspect"

var completionShells = []string{"bash", "zsh", "fish"}

func cmdCompletion(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kling completion bash|zsh|fish|install [shell]\n" +
			"  bash:     source <(kling completion bash)\n" +
			"  zsh:      source <(kling completion zsh)   (after `autoload -U compinit && compinit`)\n" +
			"  fish:     kling completion fish | source\n" +
			"  install:  writes it to a file and prints the line for your shell's rc")
	}
	switch args[0] {
	case "bash", "zsh", "fish":
		fmt.Print(completionFor(args[0]))
	case "install":
		sh := ""
		if len(args) > 1 {
			sh = args[1]
		}
		return completionInstall(os.Stdout, sh)
	default:
		return fmt.Errorf("unsupported shell %q: use bash, zsh or fish", args[0])
	}
	return nil
}

func completionFor(shell string) string {
	switch shell {
	case "zsh":
		return completionScript(true)
	case "fish":
		return fishCompletion()
	}
	return completionScript(false)
}

// completionInstall escribe el script en ~/.config/kling/completion.<shell> y
// dice qué línea añadir al rc. No toca el rc: editar los ficheros de arranque
// de alguien sin preguntar es de las cosas que no se perdonan.
func completionInstall(w io.Writer, shell string) error {
	if shell == "" {
		shell = filepath.Base(os.Getenv("SHELL"))
	}
	if !contains(completionShells, shell) {
		return &errWithHint{err: fmt.Errorf("cannot tell your shell (%q)", shell),
			hint: "kling completion install bash|zsh|fish"}
	}
	path, line := completionInstallPath(shell)
	if path == "" {
		return errors.New("cannot find the configuration directory")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(completionFor(shell)), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote %s\n\n", path)
	fmt.Fprintf(w, "Add this line to %s (once):\n  %s\n\n", rcFile(shell), line)
	fmt.Fprintln(w, "The script is static: run `kling completion install` again after installing an extension.")
	return nil
}

// completionInstallPath es el fichero y la línea que lo carga. Va junto a
// config.json (~/.config/kling), que es donde kling ya guarda lo suyo.
func completionInstallPath(shell string) (path, line string) {
	dir := filepath.Dir(config.Path())
	if dir == "" || dir == "." {
		return "", ""
	}
	path = filepath.Join(dir, "completion."+shell)
	switch shell {
	case "bash":
		line = fmt.Sprintf("[ -f %q ] && . %q", path, path)
	case "zsh":
		// compdef necesita compinit antes; la línea lo garantiza sin
		// repetirlo si el rc ya lo hace.
		line = fmt.Sprintf("(( $+functions[compdef] )) || { autoload -U compinit && compinit }; [ -f %q ] && . %q", path, path)
	case "fish":
		line = fmt.Sprintf("test -f %q; and source %q", path, path)
	}
	return path, line
}

func rcFile(shell string) string {
	switch shell {
	case "zsh":
		return "~/.zshrc"
	case "fish":
		return "~/.config/fish/config.fish"
	}
	return "~/.bashrc"
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// completionCommands son los comandos de primer nivel: los del núcleo y los que
// aportan las extensiones instaladas. El script es estático: tras instalar una
// extensión hay que volver a cargarlo.
func completionCommands() string {
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, c := range coreCommands {
		if c != "dial-stdio" && c != "volumes" && c != "builder" && c != "sandboxes" {
			add(c)
		}
	}
	for _, c := range cliCommands {
		add(c)
	}
	for _, c := range extensions().Commands() {
		add(c.Name)
	}
	return strings.Join(out, " ")
}

// subcommandPairs son (patrón, palabras) para el segundo nivel: los del núcleo
// y los de las extensiones. `help` completa con los comandos.
func subcommandPairs() [][2]string {
	var out [][2]string
	for _, sc := range coreSubcommands {
		if sc[0] == "help" {
			sc[1] = completionCommands()
		}
		out = append(out, sc)
	}
	for _, c := range extensions().Commands() {
		if len(c.Subcommands) > 0 {
			out = append(out, [2]string{c.Name, strings.Join(c.Subcommands, " ")})
		}
	}
	return out
}

func completionScript(zsh bool) string {
	var cases strings.Builder
	add := func(pattern, words string) {
		if zsh {
			fmt.Fprintf(&cases, "        %s) compadd -- %s ;;\n", pattern, words)
		} else {
			fmt.Fprintf(&cases, "        %s) COMPREPLY=( $(compgen -W \"%s\" -- \"$cur\") ); return ;;\n", pattern, words)
		}
	}
	for _, sc := range subcommandPairs() {
		add(sc[0], sc[1])
	}
	if zsh {
		fmt.Fprintf(&cases, "        %s) compadd -- ${(f)\"$(kling ps -q 2>/dev/null)\"} ;;\n", machineArgs)
		return fmt.Sprintf(zshCompletion, completionCommands(), cases.String())
	}
	fmt.Fprintf(&cases, "        %s) COMPREPLY=( $(compgen -W \"$(kling ps -q 2>/dev/null)\" -- \"$cur\") ); return ;;\n", machineArgs)
	return fmt.Sprintf(bashCompletion, completionCommands(), cases.String())
}

// fishCompletion genera el script de fish. fish no tiene un `case` sobre la
// línea: cada regla lleva su condición, y las de segundo nivel se limitan a
// la segunda palabra para no seguir ofreciendo subcomandos detrás.
func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# kling fish completion.  Load with:  kling completion fish | source\n")
	b.WriteString("set -gx _KLING_COMPLETION 1\n")
	b.WriteString("function __kling_second_word\n    test (count (commandline -opc)) -eq 2\nend\n")
	b.WriteString("complete -c kling -f\n")
	fmt.Fprintf(&b, "complete -c kling -n __fish_use_subcommand -a %q\n", completionCommands())
	for _, sc := range subcommandPairs() {
		cmds := strings.ReplaceAll(sc[0], "|", " ")
		fmt.Fprintf(&b, "complete -c kling -n '__kling_second_word; and __fish_seen_subcommand_from %s' -a %q\n", cmds, sc[1])
	}
	fmt.Fprintf(&b, "complete -c kling -n '__fish_seen_subcommand_from %s' -a '(kling ps -q 2>/dev/null)'\n",
		strings.ReplaceAll(machineArgs, "|", " "))
	return b.String()
}

const bashCompletion = `# kling bash completion.  Load with:  source <(kling completion bash)
export _KLING_COMPLETION=1
_kling() {
    local cur prev cword
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    cword=$COMP_CWORD

    local commands="%s"

    if [ "$cword" -eq 1 ]; then
        COMPREPLY=( $(compgen -W "$commands" -- "$cur") )
        return
    fi

    case "${COMP_WORDS[1]}" in
%s    esac
}
complete -F _kling kling
`

const zshCompletion = `# kling zsh completion.  Load with:  source <(kling completion zsh)
export _KLING_COMPLETION=1
_kling() {
    local -a commands
    commands=(%s)

    if (( CURRENT == 2 )); then
        compadd -- $commands
        return
    fi

    case "${words[2]}" in
%s    esac
}
compdef _kling kling
`
