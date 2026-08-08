// Servidor MCP de stdio mínimo pero real: habla JSON-RPC por stdin/stdout, como
// la mayoría de los servidores MCP open source.
//
// Existe para probar que kling-bridge convierte cualquier servidor stdio en uno
// de Streamable HTTP sin que el servidor se entere. Un servidor de verdad
// (@modelcontextprotocol/server-filesystem, por ejemplo) se envuelve igual:
//
//	kling-bridge -- npx -y @modelcontextprotocol/server-filesystem /data
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
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

func main() {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64<<10), 8<<20)
	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()

	// El estado vive en el proceso: por eso un servidor stdio es de sesión única
	// y kling-bridge lanza uno por conversación.
	var calls int

	for in.Scan() {
		line := strings.TrimSpace(in.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		// Sin id es una notificación: no lleva respuesta.
		if len(req.ID) == 0 || string(req.ID) == "null" {
			continue
		}

		resp := response{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = map[string]any{
				"protocolVersion": "2025-06-18",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "kindling-echo", "version": "1.0.0"},
			}
		case "tools/list":
			resp.Result = map[string]any{"tools": []any{
				map[string]any{
					"name":        "echo",
					"description": "Devuelve el texto recibido",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"text": map[string]any{"type": "string"}},
						"required":   []string{"text"},
					},
				},
				map[string]any{
					"name":        "session_info",
					"description": "Cuántas llamadas lleva ESTA sesión: prueba que el estado es por proceso",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				},
			}}
		case "tools/call":
			calls++
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &p)

			var text string
			switch p.Name {
			case "echo":
				text = fmt.Sprintf("%v", p.Arguments["text"])
			case "session_info":
				text = fmt.Sprintf("pid=%d llamadas_en_esta_sesion=%d", os.Getpid(), calls)
			default:
				resp.Error = &rpcError{Code: -32602, Message: "herramienta desconocida: " + p.Name}
			}
			if resp.Error == nil {
				resp.Result = map[string]any{
					"content": []any{map[string]any{"type": "text", "text": text}},
				}
			}
		case "ping":
			resp.Result = map[string]any{}
		default:
			resp.Error = &rpcError{Code: -32601, Message: "método no soportado: " + req.Method}
		}

		b, _ := json.Marshal(resp)
		out.Write(b)
		out.WriteByte('\n')
		out.Flush()
	}
}
