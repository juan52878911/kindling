package main

import "fmt"

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
const completionCommands = "up status run ps logs freeze thaw stop rm squeeze " +
	"mmds commit snapshots images rmi topo top export events info context config " +
	"volume connect migrate mcp memory add search gateway daemon completion version help"

func cmdCompletion(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kling completion [bash|zsh]\n" +
			"  bash:  source <(kling completion bash)\n" +
			"  zsh:   source <(kling completion zsh)   (after `autoload -U compinit && compinit`)")
	}
	switch args[0] {
	case "bash":
		fmt.Print(bashCompletion)
	case "zsh":
		fmt.Print(zshCompletion)
	default:
		return fmt.Errorf("unsupported shell %q: use bash or zsh", args[0])
	}
	return nil
}

const bashCompletion = `# kling bash completion.  Load with:  source <(kling completion bash)
_kling() {
    local cur prev cword
    cur="${COMP_WORDS[COMP_CWORD]}"
    prev="${COMP_WORDS[COMP_CWORD-1]}"
    cword=$COMP_CWORD

    local commands="` + completionCommands + `"

    if [ "$cword" -eq 1 ]; then
        COMPREPLY=( $(compgen -W "$commands" -- "$cur") )
        return
    fi

    case "${COMP_WORDS[1]}" in
        mcp)            COMPREPLY=( $(compgen -W "import list refresh link unlink" -- "$cur") ); return ;;
        volume|volumes) COMPREPLY=( $(compgen -W "create ls rm populate" -- "$cur") ); return ;;
        images)         COMPREPLY=( $(compgen -W "ls refresh toolchain recipe" -- "$cur") ); return ;;
        context)        COMPREPLY=( $(compgen -W "ls use add rm" -- "$cur") ); return ;;
        config)         COMPREPLY=( $(compgen -W "show path set" -- "$cur") ); return ;;
        memory)         COMPREPLY=( $(compgen -W "status enable disable install-service" -- "$cur") ); return ;;
        logs|freeze|thaw|stop|rm|top|commit|mmds|squeeze)
            COMPREPLY=( $(compgen -W "$(kling ps -q 2>/dev/null)" -- "$cur") ); return ;;
    esac
}
complete -F _kling kling
`

const zshCompletion = `# kling zsh completion.  Load with:  source <(kling completion zsh)
_kling() {
    local -a commands
    commands=(` + completionCommands + `)

    if (( CURRENT == 2 )); then
        compadd -- $commands
        return
    fi

    case "${words[2]}" in
        mcp)            compadd -- import list refresh link unlink ;;
        volume|volumes) compadd -- create ls rm populate ;;
        images)         compadd -- ls refresh toolchain recipe ;;
        context)        compadd -- ls use add rm ;;
        config)         compadd -- show path set ;;
        memory)         compadd -- status enable disable install-service ;;
        logs|freeze|thaw|stop|rm|top|commit|mmds|squeeze)
            compadd -- ${(f)"$(kling ps -q 2>/dev/null)"} ;;
    esac
}
compdef _kling kling
`
