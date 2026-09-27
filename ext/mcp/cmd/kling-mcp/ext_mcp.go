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
//
// Desde 0.14 (manifiesto v2) los comandos viven bajo `kling mcp`: `kling mcp
// add`, `kling mcp serve`… Solo `connect` está en primer nivel: es el objetivo
// del producto y lo que teclea quien llega. Los verbos sueltos de antes (add,
// search, gateway, export, memory, migrate) los traduce el núcleo en silencio.
func mcpExtension() *plugin.Builtin {
	return &plugin.Builtin{
		Manifest: plugin.Manifest{
			ManifestVersion: plugin.ManifestVersion,
			Name:            "mcp",
			Version:         strings.TrimPrefix(Version, "v"),
			// Comandos bajo el nombre de la extensión (manifiesto v2).
			MinKling:  "0.14.0",
			Summary:   "hosts MCP servers on demand in microVMs",
			HelpGroup: "SERVE",
			Commands:  mcpCommands,
			Hooks:     []string{plugin.HookStatus},
			// kling up las arranca junto al daemon si están instaladas.
			Units: []string{"kling-gateway.service", "kling-heal.timer"},
			// `kling plugin install mcp` baja y borra el puente con ella.
			Companions: []string{"kling-bridge"},
			Config: []plugin.ConfigKey{
				{Key: "memory.enabled", Type: "bool", Help: "the gateway records which tool resolved each request"},
				{Key: "memory.service", Type: "string", Help: "linked MCP service used as usage memory (default engram)"},
				{Key: "hosts", Type: "string", Help: "several daemons for `mcp serve`: name=endpoint,name2=endpoint2 (same format as -hosts; default: one host, the active context)"},
			},
		},
		Commands: map[string]func([]string) error{
			"search":         cmdSearch,
			"add":            cmdAdd,
			"import":         mcpImport,
			"ls":             mcpList,
			"list":           mcpList,
			"inspect":        mcpInspect,
			"refresh":        mcpRefresh,
			"refresh-bridge": imagesRefresh,
			"verify":         mcpVerify,
			"health":         mcpHealth,
			"heal":           mcpHeal,
			"link":           mcpLink,
			"unlink":         mcpUnlink,
			"serve":          cmdGateway,
			"export":         cmdExport,
			"memory":         cmdMemory,
			"connect":        cmdConnect,
			"migrate":        cmdMigrate,
			// Fuera del manifiesto: alias de antes (`kling-mcp gateway`, la
			// unit de systemd de 0.13; `kling-mcp mcp ls`) y el constructor,
			// que ejecuta el daemon.
			"gateway": cmdGateway,
			"mcp":     cmdMCP,
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
		Usage: `  mcp search <query>                               searches the official MCP server
                                                   registry
`,
	},
	{
		Name: "add", Group: "CATALOG",
		Summary: "packages an MCP server, imports it and leaves it frozen as a service",
		Usage: `  mcp add <server> [-as name] [-arg value]         packages it, imports it and
      [-volume NAME[:/mount][:ro]] (repeatable)    leaves it frozen as a service
`,
	},
	{
		Name: "import", Group: "SERVICES",
		Summary: "turns an MCP server image into a frozen service with its catalog",
		Usage: `  mcp import <service> -image <img>                turns an MCP server into a
      [-cpus N] [-mem 256M]                        service: it starts, asks what it
      [-egress none|internet|allowlist]            can do, freezes it and saves its
      [-allow dom1,dom2]                           catalog. All of this ends up
      [-volume NAME[:/mount][:ro]] (repeatable)    BAKED into the template
`,
	},
	{
		Name: "ls", Group: "SERVICES",
		Summary: "services and their tools",
		Usage: `  mcp ls [-v] [-q] [-json]                         services and their tools
`,
	},
	{
		Name: "inspect", Group: "SERVICES",
		Summary: "one service: its template, catalog and health",
		Usage: `  mcp inspect <service> [-json]                    one service: template, tools
                                                   and last health check
`,
	},
	{
		Name: "refresh", Group: "SERVICES",
		Summary: "recaptures the catalog",
		Usage: `  mcp refresh <service>                            recaptures the catalog
`,
	},
	{
		Name: "refresh-bridge", Group: "SERVICES",
		Summary: "puts the current bridge inside the images already built",
		Usage: `  mcp refresh-bridge [image...]                    puts the current bridge inside
                                                   the images already built
`,
	},
	{
		Name: "verify", Group: "SERVICES",
		Summary: "exercises a service for real",
		Usage: `  mcp verify <service> [-deep]                     exercises it for real: calls a
                                                   tool and checks the guest's DNS
`,
	},
	{
		Name: "health", Group: "SERVICES",
		Summary: "probes every service and records the result",
		Usage: `  mcp health                                       probes every service and records
                                                   the result
`,
	},
	{
		Name: "heal", Group: "SERVICES",
		Summary: "rebuilds only what a host reboot invalidated",
		Usage: `  mcp heal [-dry-run]                              rebuilds only what a host reboot
                                                   (TSC) invalidated
`,
	},
	{
		Name: "link", Group: "SERVICES",
		Summary: "links an external MCP server without putting it in a microVM",
		Usage: `  mcp link <name> <url>                            links an EXTERNAL MCP server (e.g.
                                                   your engram) without a microVM
`,
	},
	{
		Name: "unlink", Group: "SERVICES",
		Summary: "unlinks an external MCP server",
		Usage: `  mcp unlink <name>                                unlinks it
`,
	},
	{
		Name: "serve", Group: "GATEWAY",
		Summary: "routes MCP calls to microVMs on demand",
		Usage: `  mcp serve [-listen ADDR] [-idle 5m]              routes MCP calls to microVMs on
      [-ephemeral] [-prewarm N]                    demand. With -ephemeral, each
                                                   action runs in its own machine,
                                                   which dies when it ends; -prewarm:
                                                   ready instances per service
      [-keepwarm N]                                N popular services with their
                                                   primary running (persistent;
                                                   avoids cold start on Mac)
      [-memory SVC]                                agent memory service
      [-hosts name=endpoint,name2=endpoint2]       several daemons instead of one
                                                   (default: mcp.hosts, or the
                                                   active context). /mcp/<service>
                                                   goes to the host that has it;
                                                   /mcp/_all combines all of them
      [-no-auth] [-pprof]                          no token / with profiling; both
                                                   require listening on loopback.
                                                   Defaults to Authorization: Bearer
                                                   with gateway.token, generated
                                                   only the first time
`,
	},
	{
		Name: "export", Group: "GATEWAY",
		Summary: "browsable topology in HTML",
		Usage: `  mcp export [-o file.html]                        browsable topology in HTML
`,
	},
	{
		Name: "memory", Group: "GATEWAY",
		Summary:     "usage memory for the gateway (optional, off by default)",
		Subcommands: []string{"status", "enable", "disable", "install-service"},
		Usage: `  mcp memory status                                whether it's active and on what
  mcp memory enable [-service N]                   enables it; uses engram by default
  mcp memory disable                               disables it
  mcp memory install-service                       installs the local bridge as a
                                                   permanent service (macOS)
`,
	},
	{
		Name: "migrate", Group: "CATALOG",
		Summary: "moves an existing MCP to kindling without rewriting its skills",
		Usage: `  mcp migrate <mcp> -install <client>              moves an existing MCP to kindling
                                                   WITHOUT rewriting the skills that
                                                   use it (keeps its name and tools)
`,
	},
	{
		Name: "connect", Group: "CONNECT YOUR AGENT", TopLevel: true,
		Summary: "connects your AI agent to the gateway",
		Usage: `  connect                                          step-by-step guide
  connect -all                                     ONE entry for all services:
                                                   inventory at handshake, schemas
                                                   on demand
  connect -all -only eco,files                     only those services
  connect -all -expand                             full catalog (uses more context)
  connect <service>                                a single service
  connect ... -install all                         writes to ALL detected agents:
                                                   Claude Code, opencode, Cursor,
                                                   VS Code, Windsurf, Cline and Zed
  connect ... -install <client>                    just that one
  connect ... -token T                             uses that token instead of
                                                   gateway.token
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
		fmt.Fprintf(w, "              start it on the daemon's host:  kling mcp serve -listen 0.0.0.0:8080\n")
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
