package main

// Los agentes de IA donde kindling sabe instalarse.
//
// Cada uno inventó su propio formato para decir lo mismo —"hay un servidor MCP
// en esta URL"— así que esto es una tabla de traducción, no una abstracción. Se
// verificó contra la documentación de cada cliente, y hay dos trampas que no se
// adivinan leyendo a los demás:
//
//   - Windsurf usa `serverUrl`, no `url`.
//   - Cline exige `"type": "streamableHttp"` en camelCase; con "streamable-http"
//     o sin el campo se cae a SSE y deja de funcionar.
//
// Y una limitación ajena: Zed todavía no habla MCP remoto por HTTP. No se
// parchea su configuración; se le enseña el rodeo con mcp-remote.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// client describe cómo registrar un endpoint MCP en un agente.
type client struct {
	name  string // identificador para -install
	label string // cómo se llama para un humano
	bin   string // ejecutable que delata que está instalado, si lo hay

	// paths son los candidatos de fichero de configuración. Gana el primero
	// que exista; si no existe ninguno se crea el primero.
	paths   []string
	section string // clave de nivel superior donde viven los servidores

	// entry construye la entrada del servidor tal y como la espera ESTE cliente.
	entry func(url, token string) map[string]any

	// cliArgs es la vía oficial, cuando la hay. Se prefiere siempre: evita
	// tocar un fichero que es del usuario y sobrevive a que cambien el formato.
	cliArgs func(name, url, token string) []string

	// manual, si no está vacío, es el motivo por el que NO se parchea el
	// fichero. Se imprime el fragmento y se explica. Fallar en seguro.
	manual string

	// snippet describe la configuración a mano. Si es nil se deriva de entry.
	snippet func(name, url, token string) string
}

// clients es la tabla. El orden es el de instalación con -install a secas.
func clients() []client {
	return []client{
		{
			name:    "claude-code",
			label:   "Claude Code",
			bin:     "claude",
			paths:   []string{home(".claude.json")},
			section: "mcpServers",
			entry: func(url, token string) map[string]any {
				return withAuth(map[string]any{"type": "http", "url": url}, token)
			},
			cliArgs: func(name, url, token string) []string {
				// --scope user, o `claude mcp add` lo registra en el ámbito
				// LOCAL: la entrada solo existiría dentro del directorio desde
				// el que se lanzó kling, que casi nunca es lo que se quiere y
				// además es invisible en el resultado.
				a := []string{"mcp", "add", "--scope", "user", "--transport", "http", name, url}
				if token != "" {
					a = append(a, "--header", "Authorization: Bearer "+token)
				}
				return a
			},
		},
		{
			name:    "opencode",
			label:   "opencode",
			bin:     "opencode",
			paths:   []string{home(".config", "opencode", "opencode.json")},
			section: "mcp",
			entry: func(url, token string) map[string]any {
				return withAuth(map[string]any{"type": "remote", "url": url, "enabled": true}, token)
			},
		},
		{
			name:    "cursor",
			label:   "Cursor",
			paths:   []string{home(".cursor", "mcp.json")},
			section: "mcpServers",
			entry: func(url, token string) map[string]any {
				return withAuth(map[string]any{"url": url}, token)
			},
		},
		{
			// Sin `paths`: la ruta del mcp.json de usuario ha cambiado entre
			// versiones de VS Code y las fuentes se contradicen. La CLI es
			// estable y es la que recomienda Microsoft, así que se usa esa o
			// se imprime el fragmento; adivinar la ruta y crear un fichero
			// donde no toca sería peor que no hacer nada.
			name:    "vscode",
			label:   "VS Code",
			bin:     "code",
			section: "servers",
			entry: func(url, token string) map[string]any {
				return withAuth(map[string]any{"type": "http", "url": url}, token)
			},
			cliArgs: func(name, url, token string) []string {
				e := map[string]any{"name": name, "type": "http", "url": url}
				return []string{"--add-mcp", string(mustJSON(withAuth(e, token)))}
			},
		},
		{
			name:    "windsurf",
			label:   "Windsurf",
			paths:   []string{home(".codeium", "windsurf", "mcp_config.json")},
			section: "mcpServers",
			entry: func(url, token string) map[string]any {
				// serverUrl, no url. Con `url` Windsurf lo ignora en silencio.
				return withAuth(map[string]any{"serverUrl": url}, token)
			},
		},
		{
			name:    "cline",
			label:   "Cline",
			paths:   []string{filepath.Join(vscodeUserDir(), "globalStorage", "saoudrizwan.claude-dev", "settings", "cline_mcp_settings.json")},
			section: "mcpServers",
			entry: func(url, token string) map[string]any {
				return withAuth(map[string]any{
					"type": "streamableHttp", "url": url,
					"disabled": false, "autoApprove": []any{},
				}, token)
			},
		},
		{
			name:   "zed",
			label:  "Zed",
			paths:  []string{home(".config", "zed", "settings.json")},
			manual: "Zed todavía no habla MCP remoto por HTTP, y su settings.json admite comentarios que un parcheo automático destruiría",
			snippet: func(name, url, token string) string {
				// El rodeo es un proxy de stdio que reenvía a la URL.
				args := []string{"-y", "mcp-remote", url}
				if token != "" {
					args = append(args, "--header", "Authorization: Bearer "+token)
				}
				return fmt.Sprintf("  en ~/.config/zed/settings.json, dentro de \"context_servers\":\n"+
					"    %q: {\"source\": \"custom\", \"command\": {\"path\": \"npx\", \"args\": %s}}",
					name, mustJSON(args))
			},
		},
	}
}

// errNotJSON marca un fichero que no se puede parchear sin romperlo.
//
// Separado del resto de errores porque la respuesta correcta no es rendirse:
// es enseñar el fragmento para que lo pegue quien sí entiende ese fichero.
var errNotJSON = errors.New("no es JSON válido (¿tiene comentarios?)")

// withAuth añade la cabecera del token. Los seis clientes que hablan HTTP
// coinciden en llamarla `headers`, que es la única cosa en la que coinciden.
func withAuth(m map[string]any, token string) map[string]any {
	if token != "" {
		m["headers"] = map[string]any{"Authorization": "Bearer " + token}
	}
	return m
}

func home(rel ...string) string {
	h, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(rel...)
	}
	return filepath.Join(append([]string{h}, rel...)...)
}

// vscodeUserDir es el perfil de usuario de VS Code, donde las extensiones
// guardan su estado. Cline vive ahí dentro.
func vscodeUserDir() string {
	switch runtime.GOOS {
	case "darwin":
		return home("Library", "Application Support", "Code", "User")
	case "windows":
		return filepath.Join(os.Getenv("APPDATA"), "Code", "User")
	default:
		return home(".config", "Code", "User")
	}
}

// configPath devuelve el fichero a tocar: el primero que exista, o el primero
// a secas si no hay ninguno todavía.
func (c client) configPath() string {
	for _, p := range c.paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if len(c.paths) == 0 {
		return ""
	}
	return c.paths[0]
}

// detected dice si el cliente está instalado en esta máquina.
//
// Vale con que exista el directorio de su configuración: la app está puesta
// aunque todavía no tenga ningún servidor MCP registrado, y ese es justo el
// caso que interesa.
func (c client) detected() bool {
	if c.bin != "" {
		if _, err := exec.LookPath(c.bin); err == nil {
			return true
		}
	}
	for _, p := range c.paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
		if fi, err := os.Stat(filepath.Dir(p)); err == nil && fi.IsDir() {
			return true
		}
	}
	return false
}

// text es el fragmento de configuración manual.
func (c client) text(name, url, token string) string {
	if c.snippet != nil {
		return c.snippet(name, url, token)
	}
	var b strings.Builder
	if c.cliArgs != nil {
		fmt.Fprintf(&b, "  %s %s\n\n", c.bin, strings.Join(quoteAll(c.cliArgs(name, url, token)), " "))
	}
	if p := c.configPath(); p != "" {
		fmt.Fprintf(&b, "  en %s, dentro de %q:\n", p, c.section)
	} else {
		fmt.Fprintf(&b, "  o a mano, dentro de %q:\n", c.section)
	}
	fmt.Fprintf(&b, "    %q: %s", name, mustJSON(c.entry(url, token)))
	return b.String()
}

// install registra el endpoint en este cliente.
func (c client) install(name, url, token string) error {
	// 1. La vía oficial, si la hay: no toca el fichero del usuario y sobrevive
	//    a que el cliente cambie su formato por debajo.
	if c.cliArgs != nil {
		if _, err := exec.LookPath(c.bin); err == nil {
			out, err := exec.Command(c.bin, c.cliArgs(name, url, token)...).CombinedOutput()
			if err == nil {
				fmt.Printf("✓ %s — añadido con `%s`\n", c.label, c.bin)
				if s := strings.TrimSpace(string(out)); s != "" {
					fmt.Printf("  %s\n", s)
				}
				return nil
			}
			fmt.Printf("aviso: `%s` falló (%v)\n", c.bin, err)
		}
	}

	// 2. Clientes que no se pueden parchear: se explica y se enseña.
	if c.manual != "" || len(c.paths) == 0 {
		reason := c.manual
		if reason == "" {
			reason = "no tengo una ruta de configuración fiable para tu versión"
		}
		fmt.Printf("→ %s — hazlo a mano: %s\n%s\n", c.label, reason, c.text(name, url, token))
		return nil
	}

	// 3. Parcheo, con respaldo y escritura atómica.
	err := patchJSON(c.configPath(), c.section, name, c.entry(url, token),
		fmt.Sprintf("Reinicia %s y pregúntale qué herramientas tiene.", c.label))
	if errors.Is(err, errNotJSON) {
		fmt.Printf("→ %s — no lo toco: %v\n  Se queda como está. Pega esto tú:\n%s\n",
			c.label, err, c.text(name, url, token))
		return nil
	}
	return err
}

// findClient busca por nombre, admitiendo algún alias evidente.
func findClient(name string) (client, bool) {
	alias := map[string]string{
		"claude": "claude-code", "code": "vscode", "vs-code": "vscode",
	}
	if a, ok := alias[name]; ok {
		name = a
	}
	for _, c := range clients() {
		if c.name == name {
			return c, true
		}
	}
	return client{}, false
}

// clientNames lista los destinos válidos para los mensajes de error.
func clientNames() string {
	var n []string
	for _, c := range clients() {
		n = append(n, c.name)
	}
	sort.Strings(n)
	return strings.Join(n, ", ")
}

// detectedClients son los que están instalados en esta máquina.
func detectedClients() []client {
	var out []client
	for _, c := range clients() {
		if c.detected() {
			out = append(out, c)
		}
	}
	return out
}

// quoteAll entrecomilla los argumentos que llevan espacios, para que la línea
// que imprimimos se pueda pegar tal cual en una terminal.
func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		if strings.ContainsAny(a, " \t\"'{}") {
			out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		} else {
			out[i] = a
		}
	}
	return out
}
