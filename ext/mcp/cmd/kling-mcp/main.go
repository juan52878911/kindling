// kling-mcp es la extensión de kling para alojar servidores MCP bajo demanda en
// microVMs de kindling.
//
// No se teclea directamente: `kling mcp import`, `kling mcp add`, `kling connect`,
// `kling mcp serve`... los encuentra kling y le pasa el control (ver
// github.com/juan52878911/kindling/docs/extensions.md). También funciona a mano,
// con la misma forma: `kling-mcp ls`.
package main

import "github.com/juan52878911/kindling/pkg/plugin"

// Version se fija al compilar:  -ldflags "-X main.Version=..."
var Version = "dev"

func main() {
	ext := mcpExtension()
	plugin.Main(ext.Manifest, ext.Commands, ext.Hooks)
}
