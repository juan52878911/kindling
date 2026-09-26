// Command kindling-domotica es el ejemplo grande de kindling: una habitación
// domótica de demo que decide qué hacer con órdenes de voz (como texto) con
// modelos serverless en microVMs. No es parte de kindling ni una extensión de
// `kling`: es un programa aparte que usa kindling como cualquier aplicación,
// por su API HTTP (pkg/api, el gateway de IA) y embebiendo el gateway de IA
// (pkg/aigw) con su propio dominio de intenciones.
//
//	kindling-domotica gateway -config ai.json     el gateway de IA con el dominio "smart-room"
//	kindling-domotica room -gateway ai.sock       la página de la habitación
//	kindling-domotica decide "enciende la luz"    las capas 1-2 en el proceso, sin daemon
//
// Ver README.md y docs/domotica.md.
package main

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/juan52878911/kindling/examples/domotica/internal/tools"
)

// Version se fija al compilar: -ldflags "-X main.Version=...".
var Version = "dev"

const usage = `usage: kindling-domotica <command> [options]

The smart-home demo of kindling: a program that uses kindling, not part of it.

  gateway [-config ai.json] [-socket S]         kindling's AI gateway (pkg/aigw) with the smart-room
                                                intent domain built in: serves the demo's tasks
  room [-listen A] [-gateway G] [-offline]      the demo room web page (talks to the gateway and
                                                to the daemon for the machines panel)
` + tools.Usage + `
  version                                       prints the version

Run "kindling-domotica <command> -h" for the options of each command.
`

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:]); err != nil {
		log.Fatal("kindling-domotica: ", err)
	}
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "gateway":
		return runGateway(args[1:])
	case "room":
		return runRoom(args[1:])
	case "version", "-version", "--version":
		fmt.Println("kindling-domotica", Version)
		return nil
	}
	if cmd, ok := tools.Commands[args[0]]; ok {
		return cmd(args[1:])
	}
	return fmt.Errorf("unknown command %q (commands: %s)\n\n%s", args[0], strings.Join(commandNames(), ", "), usage)
}

func commandNames() []string {
	out := []string{"gateway", "room", "version"}
	for n := range tools.Commands {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
