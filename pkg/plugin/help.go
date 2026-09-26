package plugin

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// Un solo formato de ayuda para el núcleo y las extensiones.
//
// Antes había tres: el bloque de columnas de `kling help run`, el "Usage of
// add:" crudo del paquete flag en `kling help add`, y un tercero para las
// incorporadas. Ahora todo lo que se teclea tras `kling` se describe con un
// Command (sinopsis en Usage, resumen en Summary) y se imprime desde aquí:
//
//	kling mcp add — packages an MCP server, imports it and leaves it frozen
//
//	USAGE
//	  mcp add <server> [-as name] ...
//
//	FLAGS
//	  -as string
//	        ...
//
//	See also: kling help mcp
//
// Los FLAGS se sacan del propio comando (`... -h`) en otro proceso, porque los
// FlagSet salen con os.Exit al ver -h; FlagHelpEnv le dice a ese proceso que
// no intercepte el -h otra vez.

// FlagHelpEnv se exporta al proceso que imprime los flags de un comando, para
// que sepa que no debe volver a interceptar el -h.
const FlagHelpEnv = "KLING_FLAG_HELP"

// usageColumn es la columna donde empieza la descripción en los bloques de
// Usage; los que se generan a partir de Name y Summary la respetan.
const usageColumn = 51

// Line es la línea de un comando en la ayuda cuando no trae Usage.
func Line(name, summary string) string {
	return fmt.Sprintf("  %-*s %s\n", usageColumn-3, name, summary)
}

// UsageOf es el bloque de un comando, con prefix delante del nombre en cada
// primera línea ("mcp " para que `add` salga como `mcp add`).
func UsageOf(prefix string, c Command) string {
	u := c.Usage
	if u == "" {
		u = Line(c.Name, c.Summary)
	}
	if !strings.HasSuffix(u, "\n") {
		u += "\n"
	}
	if prefix == "" {
		return u
	}
	// Solo las líneas que empiezan por el comando llevan el prefijo; las
	// continuaciones (más sangradas) y las vacías se dejan como están.
	var b strings.Builder
	for _, l := range strings.SplitAfter(u, "\n") {
		if strings.HasPrefix(l, "  "+c.Name) && !strings.HasPrefix(l, "   ") {
			l = "  " + prefix + l[2:]
		}
		b.WriteString(l)
	}
	return b.String()
}

// WriteCommands escribe los bloques de cmds (los visibles), con prefix
// delante de cada nombre.
func WriteCommands(w io.Writer, prefix string, cmds []Command) {
	for _, c := range cmds {
		if c.Hidden {
			continue
		}
		fmt.Fprint(w, UsageOf(prefix, c))
	}
}

// WriteGrouped escribe cmds en secciones por Group, en el orden en que aparece
// cada grupo; los que no tienen grupo van bajo untitled (vacío = sin título).
func WriteGrouped(w io.Writer, prefix string, cmds []Command, untitled string) {
	groups := map[string][]Command{}
	var order []string
	for _, c := range cmds {
		if c.Hidden {
			continue
		}
		g := c.Group
		if g == "" {
			g = untitled
		}
		if _, ok := groups[g]; !ok {
			order = append(order, g)
		}
		groups[g] = append(groups[g], c)
	}
	for i, g := range order {
		if g != "" {
			fmt.Fprintln(w, g)
		}
		WriteCommands(w, prefix, groups[g])
		if i < len(order)-1 {
			fmt.Fprintln(w)
		}
	}
}

// WriteHelp imprime los comandos agrupados por sección, ordenadas por nombre,
// como hacía `kling help` con las extensiones.
func WriteHelp(w io.Writer, cmds []Command) {
	sorted := append([]Command(nil), cmds...)
	sort.SliceStable(sorted, func(i, j int) bool {
		gi, gj := sorted[i].Group, sorted[j].Group
		if gi == "" {
			gi = "EXTENSIONS"
		}
		if gj == "" {
			gj = "EXTENSIONS"
		}
		return gi < gj
	})
	WriteGrouped(w, "", sorted, "EXTENSIONS")
	fmt.Fprintln(w)
}

// WriteNamespace es la ayuda de un espacio de nombres: la de `kling mcp`,
// `kling help mcp` o `kling-mcp help`. prefix es cómo se teclea ("kling mcp").
func WriteNamespace(w io.Writer, prefix string, m Manifest) {
	fmt.Fprintf(w, "%s", prefix)
	if m.Summary != "" {
		fmt.Fprintf(w, " — %s", m.Summary)
	}
	fmt.Fprintf(w, "\n\nUSAGE\n  %s <command> [options]\n\n", prefix)
	sub := strings.TrimPrefix(prefix, "kling ")
	if sub == prefix {
		sub = ""
	} else {
		sub += " "
	}
	WriteGrouped(w, sub, m.Namespaced(), "COMMANDS")
	if promoted := visible(m.Promoted()); len(promoted) > 0 {
		fmt.Fprintf(w, "\nALSO AT TOP LEVEL\n")
		WriteCommands(w, "", promoted)
	}
	fmt.Fprintf(w, "\nRun '%s <command> -h' for the flags of each one.\n", prefix)
	if m.Version != "" {
		fmt.Fprintf(w, "Extension kling-%s %s.\n", m.Name, strings.TrimPrefix(m.Version, "v"))
	}
}

// WriteCommand es la ayuda de un comando: sinopsis, bloque de uso, flags y a
// dónde volver. full es cómo se teclea ("kling mcp add"); prefix, lo que va
// delante del nombre en el bloque ("mcp "); flags, la salida de FlagHelp
// (vacía si el comando no tiene).
func WriteCommand(w io.Writer, full, prefix string, c Command, flags, seeAlso string) {
	fmt.Fprint(w, full)
	if c.Summary != "" {
		fmt.Fprintf(w, " — %s", c.Summary)
	}
	fmt.Fprintf(w, "\n\nUSAGE\n%s", UsageOf(prefix, c))
	if flags != "" {
		fmt.Fprintf(w, "\nFLAGS\n%s", flags)
	}
	if len(c.Subcommands) > 0 {
		fmt.Fprintf(w, "\nRun '%s <subcommand> -h' for the flags of each one.\n", full)
	}
	if seeAlso != "" {
		fmt.Fprintf(w, "\nSee also: %s\n", seeAlso)
	}
}

func visible(cmds []Command) []Command {
	var out []Command
	for _, c := range cmds {
		if !c.Hidden {
			out = append(out, c)
		}
	}
	return out
}

// FlagHelp ejecuta `exe args... -h` en otro proceso, con FlagHelpEnv puesto, y
// devuelve solo sus líneas de flags (las que empiezan por "  -" y sus
// continuaciones). Se acota el tiempo y el tamaño: es ayuda, no puede colgar.
// Devuelve "" si el comando no imprime flags.
func FlagHelp(exe string, args ...string) string {
	if exe == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var out limitedBuffer
	out.max = 64 << 10
	c := exec.CommandContext(ctx, exe, append(args, "-h")...)
	c.Stdout, c.Stderr = &out, &out
	c.Env = append(os.Environ(), FlagHelpEnv+"=1")
	_ = c.Run()
	return FlagLines(out.String())
}

// FlagLines deja de la salida de un `-h` del paquete flag solo la lista de
// flags: quita "Usage of x:", "usage: ..." y cualquier otra cabecera.
func FlagLines(s string) string {
	var keep []string
	in := false
	for _, l := range strings.Split(s, "\n") {
		switch {
		case strings.HasPrefix(l, "  -"):
			in = true
		case in && strings.HasPrefix(l, "    "):
			// continuación de la descripción
		default:
			in = false
		}
		if in {
			keep = append(keep, l)
		}
	}
	if len(keep) == 0 {
		return ""
	}
	return strings.Join(keep, "\n") + "\n"
}

// limitedBuffer guarda hasta max bytes y descarta el resto sin fallar: un
// escritor que falla haría que el hijo recibiera SIGPIPE.
type limitedBuffer struct {
	bytes.Buffer
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.Len(); room > 0 {
		if len(p) > room {
			l.Buffer.Write(p[:room])
		} else {
			l.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// IsHelpArg dice si a pide ayuda.
func IsHelpArg(a string) bool { return a == "-h" || a == "--help" || a == "help" }
