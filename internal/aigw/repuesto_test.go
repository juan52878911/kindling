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
