package aigw

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// C9-08: el gateway calienta cada repuesto con la petición del contrato
// (openai: una respuesta corta; embeddings: un vector), con la credencial del
// bloque si la lleva. Un contrato sin calentamiento conocido se queda en la
// sonda. SpareFor da el spare del bloque.
func TestRepuestoSeCalientaPorContrato(t *testing.T) {
	var mu sync.Mutex
	var vistas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		vistas = append(vistas, r.Method+" "+r.URL.Path+" "+r.Header.Get("X-Key")+" "+string(b))
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	key := ficheroSecreto(t, dir, "k", "clave\n", 0o600)
	p := filepath.Join(dir, "ai.json")
	escribirRegistro(t, p, map[string]any{
		"chat":   map[string]any{"kind": "http", "contract": "openai", "snapshot": "chat-gold", "spare": 1, "auth": map[string]any{"header": "X-Key", "file": key}},
		"emb":    map[string]any{"kind": "http", "contract": "openai-embeddings", "snapshot": "emb-gold"},
		"decide": map[string]any{"kind": "http", "contract": "chispa-decide", "snapshot": "dec-gold"},
	}, nil)
	g, err := New(Options{ConfigPath: p, Replicas: &fakeReplicas{}})
	if err != nil {
		t.Fatal(err)
	}
	if g.sched.SpareFor == nil || g.sched.WarmSpare == nil || g.sched.AfterRestore == nil {
		t.Fatal("ganchos de la ronda 9 sin cablear")
	}
	if n := g.sched.SpareFor("chat-gold"); n != 1 {
		t.Errorf("SpareFor(chat-gold) = %d; quería 1", n)
	}
	if n := g.sched.SpareFor("emb-gold"); n != 0 {
		t.Errorf("SpareFor(emb-gold) = %d; quería 0", n)
	}
	addr := strings.TrimPrefix(srv.URL, "http://")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, snap := range []string{"chat-gold", "emb-gold", "dec-gold"} {
		if err := g.sched.WarmSpare(ctx, snap, addr); err != nil {
			t.Fatalf("WarmSpare(%s): %v", snap, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(vistas) != 2 {
		t.Fatalf("peticiones de calentamiento %v; quería chat y embeddings", vistas)
	}
	if !strings.HasPrefix(vistas[0], "POST /v1/chat/completions clave ") || !strings.Contains(vistas[0], `"max_tokens":8`) {
		t.Errorf("calentamiento openai = %q", vistas[0])
	}
	if !strings.HasPrefix(vistas[1], "POST /v1/embeddings  ") {
		t.Errorf("calentamiento embeddings = %q", vistas[1])
	}
}

// Con max_replicas 0 (la del gateway), spare no puede pasar del tope de
// réplicas del gateway: un repuesto es una réplica, y Validate solo lo acota
// cuando el bloque pone su max_replicas.
func TestSpareSeAcotaAlTopeDelGateway(t *testing.T) {
	cfg := &Config{
		Models: map[string]*ModelConfig{
			"sin-tope": {Kind: KindHTTP, Contract: ContractOpenAI, Snapshot: "llm-a", Port: 8080, Spare: 5},
			"con-tope": {Kind: KindHTTP, Contract: ContractOpenAI, Snapshot: "llm-b", Port: 8080, Spare: 3, MaxReplicas: 4},
			"smol":     {Kind: KindVON, Snapshot: "von-smol"},
		},
		Tasks: map[string]*TaskConfig{},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Config: cfg, Replicas: &fakeReplicas{}, MaxReplicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.sched.SpareFor("llm-a"); got != 2 {
		t.Errorf("spare 5 con max_replicas 0 y el gateway a 2: SpareFor = %d, quería 2", got)
	}
	if got := g.sched.SpareFor("llm-b"); got != 3 {
		t.Errorf("spare 3 con max_replicas 4: SpareFor = %d, quería 3", got)
	}
}
