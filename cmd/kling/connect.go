package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/juan52878911/kindling/internal/api"
	"github.com/juan52878911/kindling/internal/config"
)

// cmdConnect conecta una herramienta de kindling a un agente de IA.
//
// Existe porque el hueco entre "el servicio funciona" y "mi agente lo usa" son
// tres formatos de configuración distintos y una URL que hay que adivinar. El
// comando la deduce, COMPRUEBA que responde de verdad, y escribe la
// configuración donde corresponda.
func cmdConnect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	host := hostFlag(fs)
	gwFlag := fs.String("gateway", "", "URL del gateway (por defecto: gateway.url, o se deduce del contexto)")
	install := fs.String("install", "", "escribir la configuración: claude-code | opencode")
	name := fs.String("name", "", "nombre con el que aparecerá en el agente (por defecto: el del servicio)")
	if err := fs.Parse(reorder(args)); err != nil {
		return err
	}

	ctx, stop := ctxWithSignals()
	defer stop()

	cfg := loadConfig()
	gw := config.Or(*gwFlag, cfg.Gateway.URL, guessGateway(cfg.Host(*host)))

	// Sin servicio: modo guiado.
	if fs.NArg() == 0 {
		return connectGuide(ctx, cfg.Host(*host), gw)
	}
	service := fs.Arg(0)
	label := config.Or(*name, service)
	url := strings.TrimSuffix(gw, "/") + "/mcp/" + service

	fmt.Printf("Servicio:  %s\n", service)
	fmt.Printf("Endpoint:  %s\n", url)

	// Comprobarlo de verdad: una configuración que parece correcta y no responde
	// es peor que no tener ninguna, porque el fallo aparece dentro del agente.
	if info, tools, err := probeMCP(url); err != nil {
		fmt.Printf("Estado:    ✗ %v\n\n", err)
		fmt.Println("Arranca el gateway en el host del daemon:")
		fmt.Println("  kling gateway -listen 0.0.0.0:8080")
	} else {
		fmt.Printf("Estado:    ✓ %s · %d herramienta(s): %s\n", info, len(tools), strings.Join(tools, ", "))
	}
	fmt.Println()

	if *install != "" {
		return installConfig(*install, label, url)
	}

	printSnippets(label, url)
	fmt.Printf("\nInstalación automática:\n")
	fmt.Printf("  kling connect %s -install claude-code\n", service)
	fmt.Printf("  kling connect %s -install opencode\n", service)
	return nil
}

// guessGateway deduce la URL del gateway a partir del contexto activo: el daemon
// vive en la misma máquina, así que el host coincide.
func guessGateway(daemonHost string) string {
	h := daemonHost
	if rest, ok := strings.CutPrefix(h, "ssh://"); ok {
		if _, after, found := strings.Cut(rest, "@"); found {
			h = after
		} else {
			h = rest
		}
		h, _, _ = strings.Cut(h, "/")
		return "http://" + net.JoinHostPort(h, "8080")
	}
	// Socket local: el gateway estará en la misma máquina.
	return "http://127.0.0.1:8080"
}

// probeMCP hace un initialize real y devuelve el servidor y sus herramientas.
func probeMCP(url string) (string, []string, error) {
	c := &http.Client{Timeout: 30 * time.Second}

	post := func(sid, body string) (*http.Response, error) {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("Mcp-Session-Id", sid)
		}
		return c.Do(req)
	}

	resp, err := post("", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kling","version":"1"}}}`)
	if err != nil {
		return "", nil, fmt.Errorf("no responde: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	var init struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&init)
	server := init.Result.ServerInfo.Name
	if server == "" {
		server = "servidor MCP"
	} else if v := init.Result.ServerInfo.Version; v != "" {
		server += " v" + v
	}

	sid := resp.Header.Get("Mcp-Session-Id")
	tr, err := post(sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil {
		return server, nil, nil
	}
	defer tr.Body.Close()

	var tl struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	_ = json.NewDecoder(tr.Body).Decode(&tl)
	names := make([]string, 0, len(tl.Result.Tools))
	for _, t := range tl.Result.Tools {
		names = append(names, t.Name)
	}
	return server, names, nil
}

func printSnippets(name, url string) {
	fmt.Println("── Claude Code ──────────────────────────────────────")
	fmt.Printf("  claude mcp add --transport http %s %s\n\n", name, url)
	fmt.Println("  o a mano en ~/.claude.json, dentro de \"mcpServers\":")
	fmt.Printf("    %q: {\"type\": \"http\", \"url\": %q}\n\n", name, url)

	fmt.Println("── opencode ─────────────────────────────────────────")
	fmt.Println("  en ~/.config/opencode/opencode.json, dentro de \"mcp\":")
	fmt.Printf("    %q: {\"type\": \"remote\", \"url\": %q, \"enabled\": true}\n", name, url)
}

// installConfig escribe la entrada en la configuración del agente.
func installConfig(target, name, url string) error {
	switch target {
	case "claude-code", "claude":
		return installClaudeCode(name, url)
	case "opencode":
		return installOpencode(name, url)
	default:
		return fmt.Errorf("destino desconocido %q: usa claude-code u opencode", target)
	}
}

func installClaudeCode(name, url string) error {
	// Si el CLI está disponible es la vía oficial y evita tocar su fichero.
	if _, err := exec.LookPath("claude"); err == nil {
		cmd := exec.Command("claude", "mcp", "add", "--transport", "http", name, url)
		out, err := cmd.CombinedOutput()
		if err == nil {
			fmt.Printf("✓ añadido a Claude Code vía CLI\n%s", out)
			fmt.Println("\nReinicia Claude Code y pregúntale: \"¿qué herramientas tienes disponibles?\"")
			return nil
		}
		fmt.Printf("el CLI falló (%v), escribo el fichero directamente\n", err)
	}

	home, _ := os.UserHomeDir()
	return patchJSON(filepath.Join(home, ".claude.json"), "mcpServers", name,
		map[string]any{"type": "http", "url": url},
		"Reinicia Claude Code y pregúntale: \"¿qué herramientas tienes disponibles?\"")
}

func installOpencode(name, url string) error {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".config", "opencode", "opencode.json")
	return patchJSON(p, "mcp", name,
		map[string]any{"type": "remote", "url": url, "enabled": true},
		"Reinicia opencode: la herramienta aparecerá en su lista de MCP.")
}

// patchJSON inserta una entrada en una sección de un JSON conservando el resto.
//
// Se copia el fichero antes de tocarlo: estas configuraciones suelen tener horas
// de ajustes de su dueño, y kindling no es quién para arriesgarlas.
func patchJSON(path, section, key string, value any, hint string) error {
	doc := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s no es JSON válido: %w", path, err)
		}
		backup := path + ".kling-backup"
		if err := os.WriteFile(backup, b, 0o600); err != nil {
			return fmt.Errorf("no pude respaldar %s: %w", path, err)
		}
		fmt.Printf("copia de seguridad: %s\n", backup)
	} else if !os.IsNotExist(err) {
		return err
	} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	sec, _ := doc[section].(map[string]any)
	if sec == nil {
		sec = map[string]any{}
	}
	if _, exists := sec[key]; exists {
		fmt.Printf("aviso: %q ya existía en %s; se reemplaza\n", key, section)
	}
	sec[key] = value
	doc[section] = sec

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}

	fmt.Printf("✓ %q añadido a %s\n\n%s\n", key, path, hint)
	return nil
}

// connectGuide es el modo paso a paso cuando no se dice qué servicio conectar.
func connectGuide(ctx context.Context, daemonHost, gw string) error {
	fmt.Println("kling connect — conectar una herramienta a tu agente de IA")
	fmt.Println()

	snaps, err := api.NewClient(daemonHost).Snapshots(ctx)
	if err != nil {
		fmt.Printf("1. No alcanzo el daemon (%v)\n", err)
		fmt.Println("   Comprueba el contexto:  kling context ls")
		return nil
	}
	if len(snaps) == 0 {
		fmt.Println("1. Todavía no hay ningún servicio. Crea uno:")
		fmt.Println()
		fmt.Println("   make bridge")
		fmt.Println("   sudo ./scripts/80-mcp-image.sh stdio files -p \"nodejs npm\" -- \\")
		fmt.Println("        npx -y @modelcontextprotocol/server-filesystem /data")
		fmt.Println("   kling run -name files-tmpl -image files -service files")
		fmt.Println("   kling commit files-tmpl files && kling stop files-tmpl")
		fmt.Println()
		fmt.Println("2. Y vuelve aquí:  kling connect files")
		return nil
	}

	fmt.Println("Servicios disponibles:")
	for _, s := range snaps {
		n := s.Name
		if svc := s.Service(); svc != "" {
			n = svc
		}
		fmt.Printf("  · %-20s (%s de memoria compartida)\n", n, human(s.MemBytes))
	}
	fmt.Printf("\nGateway: %s\n", gw)
	fmt.Println("  Si no está arrancado, en el host del daemon:")
	fmt.Println("    kling gateway -listen 0.0.0.0:8080")
	fmt.Println()
	fmt.Println("Conecta uno:")
	first := snaps[0].Name
	if svc := snaps[0].Service(); svc != "" {
		first = svc
	}
	fmt.Printf("  kling connect %s                        ver la configuración\n", first)
	fmt.Printf("  kling connect %s -install opencode      escribirla por ti\n", first)
	fmt.Printf("  kling connect %s -install claude-code\n", first)
	return nil
}
