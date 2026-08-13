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
	gwFlag := fs.String("gateway", "", "URL del gateway (por defecto: gateway.url, o se deduce del contexto)")
	service := fs.String("service", "", "nombre del servicio en kindling, si difiere del nombre del MCP")
	install := fs.String("install", "", "escribir la configuración: "+clientNames()+", o `all`")
	tokenFlag := fs.String("token", "", "token del gateway (por defecto: gateway.token)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("uso: kling migrate <nombre-del-mcp> [-service <servicio>] -install <cliente>\n" +
			"  <nombre-del-mcp> es como lo referencian tus skills/config; se conserva para no reescribirlas.\n" +
			"  -service solo si el servicio se importó en kindling con OTRO nombre.")
	}
	mcp := fs.Arg(0) // el nombre que la skill referencia -> se CONSERVA como clave de la entrada

	cfg := loadConfig()
	gw := config.Or(*gwFlag, cfg.Gateway.URL, guessGateway(cfg.Host(*host)))
	token := config.Or(*tokenFlag, cfg.Gateway.Token)

	entryName, url := migrateTarget(mcp, *service, gw)
	svc := config.Or(*service, mcp)

	fmt.Printf("Migrando el MCP %q → kindling (servicio %q)\n", mcp, svc)
	fmt.Printf("Endpoint:  %s   (por-servicio: conserva los nombres de las herramientas)\n", url)
	fmt.Printf("Entrada:   %q   (mismo nombre que ya usan tus skills → NO hay que reescribirlas)\n", entryName)

	// Verificarlo de verdad es la garantía del drop-in: si el endpoint no responde
	// o da 0 herramientas, la skill fallaría DENTRO del agente, que es el peor
	// sitio para descubrirlo.
	info, tools, err := probeMCP(url, token)
	if err != nil {
		fmt.Printf("Estado:    ✗ %v\n\n", err)
		fmt.Println("¿Está importado el servicio y arrancado el gateway en el host del daemon?")
		fmt.Printf("  kling mcp import %s -image %s\n", svc, svc)
		fmt.Println("  kling gateway -listen 0.0.0.0:8080")
		if *install == "" {
			return nil
		}
		fmt.Println("\nAviso: escribo la configuración igual (el gateway puede estar caído ahora),")
		fmt.Println("pero comprueba que el servicio responde antes de confiar en la skill.")
	} else {
		fmt.Printf("Estado:    ✓ %s · %d herramienta(s), con su nombre ORIGINAL: %s\n",
			info, len(tools), strings.Join(tools, ", "))
		fmt.Printf("           tus skills las siguen encontrando bajo el servidor %q, igual que antes.\n", entryName)
	}
	fmt.Println()

	if *install != "" {
		if err := installConfig(*install, entryName, url, token); err != nil {
			return err
		}
		fmt.Printf("\n✓ %q lo sirve ahora kindling en una microVM aislada, sin cambiar la skill.\n", mcp)
		return nil
	}

	printSnippets(entryName, url, token)
	fmt.Printf("\nInstalación automática:\n")
	fmt.Printf("  kling migrate %s -install all       (todos los clientes detectados)\n", mcp)
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
