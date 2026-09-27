package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// Autocompletado de shell, generado del árbol de comandos.
//
// kling no usa cobra (dependencia cero, dispatcher propio), así que el
// generador es este. Los scripts son estáticos salvo por un detalle que SÍ es
// dinámico y el que más se agradece: los comandos que reciben una máquina
// (logs, freeze, thaw…) completan con los IDs vivos preguntándole a
// `kling ps -q` —que existe justo para esto: IDs pelados, uno por línea—.
//
// Lo que se completa sale de coreTree y de los manifiestos de las extensiones
// (tree.go): primer nivel, segundo nivel (subcomandos y comandos de cada
// extensión) y qué recibe una máquina. Los alias no se completan.
//
// Uso: `source <(kling completion bash)`, `source <(kling completion zsh)`,
// `kling completion fish | source`, o `kling completion install` para dejarlo
// en un fichero.
//
// Cada script exporta _KLING_COMPLETION=1: es como `kling doctor` sabe si el
// completado está cargado en la shell desde la que se le llama.

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

// userShell es la shell de quien teclea, por $SHELL: "zsh", "bash", "fish" o
// "" si no se sabe.
func userShell() string {
	sh := filepath.Base(os.Getenv("SHELL"))
	if contains(completionShells, sh) {
		return sh
	}
	return ""
}

// reloadHint es la línea que recarga el completado en la shell de quien
// teclea; con shell vacía se detecta. Antes decía siempre zsh.
func reloadHint(shell string) string {
	if shell == "" {
		shell = userShell()
	}
	switch shell {
	case "fish":
		return "kling completion fish | source"
	case "bash":
		return "source <(kling completion bash)"
	case "zsh":
		return "source <(kling completion zsh)"
	}
	return "source <(kling completion bash|zsh)  or  kling completion fish | source"
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

// completionCommands son las palabras de primer nivel que se completan: los
// comandos visibles del núcleo (sin los avanzados ni los alias) y lo que
// aportan las extensiones instaladas. El script es estático: tras instalar
// una extensión hay que volver a cargarlo.
func completionCommands() string {
	seen := map[string]bool{}
	var out []string
	add := func(c string) {
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	for _, s := range coreTree {
		if s.advanced {
			continue
		}
		for _, c := range s.cmds {
			if !c.Hidden {
				add(c.Name)
			}
		}
	}
	for _, c := range extensions().Commands() {
		add(c.Name)
	}
	return strings.Join(out, " ")
}

// subcommandPairs son (comando, palabras) para el segundo nivel: los
// subcomandos del núcleo y los comandos de cada extensión. `help` completa
// con los comandos.
func subcommandPairs() [][2]string {
	var out [][2]string
	for _, s := range coreTree {
		for _, c := range s.cmds {
			switch {
			case c.Name == "help":
				out = append(out, [2]string{c.Name, "all " + completionCommands()})
			case len(c.Subcommands) > 0:
				out = append(out, [2]string{c.Name, strings.Join(c.Subcommands, " ")})
			}
		}
	}
	for _, c := range extensions().Commands() {
		if len(c.Subcommands) > 0 {
			out = append(out, [2]string{c.Name, strings.Join(c.Subcommands, " ")})
		}
	}
	return out
}

// machineArgs son los comandos de primer nivel que reciben un id de máquina
// justo detrás, separados por |.
func machineArgs() string {
	var out []string
	for _, s := range coreTree {
		for _, c := range s.cmds {
			for _, m := range c.MachineArgs {
				if m == "" {
					out = append(out, c.Name)
				}
			}
		}
	}
	for _, c := range extensions().Commands() {
		for _, m := range c.MachineArgs {
			if m == "" {
				out = append(out, c.Name)
			}
		}
	}
	return strings.Join(out, "|")
}

// machineSubArgs son los (comando, subcomandos) tras los que va una máquina.
func machineSubArgs() [][2]string {
	var out [][2]string
	all := []plugin.Command{}
	for _, s := range coreTree {
		all = append(all, s.cmds...)
	}
	all = append(all, extensions().Commands()...)
	for _, c := range all {
		var subs []string
		for _, m := range c.MachineArgs {
			if m != "" {
				subs = append(subs, m)
			}
		}
		if len(subs) > 0 {
			out = append(out, [2]string{c.Name, strings.Join(subs, "|")})
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
	// Tercera palabra tras un subcomando que recibe máquina (machine resize x).
	var third strings.Builder
	for _, p := range machineSubArgs() {
		if zsh {
			fmt.Fprintf(&third, "        %s) case \"${words[3]}\" in %s) compadd -- ${(f)\"$(kling ps -q 2>/dev/null)\"} ;; esac ;;\n", p[0], p[1])
		} else {
			fmt.Fprintf(&third, "        %s) case \"${COMP_WORDS[2]}\" in %s) COMPREPLY=( $(compgen -W \"$(kling ps -q 2>/dev/null)\" -- \"$cur\") ); return ;; esac ;;\n", p[0], p[1])
		}
	}
	for _, sc := range subcommandPairs() {
		add(sc[0], sc[1])
	}
	if zsh {
		if ma := machineArgs(); ma != "" {
			fmt.Fprintf(&cases, "        %s) compadd -- ${(f)\"$(kling ps -q 2>/dev/null)\"} ;;\n", ma)
		}
		return fmt.Sprintf(zshCompletion, completionCommands(), third.String(), cases.String())
	}
	if ma := machineArgs(); ma != "" {
		fmt.Fprintf(&cases, "        %s) COMPREPLY=( $(compgen -W \"$(kling ps -q 2>/dev/null)\" -- \"$cur\") ); return ;;\n", ma)
	}
	return fmt.Sprintf(bashCompletion, completionCommands(), third.String(), cases.String())
}

// fishCompletion genera el script de fish. fish no tiene un `case` sobre la
// línea: cada regla lleva su condición, y las de segundo nivel se limitan a
// la segunda palabra para no seguir ofreciendo subcomandos detrás.
func fishCompletion() string {
	var b strings.Builder
	b.WriteString("# kling fish completion.  Load with:  kling completion fish | source\n")
	b.WriteString("set -gx _KLING_COMPLETION 1\n")
	b.WriteString("function __kling_second_word\n    test (count (commandline -opc)) -eq 2\nend\n")
	b.WriteString("function __kling_third_word\n    test (count (commandline -opc)) -eq 3\nend\n")
	b.WriteString("complete -c kling -f\n")
	fmt.Fprintf(&b, "complete -c kling -n __fish_use_subcommand -a %q\n", completionCommands())
	for _, sc := range subcommandPairs() {
		fmt.Fprintf(&b, "complete -c kling -n '__kling_second_word; and __fish_seen_subcommand_from %s' -a %q\n", sc[0], sc[1])
	}
	if ma := machineArgs(); ma != "" {
		fmt.Fprintf(&b, "complete -c kling -n '__fish_seen_subcommand_from %s' -a '(kling ps -q 2>/dev/null)'\n",
			strings.ReplaceAll(ma, "|", " "))
	}
	for _, p := range machineSubArgs() {
		fmt.Fprintf(&b, "complete -c kling -n '__kling_third_word; and __fish_seen_subcommand_from %s; and __fish_seen_subcommand_from %s' -a '(kling ps -q 2>/dev/null)'\n",
			p[0], strings.ReplaceAll(p[1], "|", " "))
	}
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

    if [ "$cword" -ge 3 ]; then
        case "${COMP_WORDS[1]}" in
%s        esac
    fi

    if [ "$cword" -ne 2 ]; then
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

    if (( CURRENT >= 4 )); then
        case "${words[2]}" in
%s        esac
        return
    fi

    case "${words[2]}" in
%s    esac
}
compdef _kling kling
`
