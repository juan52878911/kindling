// kling-hello es la extensión mínima de kling: la que enseña
// docs/extensions.md ("escribe tu extensión en 10 minutos").
//
// Se compila con el nombre kling-hello en el directorio de extensiones y a
// partir de ahí `kling hello` la ejecuta:
//
//	go build -o ~/.local/share/kling/plugins/kling-hello ./examples/hello-extension
//	kling hello -name Ada
//
// Solo usa la biblioteca estándar y pkg/plugin y pkg/config del núcleo, para
// que copiarla como plantilla no arrastre nada más.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// manifest es lo que la extensión declara de sí misma. Está en una variable, y
// no dentro de main, para que el test pueda compararla con lo que imprime
// --kling-manifest.
var manifest = plugin.Manifest{
	Name:     "hello",
	Version:  "0.1.0",
	MinKling: "0.13.0",
	Summary:  "the smallest kling extension",
	Commands: []plugin.Command{{
		Name:    "hello",
		Group:   "EXAMPLES",
		Summary: "prints a greeting",
		// La ayuda de `kling help` sale de aquí, con el mismo formato de
		// columnas que los comandos del núcleo.
		Usage: "  hello [-name N]                    prints a greeting (hello.greeting changes it)\n",
	}},
	Config: []plugin.ConfigKey{
		{Key: "greeting", Type: "string", Help: "word used by `kling hello` (default: hello)"},
	},
	Hooks: []string{plugin.HookStatus},
}

func main() {
	plugin.Main(manifest, map[string]func([]string) error{
		"hello": cmdHello,
	}, map[string]func([]string, io.Writer) error{
		plugin.HookStatus: hookStatus,
	})
}

func cmdHello(args []string) error {
	fs := flag.NewFlagSet("hello", flag.ContinueOnError)
	name := fs.String("name", "world", "who to greet")
	if err := fs.Parse(args); err != nil {
		// Un flag mal escrito es un error de uso: 2, como los comandos del
		// núcleo, en vez del 1 genérico.
		return &plugin.ExitError{Code: 2, Err: err}
	}
	fmt.Printf("%s, %s!\n", greeting(), *name)
	return nil
}

// hookStatus añade una línea a `kling status`; con -json escribe un objeto que
// el núcleo coloca bajo extensions.hello.
func hookStatus(args []string, w io.Writer) error {
	for _, a := range args {
		if a == "-json" || a == "--json" {
			return json.NewEncoder(w).Encode(map[string]string{"greeting": greeting()})
		}
	}
	_, err := fmt.Fprintf(w, "hello:        ✓ greeting %q\n", greeting())
	return err
}

// greeting lee extensions.hello.greeting de config.json, que es donde lo deja
// `kling config set hello.greeting hola`. Un config ausente o roto no impide
// saludar: se usa el valor por defecto.
func greeting() string {
	c, err := config.Load()
	if err != nil {
		return "hello"
	}
	var g string
	if raw, ok := c.Extensions["hello"]["greeting"]; ok && json.Unmarshal(raw, &g) == nil && g != "" {
		return g
	}
	return "hello"
}
