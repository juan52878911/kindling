package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/config"
	"github.com/juan52878911/kindling/pkg/plugin"
)

// mcpExtension es lo que kindling-mcp aporta a `kling`: su manifiesto (comandos,
// gancho de status, unidades de systemd) y los manejadores de cada comando.
func mcpExtension() *plugin.Builtin {
	return &plugin.Builtin{
		Manifest: plugin.Manifest{
			ManifestVersion: plugin.ManifestVersion,
			Name:            "mcp",
			Version:         strings.TrimPrefix(Version, "v"),
			// Anotaciones, store, constructores y ficheros en imágenes.
			MinKling: "0.6.0",
			Summary:  "hosts MCP servers on demand in microVMs",
			Commands: mcpCommands,
			Hooks:    []string{plugin.HookStatus},
			// kling up las arranca junto al daemon si están instaladas.
			Units: []string{"kling-gateway.service", "kling-heal.timer"},
			Config: []plugin.ConfigKey{
				{Key: "memory.enabled", Type: "bool", Help: "the gateway records which tool resolved each request"},
				{Key: "memory.service", Type: "string", Help: "linked MCP service used as usage memory (default engram)"},
				{Key: "hosts", Type: "string", Help: "several daemons for `gateway`: name=endpoint,name2=endpoint2 (same format as -hosts; default: one host, the active context)"},
			},
		},
		Commands: map[string]func([]string) error{
			"search":  cmdSearch,
			"add":     cmdAdd,
			"mcp":     cmdMCP,
			"export":  cmdExport,
			"memory":  cmdMemory,
			"connect": cmdConnect,
			"migrate": cmdMigrate,
			"gateway": cmdGateway,
			// No está en el manifiesto: lo ejecuta el daemon como constructor.
			"builder": cmdBuilder,
		},
		Hooks: map[string]func([]string, io.Writer) error{
			plugin.HookStatus: mcpStatusHook,
		},
	}
}

var mcpCommands = []plugin.Command{
	{
		Name: "search", Group: "CATALOG",
		Summary: "searches the official MCP server registry",
		Usage: `  search <query>                                   searches the official
                                                   MCP server registry
`,
	},
	{
		Name: "add", Group: "CATALOG",
		Summary: "packages an MCP server, imports it and leaves it frozen as a service",
		Usage: `  add <server> [-as name] [-arg value]             packages it, imports it and
      [-volume NAME[:/mount][:ro]] (repeatable)    leaves it frozen as a service
`,
	},
	{
		Name: "mcp", Group: "MCP SERVICES",
		Summary:     "MCP services: import, list, refresh, verify, health, heal, link",
		Subcommands: []string{"import", "list", "ls", "refresh", "refresh-bridge", "verify", "health", "heal", "link", "unlink"},
		Usage: `  mcp import <service> -image <img>                turns an MCP server into a
      [-cpus N] [-mem MiB]                         service: it starts, asks
      [-egress none|internet|allowlist]            what it can do, freezes it and
      [-allow dom1,dom2]                           saves its catalog. All of this
      [-volume NAME[:/mount][:ro]] (repeatable)    ends up BAKED into the snapshot
  mcp list [-v] [-json]                            services and their tools
  mcp refresh <service>                            recaptures the catalog
  mcp refresh-bridge [image...]                    puts the current bridge inside
                                                   the images already built
  mcp verify <service> [-deep]                     exercises it for real: calls a
                                                   tool and checks the guest's DNS
  mcp health                                       probes every service and
                                                   records the result
  mcp heal [-dry-run]                              rebuilds only what a host
                                                   reboot (TSC) invalidated
  mcp link <name> <url>                            links an EXTERNAL MCP server
                                                   (e.g. your engram) without putting
                                                   it in a microVM
  mcp unlink <name>                                unlinks it
`,
	},
	{
		Name: "export", Group: "MCP SERVICES",
		Summary: "browsable topology in HTML",
		Usage: `  export [-o file.html]                            browsable topology in HTML
`,
	},
	{
		Name: "memory", Group: "USAGE MEMORY (optional, off by default)",
		Summary:     "usage memory for the gateway (optional, off by default)",
		Subcommands: []string{"status", "enable", "disable", "install-service"},
		Usage: `  memory status                                    whether it's active and on what
  memory enable [-service N]                       enables it; uses engram by default
  memory disable                                   disables it
  memory install-service                           installs the local bridge as a
                                                   permanent service (macOS)
`,
	},
	{
		Name: "connect", Group: "CONNECT YOUR AGENT",
		Summary: "connects your AI agent to the gateway",
		Usage: `  connect                                          step-by-step guide
  connect -all                                     ONE entry for all
                                                   services: inventory at
                                                   handshake, schemas on demand
  connect -all -only eco,files                     only those services
  connect -all -expand                             full catalog (uses more
                                                   context)
  connect <service>                                a single service
  connect ... -install all                         writes to ALL detected
                                                   agents: Claude Code,
                                                   opencode, Cursor, VS Code,
                                                   Windsurf, Cline and Zed
  connect ... -install <client>                    just that one
  connect ... -token T                             uses that token instead of
                                                   gateway.token
`,
	},
	{
		Name: "migrate", Group: "CONNECT YOUR AGENT",
		Summary: "moves an existing MCP to kindling without rewriting its skills",
		Usage: `  migrate <mcp> -install <client>                  moves an existing MCP to
                                                   kindling WITHOUT rewriting the
                                                   skills that use it (keeps its
                                                   name and tools)
`,
	},
	{
		Name: "gateway", Group: "GATEWAY",
		Summary: "routes MCP calls to microVMs on demand",
		Usage: `  gateway [-listen ADDR] [-idle DUR] [-ephemeral]  routes MCP calls to microVMs
                                                   on demand. With -ephemeral,
                                                   each action runs in its own
                                                   machine, which dies when it ends
          [-prewarm N]                             ready instances per service
                                                   (only -ephemeral)
          [-keepwarm N]                            N popular services with their
                                                   primary warm (persistent;
                                                   avoids cold start on Mac)
          [-memory SVC]                            agent memory service
          [-hosts name=endpoint,name2=endpoint2]   several daemons instead of
                                                   one (default: mcp.hosts, or
                                                   one host, the active
                                                   context). /mcp/<service>
                                                   goes to the host that has
                                                   it; /mcp/_all combines all
                                                   of them
          [-no-auth] [-pprof]                      no token / with profiling; both
                                                   require listening on
                                                   loopback. Defaults to requiring
                                                   Authorization: Bearer with the
                                                   gateway.token token, which is
                                                   generated only the first time
`,
	},
}

// mcpStatusHook añade a `kling status` lo que es de MCP: el gateway, la salud de
// los servicios y los agentes detectados. Con -json escribe un objeto con esas
// mismas tres cosas, que `kling status -json` pone bajo extensions.mcp.
func mcpStatusHook(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	host := hostFlag(fs)
	gwFlag := fs.String("gateway", "", "gateway URL")
	asJSON := fs.Bool("json", false, "JSON output")
	_ = fs.Parse(knownFlags(fs, args))

	ctx, stop := ctxWithSignals()
	defer stop()
	cfg := loadConfig()
	c := api.NewClient(cfg.Host(*host))
	gw := strings.TrimSuffix(config.Or(*gwFlag, cfg.Gateway.URL, guessGateway(cfg.Host(*host))), "/")
	if *asJSON {
		return mcpStatusJSON(ctx, w, c, gw, cfg.Gateway.Token)
	}

	// El gateway vive en otro proceso —y a menudo en otra máquina— que el
	// daemon: que el daemon conteste no dice nada de él.
	fmt.Fprintf(w, "gateway:      %s\n", gw)
	if err := httpOK(gw + "/healthz"); err != nil {
		fmt.Fprintf(w, "  health:     ✗ not responding (%v)\n", err)
		fmt.Fprintf(w, "              start it on the daemon's host:  kling gateway -listen 0.0.0.0:8080\n")
	} else {
		fmt.Fprintf(w, "  health:     ✓ alive\n")
		fmt.Fprintf(w, "  services:   %s\n", servicesLine(gw+"/services", cfg.Gateway.Token))
	}

	// Solo si el daemon contesta: sin él no hay snapshots que leer, y la línea
	// del daemon ya dijo por qué.
	if snaps, err := c.Snapshots(ctx); err == nil && len(snaps) > 0 {
		fmt.Fprintf(w, "mcp health:   %s\n", mcpHealthLine(snaps))
	}

	det := detectedClients()
	if len(det) == 0 {
		fmt.Fprintf(w, "agents:       none detected on this machine\n")
		return nil
	}
	var names []string
	for _, cl := range det {
		names = append(names, cl.label)
	}
	fmt.Fprintf(w, "agents:       %s\n", strings.Join(names, ", "))
	fmt.Fprintf(w, "              plug them in with:  kling connect -all -install all\n")
	return nil
}

func mcpStatusJSON(ctx context.Context, w io.Writer, c *api.Client, gw, token string) error {
	type gatewayInfo struct {
		URL      string   `json:"url"`
		Healthy  bool     `json:"healthy"`
		Services []string `json:"services,omitempty"`
		Error    string   `json:"error,omitempty"`
	}
	var report struct {
		Gateway gatewayInfo `json:"gateway"`
		Agents  []string    `json:"agents"`
	}
	report.Gateway.URL = gw
	if err := httpOK(gw + "/healthz"); err != nil {
		report.Gateway.Error = err.Error()
	} else {
		report.Gateway.Healthy = true
		if names, err := gatewayServiceNames(gw+"/services", token); err != nil {
			report.Gateway.Error = err.Error()
		} else {
			report.Gateway.Services = names
		}
	}
	report.Agents = []string{}
	for _, cl := range detectedClients() {
		report.Agents = append(report.Agents, cl.label)
	}
	return json.NewEncoder(w).Encode(report)
}
