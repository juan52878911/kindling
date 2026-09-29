package main

// Plantillas incluidas: `kling db templates` las lista y
// `kling db golden build -template <nombre> <golden>` las construye. El
// contenido está embebido en el binario (ext/db/templates); al construir se
// escribe a un directorio temporal 0700 y se pasa a db-golden.sh como
// -migrations y -seed, que es lo que ya sabe hacer.

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/juan52878911/kindling/ext/db/templates"
)

func cmdTemplates(args []string) error {
	if len(args) != 0 {
		return usageErr("usage: kling db templates")
	}
	list, err := templates.List()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TEMPLATE\tDESCRIPTION")
	for _, t := range list {
		fmt.Fprintf(w, "%s\t%s\n", t.Name, t.Summary)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "\nbuild one:  kling db golden build -template <template> <name>")
	return nil
}

// expandTemplate sustituye `-template T` de los argumentos de `golden build`
// por `-migrations DIR -seed FILE` de la plantilla escrita a un temporal. Sin
// -template devuelve los argumentos tal cual y un cleanup vacío.
func expandTemplate(rest []string) ([]string, func(), error) {
	noop := func() {}
	if len(rest) == 0 || rest[0] != "build" {
		return rest, noop, nil
	}
	var name string
	out := []string{rest[0]}
	for i := 1; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "-template" || a == "--template":
			if i+1 >= len(rest) || strings.HasPrefix(rest[i+1], "-") {
				return nil, noop, usageErr("-template needs a name (see kling db templates)")
			}
			if name != "" {
				return nil, noop, usageErr("-template given twice")
			}
			name = rest[i+1]
			i++
		case strings.HasPrefix(a, "-template=") || strings.HasPrefix(a, "--template="):
			if name != "" {
				return nil, noop, usageErr("-template given twice")
			}
			_, name, _ = strings.Cut(a, "=")
			if name == "" {
				return nil, noop, usageErr("-template needs a name (see kling db templates)")
			}
		default:
			out = append(out, a)
		}
	}
	if name == "" {
		return rest, noop, nil
	}
	for _, a := range out[1:] {
		switch strings.TrimPrefix(a, "-") {
		case "migrations", "seed", "seed-mb":
			return nil, noop, usageErr("-template excludes %s: the template brings its own migrations and seed", a)
		}
	}
	d, err := templates.Materialize(name)
	if err != nil {
		return nil, noop, usageErr("%v", err)
	}
	// Los flags primero: el nombre de la golden es el último argumento.
	args := append([]string{out[0], "-migrations", d.Migrations, "-seed", d.Seed}, out[1:]...)
	return args, d.Cleanup, nil
}
