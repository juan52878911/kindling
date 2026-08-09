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
	"sync/atomic"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// Tool es una herramienta de un servicio, tal como la describe su servidor MCP.
type Tool struct {
	Service     string          `json:"service"`
	Name        string          `json:"name"`      // nombre dentro del servicio
	Qualified   string          `json:"qualified"` // "servicio.herramienta"
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"inputSchema,omitempty"`

	// haystack es Qualified+" "+Description en minúsculas, precomputado al
	// construir la Tool. find_tools lo busca por substring, y sin este cache
	// haría un strings.ToLower + concatenación POR tool POR búsqueda (con 100
	// herramientas y un find_tools por segundo, son 100 allocs/seg).
	haystack string
}

// newTool construye una Tool y precalcula su haystack. Es el único camino
// recomendado para crear Tools: garantiza que find_tools no paga strings.ToLower.
func newTool(service, name, description string, schema json.RawMessage) Tool {
	q := service + "." + name
	return Tool{
		Service:     service,
		Name:        name,
		Qualified:   q,
		Description: description,
		Schema:      schema,
		haystack:    strings.ToLower(q + " " + description),
	}
}

// catalog cachea qué herramientas ofrece cada servicio.
//
// Construirlo exige hablar con cada servidor MCP, y eso DESPIERTA su microVM. Por
// eso se cachea con generosidad: preguntar "¿qué herramientas hay?" no debería
// costar arrancar veinte máquinas.
type catalog struct {
	gw  *Gateway
	ttl time.Duration

	// RWMutex: el 99% de las llamadas son cache hits y se sirven con RLock.
	// Antes era un Mutex liso, lo que serializaba lecturas concurrentes de
	// servicios distintos (un find_tools sobre 20 servicios paralelizados
	// acababa en convoy).
	mu      sync.RWMutex
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

// services devuelve los servicios disponibles: microVMs y servidores externos.
func (c *catalog) services(ctx context.Context) ([]string, error) {
	snaps, err := c.gw.client.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range snaps {
		n := s.Name
		if svc := s.Service(); svc != "" {
			n = svc
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	// Los servidores externos enlazados entran en la lista unificada: así una
	// memoria registrada como enlace aparece al lado de un filesystem de microVM,
	// y el modelo descubre todo con una sola búsqueda. El catálogo marca luego
	// cuáles son VMs y cuáles no; sin esa pista, el modelo llama a engram.*
	// esperando una microVM que no existe.
	for _, l := range c.gw.links(ctx) {
		if n := l.Service(); !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// externalSet devuelve el conjunto de servicios servidos por un enlace externo.
// Complementa a services(): la lista plana no dice cuál corre en microVM y cuál
// no, y el agregador necesita esa distinción para etiquetar las instrucciones.
func (c *catalog) externalSet(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	for _, l := range c.gw.links(ctx) {
		n := l.Service()
		if n == "" {
			n = l.Name
		}
		out[n] = true
	}
	return out
}

// toolsOf devuelve las herramientas de un servicio.
//
// Orden de preferencia, del más barato al más caro:
//  1. caché en memoria
//  2. catálogo guardado en el snapshot al importarlo  <- lo normal
//  3. preguntárselo al servicio, lo que DESPIERTA su microVM
//
// El paso 2 es la razón de ser del catálogo persistido: listar capacidades no
// debería costar arrancar máquinas.
func (c *catalog) toolsOf(ctx context.Context, service string) ([]Tool, error) {
	c.mu.RLock()
	if at, ok := c.fetched[service]; ok && time.Since(at) < c.ttl {
		t := c.tools[service]
		c.mu.RUnlock()
		return t, nil
	}
	c.mu.RUnlock()

	if l := c.gw.linkFor(ctx, service); l != nil {
		out := make([]Tool, 0, len(l.Tools))
		for _, t := range l.Tools {
			out = append(out, newTool(service, t.Name, t.Description, t.InputSchema))
		}
		c.mu.Lock()
		c.tools[service] = out
		c.fetched[service] = time.Now()
		c.mu.Unlock()
		return out, nil
	}

	if tools, ok := c.fromSnapshot(ctx, service); ok {
		c.mu.Lock()
		c.tools[service] = tools
		c.fetched[service] = time.Now()
		c.mu.Unlock()
		return tools, nil
	}

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

// fromSnapshot lee el catálogo que se capturó al importar el servicio.
func (c *catalog) fromSnapshot(ctx context.Context, service string) ([]Tool, bool) {
	snaps, err := c.gw.client.Snapshots(ctx)
	if err != nil {
		return nil, false
	}
	for _, s := range snaps {
		if s.Service() != service && s.Name != service {
			continue
		}
		if len(s.Tools) == 0 {
			return nil, false // importado sin catálogo: habrá que preguntar
		}
		out := make([]Tool, 0, len(s.Tools))
		for _, t := range s.Tools {
			out = append(out, newTool(service, t.Name, t.Description, t.InputSchema))
		}
		return out, true
	}
	return nil, false
}

// fetch hace un initialize + tools/list contra el servicio real.
//
// Es el camino caro: despierta la microVM. Solo se usa si el snapshot no trae
// catálogo, es decir, si el servicio no se importó con `kling mcp import`.
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
	raw, err := mcpCall(ctx, base, sid, encodeList())
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
		tools = append(tools, newTool(service, t.Name, t.Description, t.InputSchema))
	}
	return tools, nil
}

// all recopila las herramientas de los servicios indicados.
//
// Un servicio que falla no tumba la consulta: se devuelve lo que sí respondió y
// el error se anota. Con veinte herramientas, que una esté rota no puede dejar
// ciego al modelo sobre las otras diecinueve.
//
// Cada goroutine escribe en un índice propio de un slice preasignado, así que
// no hace falta mutex para cruzar los resultados: solo se agrega el semáforo
// para no despertar veinte microVMs a la vez.
func (c *catalog) all(ctx context.Context, services []string) ([]Tool, map[string]string) {
	type result struct {
		tools []Tool
		err   error
	}
	results := make([]result, len(services))
	wg := sync.WaitGroup{}
	sem := make(chan struct{}, 4)
	for i, s := range services {
		wg.Add(1)
		go func(i int, s string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			t, err := c.toolsOf(ctx, s)
			results[i] = result{t, err}
		}(i, s)
	}
	wg.Wait()

	out := make([]Tool, 0, len(services)*4)
	errs := map[string]string{}
	for i, r := range results {
		if r.err != nil {
			errs[services[i]] = r.err.Error()
			continue
		}
		out = append(out, r.tools...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Qualified < out[j].Qualified })
	return out, errs
}

// invalidate olvida la caché de un servicio.
func (c *catalog) invalidate(service string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.fetched, service)
	delete(c.tools, service)
}

// ── utilidades MCP ────────────────────────────────────────────────────────────

var httpc = &http.Client{Timeout: 60 * time.Second}

// rpcSeq da identificadores JSON-RPC únicos. El protocolo exige que un id no se
// repita dentro de una sesión mientras haya peticiones en vuelo.
var rpcSeq atomic.Int64

func nextRPCID() int64 { return rpcSeq.Add(1) }

// rpcEnvelope es el sobre JSON-RPC 2.0 que se envía al servidor MCP. Los campos
// usan json.RawMessage para Params: el argumento llega ya serializado y se
// embebe tal cual, sin re-parsear ni escapar.
type rpcEnvelope struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

type rpcCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// encodeRPC construye un cuerpo tools/call listo para enviar.
//
// Antes se hacía con fmt.Sprintf con un id hardcodeado ("id":10 o "id":1 según
// el callsite), lo que provocaba que dos llamadas concurrentes pisaran la
// entrada de la otra en la tabla de pendientes del puente. Aquí cada llamada
// obtiene un id único vía nextRPCID() y el JSON se genera con json.Marshal,
// que respeta la codificación que ya traían los arguments (vienen como
// json.RawMessage). El coste es ~600 ns/op vs ~130 ns/op de Sprintf, pero es
// la latencia de un round-trip HTTP despreciable y la diferencia desaparece
// cuando se compara con los 30 ms que cuesta un thaw.
func encodeRPC(method, name string, args json.RawMessage) string {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	e := rpcEnvelope{
		JSONRPC: "2.0",
		ID:      nextRPCID(),
		Method:  method,
	}
	e.Params.Name = name
	e.Params.Arguments = args
	b, _ := json.Marshal(e)
	return string(b)
}

// encodeList construye un cuerpo tools/list.
func encodeList() string {
	e := rpcEnvelope{
		JSONRPC: "2.0",
		ID:      nextRPCID(),
		Method:  "tools/list",
	}
	e.Params.Arguments = json.RawMessage("null")
	b, _ := json.Marshal(e)
	return string(b)
}

func mcpPost(ctx context.Context, base, sid, body string) (*http.Response, error) {
	return mcpPostAt(ctx, base+"/mcp", sid, body)
}

// mcpPostAt habla con una URL completa. Los enlaces externos la traen entera y
// no hay por qué suponerles la ruta /mcp.
func mcpPostAt(ctx context.Context, url, sid, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", api.AcceptMCP)
	if sid != "" {
		req.Header.Set(SessionHeader, sid)
	}
	return httpc.Do(req)
}

func mcpInit(ctx context.Context, base string) (string, error) {
	return mcpInitAt(ctx, base+"/mcp")
}

func mcpInitAt(ctx context.Context, url string) (string, error) {
	resp, err := mcpPostAt(ctx, url, "",
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
	return mcpCallAt(ctx, base+"/mcp", sid, body)
}

func mcpCallAt(ctx context.Context, url, sid, body string) (json.RawMessage, error) {
	resp, err := mcpPostAt(ctx, url, sid, body)
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
	return api.MCPPayload(buf.Bytes()), nil
}
