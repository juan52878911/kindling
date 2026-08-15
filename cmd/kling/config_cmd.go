package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/config"
	"github.com/juan52878911/kindling/internal/transport"
)

// ── contextos ─────────────────────────────────────────────────────────────────

func cmdContext(args []string) error {
	if len(args) == 0 {
		return contextList()
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return contextList()
	case "use":
		return contextUse(rest)
	case "add", "set":
		return contextAdd(rest)
	case "rm", "remove":
		return contextRemove(rest)
	default:
		return fmt.Errorf("usage: kling context [ls|use <name>|add <name> <host>|rm <name>]")
	}
}

func contextList() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Contexts) == 0 {
		fmt.Println("No contexts. Add one with:")
		fmt.Println("  kling context add lab ssh://user@host")
		fmt.Printf("\nWith no active context the local socket is used (%s).\n", transport.DefaultSocket)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "\tNAME\tHOST\tDESCRIPTION")
	for _, n := range cfg.ContextNames() {
		mark := " "
		if n == cfg.CurrentContext {
			mark = "*"
		}
		c := cfg.Contexts[n]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", mark, n, c.Host, c.Description)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if v := os.Getenv("KLING_HOST"); v != "" {
		fmt.Printf("\nNOTE: $KLING_HOST=%s takes priority over the active context.\n", v)
	}
	return nil
}

func contextUse(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kling context use <name>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	name := args[0]
	if name == "-" || name == "local" && cfg.Contexts["local"] == nil {
		cfg.CurrentContext = ""
		if err := cfg.Save(); err != nil {
			return err
		}
		fmt.Printf("no context: the local socket will be used (%s)\n", transport.DefaultSocket)
		return nil
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return fmt.Errorf("context %q does not exist (see `kling context ls`)", name)
	}
	cfg.CurrentContext = name
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("active context: %s (%s)\n", name, cfg.Contexts[name].Host)
	return nil
}

func contextAdd(args []string) error {
	fs := flag.NewFlagSet("context add", flag.ExitOnError)
	desc := fs.String("description", "", "description")
	use := fs.Bool("use", true, "activate it after adding")

	// El paquete flag deja de parsear en el primer argumento posicional, así que
	// `context add lab ssh://... -description X` perdería el flag en silencio.
	// Se separan a mano para que el orden no importe.
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("usage: kling context add <name> <host>\n" +
			"  host: ssh://user@machine  or  /run/kling.sock")
	}
	name, host := fs.Arg(0), fs.Arg(1)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	cfg.Contexts[name] = &config.Context{Host: host, Description: *desc}
	if *use {
		cfg.CurrentContext = name
	}
	if err := cfg.Save(); err != nil {
		return err
	}

	fmt.Printf("context %q -> %s\n", name, host)
	// Comprobarlo aquí ahorra descubrir el error en la primera orden de verdad.
	ctx, stop := ctxWithSignals()
	defer stop()
	if info, err := api.NewClient(host).Info(ctx); err != nil {
		fmt.Printf("warning: cannot reach the daemon yet: %v\n", err)
	} else {
		fmt.Printf("daemon %s reached, %d machines\n", info.Version, info.Machines)
	}
	return nil
}

func contextRemove(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kling context rm <name>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	name := args[0]
	if _, ok := cfg.Contexts[name]; !ok {
		return fmt.Errorf("context %q does not exist", name)
	}
	delete(cfg.Contexts, name)
	if cfg.CurrentContext == name {
		cfg.CurrentContext = ""
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Println(name)
	return nil
}

// reorderFor mueve los flags delante de los posicionales para que un flag escrito
// DESPUÉS de un argumento posicional no se ignore en silencio (el paquete `flag`
// de Go deja de parsear al primer no-flag). Es la versión correcta de reorder:
//   - Consulta el flagset para saber qué flags son booleanos (y por tanto NO se
//     llevan el siguiente argumento), en vez de una lista hardcodeada.
//   - Se detiene en `--`: todo lo que sigue es el comando del servidor y se deja
//     intacto, sin reordenar.
//
// Los flags SÍ deben estar definidos en fs antes de llamar aquí (lo están: se
// define todo y luego se parsea).
func reorderFor(fs *flag.FlagSet, args []string) []string {
	isBool := func(a string) bool {
		name := strings.TrimLeft(a, "-")
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[:i]
		}
		f := fs.Lookup(name)
		if f == nil {
			return false
		}
		bf, ok := f.Value.(interface{ IsBoolFlag() bool })
		return ok && bf.IsBoolFlag()
	}
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			// Fin de las opciones: el resto es el comando del servidor.
			out := append(flags, positional...)
			return append(out, args[i:]...)
		}
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && i+1 < len(args) &&
				(len(args[i+1]) == 0 || args[i+1][0] != '-') && !isBool(a) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

// ── configuración general ─────────────────────────────────────────────────────

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return configShow()
	}
	switch args[0] {
	case "show", "get":
		return configShow()
	case "path":
		fmt.Println(config.Path())
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: kling config set <key> <value>\n" +
				"  e.g.: kling config set defaults.image min")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		if err := cfg.Set(args[1], args[2]); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		// Se reimprime desde la configuración ya guardada, no desde el
		// argumento: así los secretos salen enmascarados igual que en
		// `config show`. El valor suele venir de un `$(...)` que quien lo
		// teclea nunca llegó a ver, y no hay razón para enseñarlo ahora.
		fmt.Printf("%s = %s\n", args[1], valueOf(cfg, args[1]))
		return nil
	default:
		return fmt.Errorf("usage: kling config [show|path|set <key> <value>]")
	}
}

// valueOf busca una clave entre las que lista Keys(), que ya enmascara lo que
// no debe salir por pantalla.
func valueOf(cfg *config.Config, key string) string {
	for _, kv := range cfg.Keys() {
		if kv[0] == key {
			return kv[1]
		}
	}
	return ""
}

func configShow() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("file:     %s\n", config.Path())
	if _, err := os.Stat(config.Path()); os.IsNotExist(err) {
		fmt.Println("          (does not exist yet; created on first write)")
	}
	ctxName := cfg.CurrentContext
	if ctxName == "" {
		ctxName = "(none: local socket)"
	}
	fmt.Printf("context:  %s\n\n", ctxName)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "KEY\tVALUE")
	for _, kv := range cfg.Keys() {
		v := kv[1]
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\n", kv[0], v)
	}
	return tw.Flush()
}
