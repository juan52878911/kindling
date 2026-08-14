package main

import (
	"flag"
	"fmt"
	"strings"

	"github.com/juan52878911/kindling/internal/config"
)

// cmdMigrate mueve un servidor MCP existente a kindling SIN romper las skills,
// agentes o configuraciones que ya lo usan.
//
// EL PROBLEMA que resuelve: una skill referencia las herramientas de un MCP por
// su NOMBRE —p. ej. `mcp__context7__get-library-docs`, donde `context7` es el
// nombre del servidor en la config del cliente y `get-library-docs` el de la
// herramienta—. Si al pasar ese MCP a kindling cambiara cualquiera de los dos,
// habría que reescribir la skill.
//
// LA CLAVE: el endpoint POR-SERVICIO de kindling (`/mcp/<servicio>`) es un proxy
// FIEL del servidor real que corre dentro de la microVM: expone sus herramientas
// con sus nombres y esquemas ORIGINALES, 1:1. Así que basta con reapuntar la
// entrada del cliente a ese endpoint CONSERVANDO el nombre con el que la skill
// la referencia, y todo sigue resolviendo igual. `migrate` hace exactamente eso.
//
// NO usa el agregado `/mcp/_all`: ese reúne todos los servicios bajo un único
// servidor con tres meta-herramientas y renombra todo a `servicio.herramienta`,
// lo que SÍ rompería las referencias por nombre de una skill. El agregado es
// para ahorrar contexto en uso general; `migrate` es para compatibilidad.
func cmdMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	host := hostFlag(fs)
	gwFlag := fs.String("gateway", "", "gateway URL (default: gateway.url, or inferred from context)")
	service := fs.String("service", "", "service name in kindling, if it differs from the MCP name")
	install := fs.String("install", "", "write the configuration: "+clientNames()+", or `all`")
	tokenFlag := fs.String("token", "", "gateway token (default: gateway.token)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: kling migrate <mcp-name> [-service <service>] -install <client>\n" +
			"  <mcp-name> is how your skills/config reference it; it's kept so they don't need rewriting.\n" +
			"  -service only if the service was imported into kindling under a DIFFERENT name.")
	}
	mcp := fs.Arg(0) // el nombre que la skill referencia -> se CONSERVA como clave de la entrada

	cfg := loadConfig()
	gw := config.Or(*gwFlag, cfg.Gateway.URL, guessGateway(cfg.Host(*host)))
	token := config.Or(*tokenFlag, cfg.Gateway.Token)

	entryName, url := migrateTarget(mcp, *service, gw)
	svc := config.Or(*service, mcp)

	fmt.Printf("Migrating MCP %q → kindling (service %q)\n", mcp, svc)
	fmt.Printf("Endpoint:  %s   (per-service: preserves tool names)\n", url)
	fmt.Printf("Entry:     %q   (same name your skills already use → no need to rewrite them)\n", entryName)

	// Verificarlo de verdad es la garantía del drop-in: si el endpoint no responde
	// o da 0 herramientas, la skill fallaría DENTRO del agente, que es el peor
	// sitio para descubrirlo.
	info, tools, err := probeMCP(url, token)
	if err != nil {
		fmt.Printf("Status:    ✗ %v\n\n", err)
		fmt.Println("Is the service imported and the gateway running on the daemon's host?")
		fmt.Printf("  kling mcp import %s -image %s\n", svc, svc)
		fmt.Println("  kling gateway -listen 0.0.0.0:8080")
		if *install == "" {
			return nil
		}
		fmt.Println("\nWarning: writing the configuration anyway (the gateway may be down right now),")
		fmt.Println("but check that the service responds before relying on the skill.")
	} else {
		fmt.Printf("Status:    ✓ %s · %d tool(s), with their ORIGINAL name(s): %s\n",
			info, len(tools), strings.Join(tools, ", "))
		fmt.Printf("           your skills still find them under server %q, same as before.\n", entryName)
	}
	fmt.Println()

	if *install != "" {
		if err := installConfig(*install, entryName, url, token); err != nil {
			return err
		}
		fmt.Printf("\n✓ %q is now served by kindling in an isolated microVM, without changing the skill.\n", mcp)
		return nil
	}

	printSnippets(entryName, url, token)
	fmt.Printf("\nAutomatic installation:\n")
	fmt.Printf("  kling migrate %s -install all       (all detected clients)\n", mcp)
	fmt.Printf("  kling migrate %s -install opencode\n", mcp)
	return nil
}

// migrateTarget calcula la entrada resultante de migrar un MCP a kindling: el
// NOMBRE se conserva (es el que referencia la skill) y la URL apunta al endpoint
// POR-SERVICIO, nunca al agregado. Se saca aparte para poder probar la invariante
// —conservar el nombre y usar per-servicio— sin red.
//
// service permite que el servicio en kindling se llame distinto del MCP original
// (p. ej. se importó como `ctx7`); aun así la entrada mantiene el nombre del MCP.
func migrateTarget(mcpName, service, gateway string) (entryName, url string) {
	svc := service
	if svc == "" {
		svc = mcpName
	}
	return mcpName, strings.TrimSuffix(gateway, "/") + "/mcp/" + svc
}
