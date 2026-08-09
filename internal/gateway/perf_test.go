package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/api"
)

// ── encodeRPC ──────────────────────────────────────────────────────────────

func TestEncodeRPCCall(t *testing.T) {
	args := json.RawMessage(`{"path":"/etc/hostname"}`)
	body := encodeRPC("tools/call", "read_file", args)

	var env struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int64  `json:"id"`
		Method  string `json:"method"`
		Params  struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("JSON inválido: %v\n%s", err, body)
	}
	if env.JSONRPC != "2.0" || env.Method != "tools/call" {
		t.Fatalf("sobre mal formado: %s", body)
	}
	if env.Params.Name != "read_file" {
		t.Fatalf("name=%q", env.Params.Name)
	}
	if string(env.Params.Arguments) != `{"path":"/etc/hostname"}` {
		t.Fatalf("arguments=%s", env.Params.Arguments)
	}
	if env.ID == 0 {
		t.Fatalf("id no asignado: %s", body)
	}
}

func TestEncodeRPCList(t *testing.T) {
	body := encodeList()
	if !strings.Contains(body, `"method":"tools/list"`) {
		t.Fatalf("no contiene tools/list: %s", body)
	}
	if !strings.Contains(body, `"jsonrpc":"2.0"`) {
		t.Fatalf("no contiene jsonrpc: %s", body)
	}
}

func TestEncodeRPCEmptyArgs(t *testing.T) {
	body := encodeRPC("tools/call", "list", nil)
	if !strings.Contains(body, `"arguments":{}`) {
		t.Fatalf("args vacíos deberían serializarse como {}: %s", body)
	}
}

// ── haystack precomputado ─────────────────────────────────────────────────

func TestNewToolHaystack(t *testing.T) {
	tool := newTool("files", "read_text", "Lee el contenido de un fichero", json.RawMessage(`{}`))
	want := "files.read_text lee el contenido de un fichero"
	if tool.haystack != want {
		t.Fatalf("haystack=%q, want=%q", tool.haystack, want)
	}
}

// ── Rank con índices ──────────────────────────────────────────────────────

func TestRankNoMemory(t *testing.T) {
	var m *memory
	if got := m.Rank("hola", nil); got != nil {
		t.Fatalf("Rank(nil) = %v, want nil", got)
	}
	if got := m.Rank("hola", []Tool{{Qualified: "x"}}); got != nil {
		t.Fatalf("Rank(1 elem) = %v, want nil", got)
	}
}

func TestRankIndices(t *testing.T) {
	m := &memory{hits: map[string]map[string]int{
		"leer fichero": {"files.read_text": 5},
	}}
	tools := []Tool{
		{Qualified: "other.tool"},
		{Qualified: "files.read_text"},
		{Qualified: "another.tool"},
	}
	idx := m.Rank("leer fichero", tools)
	if len(idx) != 3 {
		t.Fatalf("len=%d, want 3", len(idx))
	}
	if tools[idx[0]].Qualified != "files.read_text" {
		t.Fatalf("primera herramienta debería ser files.read_text, es %s",
			tools[idx[0]].Qualified)
	}
}

func TestRankNoMatch(t *testing.T) {
	m := &memory{hits: map[string]map[string]int{
		"escribir": {"files.write": 3},
	}}
	tools := []Tool{
		{Qualified: "files.read_text"},
		{Qualified: "files.write"},
	}
	idx := m.Rank("leer", tools)
	if idx != nil {
		t.Fatalf("sin match debería devolver nil, got %v", idx)
	}
}

// ── catalog.invalidate ────────────────────────────────────────────────────

func TestCatalogInvalidate(t *testing.T) {
	c := newCatalog(nil, time.Minute)
	c.tools["svc"] = []Tool{{Qualified: "svc.x"}}
	c.fetched["svc"] = time.Now()
	c.invalidate("svc")
	if _, ok := c.tools["svc"]; ok {
		t.Fatalf("invalidate no borró tools")
	}
	if _, ok := c.fetched["svc"]; ok {
		t.Fatalf("invalidate no borró fetched")
	}
}

// ── api.MCPPayload sigue intacto ──────────────────────────────────────────

func TestMCPPayloadUnchanged(t *testing.T) {
	raw := []byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":1,\"result\":42}\n\n")
	got := api.MCPPayload(raw)
	if string(got) != `{"jsonrpc":"2.0","id":1,"result":42}` {
		t.Fatalf("got=%s", got)
	}
}

// ── Benchmark: encodeRPC (json.Marshal) vs sprintf ────────────────────────
//
// Se conserva la versión con Sprintf para medir la ganancia: corre las dos
// con -benchmem y compara. Sin Sprintf, encodeRPC asigna un []byte por
// llamada; con Sprintf, 2-3 allocs (plantilla parseada + buffer + quoting).

func BenchmarkEncodeRPC(b *testing.B) {
	args := json.RawMessage(`{"path":"/etc/hostname","encoding":"utf-8"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = encodeRPC("tools/call", "read_file", args)
	}
}

func BenchmarkEncodeRPCSprintf(b *testing.B) {
	args := []byte(`{"path":"/etc/hostname","encoding":"utf-8"}`)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = fmt.Sprintf(
			`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`,
			nextRPCID(), "read_file", args)
	}
}

// ── Benchmark: haystack precomputado vs por-llamada ───────────────────────

func BenchmarkHaystackPrecomputed(b *testing.B) {
	tools := makeBenchTools(100)
	q := "leer fichero"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		n := 0
		for _, t := range tools {
			if strings.Contains(t.haystack, q) {
				n++
			}
		}
		_ = n
	}
}

func BenchmarkHaystackComputed(b *testing.B) {
	tools := makeBenchTools(100)
	q := "leer fichero"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		n := 0
		for _, t := range tools {
			hay := strings.ToLower(t.Qualified + " " + t.Description)
			if strings.Contains(hay, q) {
				n++
			}
		}
		_ = n
	}
}

func makeBenchTools(n int) []Tool {
	out := make([]Tool, n)
	for i := range out {
		out[i] = newTool(
			"svc",
			fmt.Sprintf("tool_%d", i),
			fmt.Sprintf("Description for tool %d that mentions fichero and similar words", i),
			json.RawMessage(`{}`),
		)
	}
	return out
}