package main

import (
	"flag"
	"fmt"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
)

// mcpIsolation es `kling mcp isolation <servicio> [service|session]`: muestra o
// cambia si las sesiones del servicio comparten instancia o tienen cada una su
// microVM. Se guarda como anotación del snapshot, así que cambiarlo no pide
// reimportar; el gateway lo relee en segundos y vale para las sesiones NUEVAS.
// Las que ya estaban abiertas siguen donde estaban.
func mcpIsolation(args []string) error {
	fs := flag.NewFlagSet("mcp isolation", flag.ExitOnError)
	host := hostFlag(fs)
	if err := fs.Parse(reorderFor(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		return fmt.Errorf("usage: kling mcp isolation <service> [%s|%s]", mcp.IsolationService, mcp.IsolationSession)
	}
	service := fs.Arg(0)

	ctx, stop := ctxWithSignals()
	defer stop()
	c := api.NewClient(loadConfig().Host(*host))
	s, err := c.Snapshot(ctx, service)
	if err != nil {
		return fmt.Errorf("service %q: %w", service, err)
	}

	if fs.NArg() == 1 {
		fmt.Printf("%s: %s\n", service, describeIsolation(mcp.Isolation(s)))
		return nil
	}
	mode := fs.Arg(1)
	if !mcp.ValidIsolation(mode) {
		return fmt.Errorf("unknown isolation %q: use %s or %s", mode, mcp.IsolationService, mcp.IsolationSession)
	}
	if err := mcp.SetIsolation(ctx, c, s.Name, mode); err != nil {
		return err
	}
	fmt.Printf("%s: %s\n", service, describeIsolation(mode))
	fmt.Println("It applies to new sessions; the ones already open stay where they are.")
	return nil
}

func describeIsolation(mode string) string {
	if mode == mcp.IsolationSession {
		return "session — each MCP session has its own microVM and disk, destroyed when the session ends"
	}
	return "service — sessions share the instance and its disk (each has its own process)"
}
