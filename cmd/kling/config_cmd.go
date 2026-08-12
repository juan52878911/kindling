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
		return fmt.Errorf("uso: kling context [ls|use <nombre>|add <nombre> <host>|rm <nombre>]")
	}
}

func contextList() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if len(cfg.Contexts) == 0 {
		fmt.Println("Sin contextos. Añade uno con:")
		fmt.Println("  kling context add lab ssh://usuario@host")
		fmt.Printf("\nSin contexto activo se usa el socket local (%s).\n", transport.DefaultSocket)
		return nil
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "\tNOMBRE\tHOST\tDESCRIPCIÓN")
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
		fmt.Printf("\nOJO: $KLING_HOST=%s tiene prioridad sobre el contexto activo.\n", v)
	}
	return nil
}

func contextUse(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("uso: kling context use <nombre>")
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
		fmt.Printf("sin contexto: se usará el socket local (%s)\n", transport.DefaultSocket)
		return nil
	}
	if _, ok := cfg.Contexts[name]; !ok {
		return fmt.Errorf("no existe el contexto %q (mira `kling context ls`)", name)
	}
	cfg.CurrentContext = name
	if err := cfg.Save(); err != nil {
		return err
	}
	fmt.Printf("contexto activo: %s (%s)\n", name, cfg.Contexts[name].Host)
	return nil
}

func contextAdd(args []string) error {
	fs := flag.NewFlagSet("context add", flag.ExitOnError)
	desc := fs.String("description", "", "descripción")
	use := fs.Bool("use", true, "activarlo tras añadirlo")

	// El paquete flag deja de parsear en el primer argumento posicional, así que
	// `context add lab ssh://... -description X` perdería el flag en silencio.
	// Se separan a mano para que el orden no importe.
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() < 2 {
		return fmt.Errorf("uso: kling context add <nombre> <host>\n" +
			"  host: ssh://usuario@maquina  o  /run/kling.sock")
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

	fmt.Printf("contexto %q -> %s\n", name, host)
	// Comprobarlo aquí ahorra descubrir el error en la primera orden de verdad.
	ctx, stop := ctxWithSignals()
	defer stop()
	if info, err := api.NewClient(host).Info(ctx); err != nil {
		fmt.Printf("aviso: no alcanzo el daemon todavía: %v\n", err)
	} else {
		fmt.Printf("daemon %s alcanzado, %d máquinas\n", info.Version, info.Machines)
	}
	return nil
}

func contextRemove(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("uso: kling context rm <nombre>")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	name := args[0]
	if _, ok := cfg.Contexts[name]; !ok {
		return fmt.Errorf("no existe el contexto %q", name)
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

// reorder pone los flags delante de los posicionales.
func reorder(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) > 1 && a[0] == '-' {
			flags = append(flags, a)
			// Un flag con valor separado se lleva el siguiente argumento.
			if !strings.Contains(a, "=") && i+1 < len(args) &&
				(len(args[i+1]) == 0 || args[i+1][0] != '-') && !isBoolFlag(a) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, a)
	}
	return append(flags, positional...)
}

// isBoolFlag lista los flags que no llevan valor, para no tragarse el siguiente
// argumento al reordenar.
func isBoolFlag(f string) bool {
	switch strings.TrimLeft(f, "-") {
	case "use", "no-use", "all", "expand":
		return true
	}
	return false
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
			return fmt.Errorf("uso: kling config set <clave> <valor>\n" +
				"  p. ej.: kling config set defaults.image min")
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
		return fmt.Errorf("uso: kling config [show|path|set <clave> <valor>]")
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
	fmt.Printf("fichero:  %s\n", config.Path())
	if _, err := os.Stat(config.Path()); os.IsNotExist(err) {
		fmt.Println("          (aún no existe; se crea al escribir algo)")
	}
	ctxName := cfg.CurrentContext
	if ctxName == "" {
		ctxName = "(ninguno: socket local)"
	}
	fmt.Printf("contexto: %s\n\n", ctxName)

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "CLAVE\tVALOR")
	for _, kv := range cfg.Keys() {
		v := kv[1]
		if v == "" {
			v = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\n", kv[0], v)
	}
	return tw.Flush()
}
