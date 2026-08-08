package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// MEMORIA DE USO.
//
// Opcional y apagada por defecto. Cuando se activa, kindling apunta en un
// servicio MCP de memoria —el que el usuario haya enlazado— qué herramienta
// resolvió cada petición, y usa ese historial para ordenar mejor las búsquedas
// futuras.
//
// La idea es sencilla: si la última vez que alguien pidió "leer un fichero" la
// respuesta útil fue filesystem.read_text_file, esa debería aparecer primero la
// próxima vez, por encima de otras que también mencionan "fichero".
//
// Se apoya en un servicio existente en vez de guardar nada propio. kindling no
// tiene por qué convertirse en una base de datos, y quien ya usa engram o
// similar prefiere tener ahí todo su contexto.
type memory struct {
	gw      *Gateway
	service string // servicio MCP donde escribir y leer

	mu      sync.Mutex
	session string
	// hits cuenta, en esta ejecución, qué herramienta resolvió qué consulta.
	// Evita ir a la memoria remota en cada búsqueda.
	hits map[string]map[string]int // consulta normalizada -> herramienta -> veces
}

func newMemory(gw *Gateway, service string) *memory {
	if service == "" {
		return nil
	}
	return &memory{gw: gw, service: service, hits: map[string]map[string]int{}}
}

func normalize(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// Record anota que una herramienta resolvió una consulta.
//
// Nunca bloquea al que llama ni le devuelve error: si la memoria falla, la
// herramienta ya hizo su trabajo y el usuario no tiene por qué enterarse.
func (m *memory) Record(ctx context.Context, query, tool string) {
	if m == nil || query == "" {
		return
	}
	q := normalize(query)

	m.mu.Lock()
	if m.hits[q] == nil {
		m.hits[q] = map[string]int{}
	}
	m.hits[q][tool]++
	m.mu.Unlock()

	go func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if err := m.save(bg, q, tool); err != nil {
			log.Printf("memoria: no pude anotar %q -> %s: %v", trunc(q, 60), tool, err)
		}
	}()
}

// Rank reordena una lista de herramientas poniendo delante las que ya
// resolvieron consultas parecidas.
func (m *memory) Rank(query string, tools []Tool) []Tool {
	if m == nil || len(tools) < 2 {
		return tools
	}
	q := normalize(query)

	m.mu.Lock()
	scores := map[string]int{}
	for past, byTool := range m.hits {
		// Coincidencia laxa: basta con que una consulta pasada comparta términos
		// con la actual. No hace falta más finura para ordenar cuatro resultados.
		if !shareTerms(past, q) {
			continue
		}
		for tool, n := range byTool {
			scores[tool] += n
		}
	}
	m.mu.Unlock()

	if len(scores) == 0 {
		return tools
	}
	out := append([]Tool(nil), tools...)
	sort.SliceStable(out, func(i, j int) bool {
		return scores[out[i].Qualified] > scores[out[j].Qualified]
	})
	return out
}

func shareTerms(a, b string) bool {
	fa := strings.Fields(a)
	for _, w := range strings.Fields(b) {
		if len(w) < 4 {
			continue // "de", "un", "el" no dicen nada
		}
		for _, x := range fa {
			if x == w {
				return true
			}
		}
	}
	return false
}

// save escribe la observación en el servicio de memoria enlazado.
func (m *memory) save(ctx context.Context, query, tool string) error {
	l := m.gw.linkFor(ctx, m.service)
	if l == nil {
		return fmt.Errorf("el servicio de memoria %q no está enlazado", m.service)
	}

	m.mu.Lock()
	sid := m.session
	m.mu.Unlock()
	if sid == "" {
		newSid, err := mcpInitAt(ctx, l.URL)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.session, sid = newSid, newSid
		m.mu.Unlock()
	}

	// Se busca una herramienta de escritura en el catálogo del servicio, en vez
	// de suponer la API de engram: así vale cualquier servidor de memoria.
	write := pickTool(l.Tools, []string{"save", "remember", "store", "add", "create", "guardar"})
	if write == "" {
		return fmt.Errorf("%s no expone ninguna herramienta de escritura", m.service)
	}

	content := fmt.Sprintf("kindling: la petición %q se resolvió con la herramienta %s", query, tool)
	args, _ := json.Marshal(map[string]any{
		"content":     content,
		"text":        content,
		"observation": content,
		"tags":        []string{"kindling", "uso-de-herramientas"},
	})
	_, err := mcpCallAt(ctx, l.URL, sid, fmt.Sprintf(
		`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
		nextRPCID(), write, args))
	return err
}

// pickTool elige la primera herramienta cuyo nombre contenga alguno de los
// verbos buscados. Adivinar por nombre es frágil, pero lo alternativo sería
// atar kindling a la API de un servidor concreto.
func pickTool(tools []api.ToolSpec, verbs []string) string {
	for _, v := range verbs {
		for _, t := range tools {
			if strings.Contains(strings.ToLower(t.Name), v) {
				return t.Name
			}
		}
	}
	return ""
}
