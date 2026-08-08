package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tool es una herramienta de un servicio, tal como la describe su servidor MCP.
type Tool struct {
	Service     string          `json:"service"`
	Name        string          `json:"name"`      // nombre dentro del servicio
	Qualified   string          `json:"qualified"` // "servicio.herramienta"
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"inputSchema,omitempty"`
}

// catalog cachea qué herramientas ofrece cada servicio.
//
// Construirlo exige hablar con cada servidor MCP, y eso DESPIERTA su microVM. Por
// eso se cachea con generosidad: preguntar "¿qué herramientas hay?" no debería
// costar arrancar veinte máquinas.
type catalog struct {
	gw  *Gateway
	ttl time.Duration

	mu      sync.Mutex
	tools   map[string][]Tool // servicio -> herramientas
	fetched map[string]time.Time
}

func newCatalog(gw *Gateway, ttl time.Duration) *catalog {
	return &catalog{
		gw: gw, ttl: ttl,
		tools:   map[string][]Tool{},
		fetched: map[string]time.Time{},
	}
}

// services devuelve los nombres de servicio disponibles, de los snapshots.
func (c *catalog) services(ctx context.Context) ([]string, error) {
	snaps, err := c.gw.client.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		n := s.Name
		if svc := s.Service(); svc != "" {
			n = svc
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// toolsOf devuelve las herramientas de un servicio, cacheadas.
func (c *catalog) toolsOf(ctx context.Context, service string) ([]Tool, error) {
	c.mu.Lock()
	if at, ok := c.fetched[service]; ok && time.Since(at) < c.ttl {
		t := c.tools[service]
		c.mu.Unlock()
		return t, nil
	}
	c.mu.Unlock()

	tools, err := c.fetch(ctx, service)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.tools[service] = tools
	c.fetched[service] = time.Now()
	c.mu.Unlock()
	return tools, nil
}

// fetch hace un initialize + tools/list contra el servicio real.
func (c *catalog) fetch(ctx context.Context, service string) ([]Tool, error) {
	e, err := c.gw.ensure(ctx, service)
	if err != nil {
		return nil, err
	}
	base := "http://" + e.ip + ":" + fmt.Sprint(GuestPort)

	sid, err := mcpInit(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", service, err)
	}
	raw, err := mcpCall(ctx, base, sid, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", service, err)
	}

	var out struct {
		Result struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: respuesta ilegible: %w", service, err)
	}

	tools := make([]Tool, 0, len(out.Result.Tools))
	for _, t := range out.Result.Tools {
		tools = append(tools, Tool{
			Service: service, Name: t.Name,
			Qualified:   service + "." + t.Name,
			Description: t.Description,
			Schema:      t.InputSchema,
		})
	}
	return tools, nil
}

// all recopila las herramientas de los servicios indicados.
//
// Un servicio que falla no tumba la consulta: se devuelve lo que sí respondió y
// el error se anota. Con veinte herramientas, que una esté rota no puede dejar
// ciego al modelo sobre las otras diecinueve.
func (c *catalog) all(ctx context.Context, services []string) ([]Tool, map[string]string) {
	var (
		mu   sync.Mutex
		out  []Tool
		errs = map[string]string{}
		wg   sync.WaitGroup
		sem  = make(chan struct{}, 4) // no despertar veinte microVMs a la vez
	)
	for _, s := range services {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			t, err := c.toolsOf(ctx, s)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[s] = err.Error()
				return
			}
			out = append(out, t...)
		}(s)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Qualified < out[j].Qualified })
	return out, errs
}

// invalidate olvida la caché de un servicio.
func (c *catalog) invalidate(service string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.fetched, service)
}

// ── utilidades MCP ────────────────────────────────────────────────────────────

var httpc = &http.Client{Timeout: 60 * time.Second}

func mcpPost(ctx context.Context, base, sid, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if sid != "" {
		req.Header.Set(SessionHeader, sid)
	}
	return httpc.Do(req)
}

func mcpInit(ctx context.Context, base string) (string, error) {
	resp, err := mcpPost(ctx, base, "",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"kling-gateway","version":"1"}}}`)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("initialize: HTTP %s", resp.Status)
	}
	return resp.Header.Get(SessionHeader), nil
}

func mcpCall(ctx context.Context, base, sid, body string) (json.RawMessage, error) {
	resp, err := mcpPost(ctx, base, sid, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
