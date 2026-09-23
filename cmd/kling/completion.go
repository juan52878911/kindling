package main

import (
	"fmt"
	"strings"
)

// Autocompletado de shell, escrito a mano.
//
// kling no usa cobra (dependencia cero, dispatcher propio), así que tampoco hay
// generador de completado. Estos scripts son estáticos salvo por un detalle que
// SÍ es dinámico y el que más se agradece: los comandos que reciben una máquina
// (logs, freeze, thaw…) completan con los IDs vivos preguntándole a `kling ps -q`
// —que existe justo para esto: IDs pelados, un por línea, sin jq ni awk—.
//
// Uso: `source <(kling completion bash)` o `source <(kling completion zsh)`.

// completionCommands es la lista de comandos de primer nivel, en el orden del
// dispatcher de main(). Si se añade un comando nuevo, va aquí.
// coreSubcommands son los subcomandos del núcleo que se completan.
var coreSubcommands = [][2]string{
	{"volume|volumes", "create ls rm populate"},
	{"images", "ls toolchain recipe rm build cat put copy"},
	{"context", "ls use add rm"},
	{"config", "show path set"},
	{"plugins", "ls"},
	{"sandbox|sandboxes", "create ls renew rm"},
	{"jev", "train eval predict inspect"},
}

// machineArgs son los comandos del núcleo que reciben un id de máquina.
const machineArgs = "logs|freeze|thaw|stop|rm|top|commit|mmds|squeeze|exec|shell|resize|inspect"

func cmdCompletion(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kling completion [bash|zsh]\n" +
			"  bash:  source <(kling completion bash)\n" +
			"  zsh:   source <(kling completion zsh)   (after `autoload -U compinit && compinit`)")
	}
	switch args[0] {
	case "bash":
		fmt.Print(completionScript(false))
	case "zsh":
		fmt.Print(completionScript(true))
	default:
		return fmt.Errorf("unsupported shell %q: use bash or zsh", args[0])
	}
	return nil
}

// completionCommands son los comandos de primer nivel: los del núcleo y los que
// aportan las extensiones instaladas. El script es estático: tras instalar una
// extensión hay que volver a cargarlo.
func completionCommands() string {
	var out []string
	for _, c := range coreCommands {
		if c != "dial-stdio" && c != "volumes" && c != "builder" {
			out = append(out, c)
		}
	}
	for _, c := range extensions().Commands() {
		out = append(out, c.Name)
	}
	return strings.Join(out, " ")
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
	for _, sc := range coreSubcommands {
		add(sc[0], sc[1])
	}
	for _, c := range extensions().Commands() {
		if len(c.Subcommands) > 0 {
			add(c.Name, strings.Join(c.Subcommands, " "))
		}
	}
	if zsh {
		fmt.Fprintf(&cases, "        %s) compadd -- ${(f)\"$(kling ps -q 2>/dev/null)\"} ;;\n", machineArgs)
		return fmt.Sprintf(zshCompletion, completionCommands(), cases.String())
	}
	fmt.Fprintf(&cases, "        %s) COMPREPLY=( $(compgen -W \"$(kling ps -q 2>/dev/null)\" -- \"$cur\") ); return ;;\n", machineArgs)
	return fmt.Sprintf(bashCompletion, completionCommands(), cases.String())
}

const bashCompletion = `# kling bash completion.  Load with:  source <(kling completion bash)
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
