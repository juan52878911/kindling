package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func text(s string) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": s}},
	}
}

func main() {
	root := "/data/notas"
	_ = os.MkdirAll(root, 0o755)

	var mu sync.Mutex
	store := map[string]string{}
	for _, e := range list(root) {
		b, err := os.ReadFile(e)
		if err == nil {
			store[filepath.Base(e)] = string(b)
		}
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 8<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		if len(req.ID) == 0 {
			continue
		}
		resp := response{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "kindling-notas", "version": "1.0.0"},
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": []any{
				map[string]any{
					"name":        "guardar_nota",
					"description": "Writes a note with a name and content",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"nombre":    map[string]any{"type": "string"},
							"contenido": map[string]any{"type": "string"},
						},
						"required": []string{"nombre", "contenido"},
					},
				},
				map[string]any{
					"name":        "leer_nota",
					"description": "Reads the content of a note",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"nombre": map[string]any{"type": "string"}},
						"required":   []string{"nombre"},
					},
				},
				map[string]any{
					"name":        "listar_notas",
					"description": "Lists the names of all saved notes",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				},
			}}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)
			switch p.Name {
			case "guardar_nota":
				n, _ := p.Arguments["nombre"].(string)
				c, _ := p.Arguments["contenido"].(string)
				if n == "" {
					resp.Error = &rpcError{Code: -32602, Message: "missing name"}
					break
				}
				mu.Lock()
				store[n] = c
				_ = os.WriteFile(filepath.Join(root, n), []byte(c), 0o644)
				mu.Unlock()
				resp.Result = text(fmt.Sprintf("note %q saved (%d bytes)", n, len(c)))
			case "leer_nota":
				n, _ := p.Arguments["nombre"].(string)
				mu.Lock()
				v, ok := store[n]
				mu.Unlock()
				if !ok {
					resp.Error = &rpcError{Code: -32602, Message: "does not exist: " + n}
					break
				}
				resp.Result = text(v)
			case "listar_notas":
				mu.Lock()
				keys := make([]string, 0, len(store))
				for k := range store {
					keys = append(keys, k)
				}
				mu.Unlock()
				resp.Result = text(strings.Join(keys, ", "))
			default:
				resp.Error = &rpcError{Code: -32602, Message: "unknown tool: " + p.Name}
			}
		case "ping":
			resp.Result = map[string]any{}
		default:
			resp.Error = &rpcError{Code: -32601, Message: "unsupported method: " + req.Method}
		}
		b, _ := json.Marshal(resp)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
}

func list(dir string) []string {
	var out []string
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}
