package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
	"github.com/juan52878911/kindling/pkg/transport"
)

// ── contextos ─────────────────────────────────────────────────────────────────

func cmdContext(args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return contextList(args)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "ls", "list":
		return contextList(rest)
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

func contextList(args []string) error {
	fs := flag.NewFlagSet("context ls", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *asJSON {
		return writeContextsJSON(os.Stdout, cfg)
	}
	if len(cfg.Contexts) == 0 {
		fmt.Println("No contexts. Add one with:")
		fmt.Println("  kling context add lab ssh://user@host")
		fmt.Printf("\nWith no active context the local socket is used (%s).\n", transport.DefaultSocketPath())
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

// writeContextsJSON lista los contextos con el que está activo marcado, y el
// endpoint que se usaría de verdad (que $KLING_HOST puede cambiar).
func writeContextsJSON(w io.Writer, cfg *config.Config) error {
	type row struct {
		Name        string `json:"name"`
		Host        string `json:"host"`
		Description string `json:"description,omitempty"`
		Current     bool   `json:"current"`
	}
	out := struct {
		Current  string `json:"current"`
		Endpoint string `json:"endpoint"`
		Contexts []row  `json:"contexts"`
	}{Current: cfg.CurrentContext, Endpoint: cfg.Host(""), Contexts: []row{}}
	for _, n := range cfg.ContextNames() {
		c := cfg.Contexts[n]
		out.Contexts = append(out.Contexts, row{n, c.Host, c.Description, n == cfg.CurrentContext})
	}
	return json.NewEncoder(w).Encode(out)
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
		fmt.Printf("no context: the local socket will be used (%s)\n", transport.DefaultSocketPath())
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
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return configShow(args)
	}
	switch args[0] {
	case "show", "get":
		return configShow(args[1:])
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
		shown := ""
		if ext, key, ck := extensionKey(args[1]); ck != nil {
			if err := cfg.SetExtension(ext, key, ck.Type, args[2]); err != nil {
				return err
			}
			shown = cfg.ExtensionValue(ext, key, ck.Type)
		} else if err := cfg.Set(args[1], args[2]); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		if shown != "" {
			fmt.Printf("%s = %s\n", args[1], shown)
			return nil
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

// extensionKey reconoce "<extensión>.<clave>" cuando la clave la declara una
// extensión instalada. Las secciones del núcleo nunca se tratan como
// extensiones, aunque una se llame igual.
func extensionKey(full string) (string, string, *plugin.ConfigKey) {
	ext, key, ok := strings.Cut(full, ".")
	if !ok {
		return "", "", nil
	}
	switch ext {
	case "defaults", "gateway":
		return "", "", nil
	}
	for _, p := range extensions().Plugins {
		if p.Name == ext && p.Err == nil {
			if ck := p.Manifest.ConfigKey(key); ck != nil {
				return ext, key, ck
			}
		}
	}
	return "", "", nil
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

func configShow(args []string) error {
	fs := flag.NewFlagSet("config show", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "JSON output (secrets masked, as in the table)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *asJSON {
		return writeConfigJSON(os.Stdout, cfg)
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
		switch {
		case v == "" && kv[0] == "daemon.vmm":
			// Sin valor también hay backend: el de la plataforma. Decirlo evita
			// que alguien crea que el daemon no tiene ninguno.
			v = "- (default: " + config.DefaultVMM(runtime.GOOS) + ")"
		case v == "":
			v = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\n", kv[0], v)
	}
	// Las claves que declaran las extensiones instaladas, con su valor.
	for _, p := range extensions().Plugins {
		if p.Err != nil {
			continue
		}
		for _, ck := range p.Manifest.Config {
			v := cfg.ExtensionValue(p.Name, ck.Key, ck.Type)
			if v == "" {
				v = "-"
			}
			fmt.Fprintf(tw, "%s.%s\t%s\n", p.Name, ck.Key, v)
		}
	}
	return tw.Flush()
}

// writeConfigJSON es `config show -json`: las mismas claves que la tabla, con
// los secretos enmascarados por Keys(), y las de las extensiones. Un valor
// vacío sale como "" (no "-"): en JSON el guion sería un valor más.
func writeConfigJSON(w io.Writer, cfg *config.Config) error {
	keys := map[string]string{}
	for _, kv := range cfg.Keys() {
		keys[kv[0]] = kv[1]
	}
	for _, p := range extensions().Plugins {
		if p.Err != nil || p.Manifest == nil {
			continue
		}
		for _, ck := range p.Manifest.Config {
			keys[p.Name+"."+ck.Key] = cfg.ExtensionValue(p.Name, ck.Key, ck.Type)
		}
	}
	_, statErr := os.Stat(config.Path())
	out := struct {
		File       string            `json:"file"`
		Exists     bool              `json:"exists"`
		Context    string            `json:"context"`
		DefaultVMM string            `json:"default_vmm"`
		Keys       map[string]string `json:"keys"`
	}{config.Path(), statErr == nil, cfg.CurrentContext, config.DefaultVMM(runtime.GOOS), keys}
	return json.NewEncoder(w).Encode(out)
}
