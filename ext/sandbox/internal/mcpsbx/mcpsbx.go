// Package mcpsbx es un servidor MCP (transporte stdio, mensajes JSON-RPC 2.0
// delimitados por línea, protocolo 2025-06-18) que deja a un agente crear y
// manejar sandboxes a través de la API /v1 del gateway.
//
// No tiene cliente HTTP propio: recibe un Doer, que en el binario es el
// cliente de `kling sbx` (mismo token, mismo control de redirecciones). Los
// tipos de JSON-RPC son mínimos y locales: ext/mcp es otro módulo y no debe
// haber dependencia entre ambos.
package mcpsbx

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

// Version del protocolo MCP que se anuncia.
const Version = "2025-06-18"

// Servidor atiende MCP sobre un par de flujos.
type Servidor struct {
	API     API
	Nombre  string
	Version string
	// Secretos se borran de cualquier texto que salga (errores incluidos).
	Secretos []string
}

type peticion struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type respuesta struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *errRPC         `json:"error,omitempty"`
}

type errRPC struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Servir lee peticiones de r hasta EOF o hasta que ctx se cancele, y escribe las
// respuestas en w. Cada petición se atiende en su sitio: un exec largo no
// bloquea un ping, y las respuestas se serializan con un cerrojo.
func (s *Servidor) Servir(ctx context.Context, r io.Reader, w io.Writer) error {
	var mu sync.Mutex
	var wg sync.WaitGroup
	enviar := func(v respuesta) {
		b, _ := json.Marshal(v)
		mu.Lock()
		defer mu.Unlock()
		w.Write(append(b, '\n'))
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		linea := strings.TrimSpace(sc.Text())
		if linea == "" {
			continue
		}
		var p peticion
		if err := json.Unmarshal([]byte(linea), &p); err != nil || p.JSONRPC != "2.0" {
			enviar(respuesta{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &errRPC{-32700, "invalid JSON-RPC message"}})
			continue
		}
		if len(p.ID) == 0 { // notificación: no se contesta
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, e := s.despachar(ctx, p)
			enviar(respuesta{JSONRPC: "2.0", ID: p.ID, Result: res, Error: e})
		}()
	}
	cancel()
	wg.Wait()
	return sc.Err()
}

func (s *Servidor) despachar(ctx context.Context, p peticion) (any, *errRPC) {
	switch p.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": Version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": s.Nombre, "version": s.Version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": herramientas}, nil
	case "tools/call":
		var c struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(p.Params, &c); err != nil || c.Name == "" {
			return nil, &errRPC{-32602, "invalid params"}
		}
		h, ok := manejadores[c.Name]
		if !ok {
			return nil, &errRPC{-32602, fmt.Sprintf("unknown tool %q", c.Name)}
		}
		texto, err := h(ctx, s.API, c.Arguments)
		if err != nil {
			return resultado(s.limpiar(err.Error()), true), nil
		}
		return resultado(s.limpiar(texto), false), nil
	}
	return nil, &errRPC{-32601, "method not found: " + p.Method}
}

func (s *Servidor) limpiar(t string) string {
	for _, sec := range s.Secretos {
		if sec != "" {
			t = strings.ReplaceAll(t, sec, "[redacted]")
		}
	}
	return t
}

func resultado(texto string, esError bool) map[string]any {
	m := map[string]any{"content": []map[string]any{{"type": "text", "text": texto}}}
	if esError {
		m["isError"] = true
	}
	return m
}
