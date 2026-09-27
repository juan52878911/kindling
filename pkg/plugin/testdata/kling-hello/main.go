// kling-hello es una extensión de prueba para los tests de pkg/plugin.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/juan52878911/kindling/pkg/plugin"
)

func main() {
	plugin.Main(plugin.Manifest{
		Name:    "hello",
		Version: "1.2.3",
		Summary: "test extension",
		Commands: []plugin.Command{
			{Name: "hello", Group: "HELLO", Summary: "says hello", Subcommands: []string{"world"}},
			{Name: "fail", Group: "HELLO", Summary: "exits with 7", TopLevel: true},
			{Name: "ps", Summary: "tries to take over a core command", TopLevel: true},
		},
		Config: []plugin.ConfigKey{{Key: "greeting", Type: "string"}},
		Hooks:  []string{plugin.HookStatus, plugin.HookUp},
	}, map[string]func([]string) error{
		"hello": func(args []string) error {
			fmt.Printf("hola %s api=%s config=%s\n", strings.Join(args, " "),
				os.Getenv("KLING_PLUGIN_API"), os.Getenv("KLING_CONFIG"))
			return nil
		},
		"fail": func([]string) error { return &plugin.ExitError{Code: 7, Err: errors.New("seven")} },
		"ps":   func([]string) error { return nil },
	}, map[string]func([]string, io.Writer) error{
		plugin.HookStatus: func(args []string, w io.Writer) error {
			if len(args) > 0 && args[0] == "-json" {
				fmt.Fprintln(w, `{"ok":true}`)
				return nil
			}
			fmt.Fprintln(w, "hello:        ✓ fine")
			return nil
		},
		plugin.HookUp: func([]string, io.Writer) error {
			time.Sleep(10 * time.Second)
			return nil
		},
	})
}
