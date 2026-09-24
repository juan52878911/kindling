package aigw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/jev"
	"github.com/juan52878911/kindling/pkg/jev/train"
)

// corpus es un conjunto sintético con cuatro clases fáciles de separar.
func corpus(n int, seed uint64) []jev.Example {
	vocab := map[string][]string{
		"bug":   {"crash", "panic", "null", "overflow", "segfault", "broken"},
		"feat":  {"add", "support", "new", "implement", "introduce", "option"},
		"docs":  {"readme", "typo", "documentation", "guide", "example", "wording"},
		"chore": {"bump", "deps", "release", "version", "lockfile", "cleanup"},
	}
	labels := []string{"bug", "chore", "docs", "feat"}
	s := seed
	next := func() uint64 { s = s*6364136223846793005 + 1442695040888963407; return s >> 33 }
	out := make([]jev.Example, n)
	for i := range out {
		l := labels[next()%4]
		var b strings.Builder
		for j := 0; j < 3; j++ {
			b.WriteString(vocab[l][next()%6] + " the parser ")
		}
		out[i] = jev.Example{Text: b.String(), Label: l}
	}
	return out
}

// trainedModel entrena (una vez por test) un modelo pequeño y lo guarda.
func trainedModel(t *testing.T) string {
	t.Helper()
	cfg := train.Config{Spec: jev.DefaultSpec(), Seed: 1}
	cfg.Spec.Buckets = 1 << 12
	res, err := train.Train(corpus(600, 1), corpus(200, 2), cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "m.jev")
	if err := res.Model.Save(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeLlama es un llama-server de mentira: contesta lo que diga answer y
// guarda lo que le preguntan.
type fakeLlama struct {
	srv    *httptest.Server
	mu     sync.Mutex
	answer string
	status int
	bodies []map[string]any
	auth   []string
	calls  atomic.Int64
	// entered y block, si están, retienen cada petición hasta que se cierre
	// block (para tener peticiones en vuelo).
	entered chan struct{}
	block   chan struct{}
}

func newFakeLlama(t *testing.T) *fakeLlama {
	f := &fakeLlama{answer: "bug", status: 200}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.block != nil {
			f.entered <- struct{}{}
			<-f.block
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		ans, st := f.answer, f.status
		f.mu.Unlock()
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			for _, tok := range strings.Fields(ans) {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", tok)
				fl.Flush()
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": ans}}},
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLlama) set(answer string) {
	f.mu.Lock()
	f.answer = answer
	f.mu.Unlock()
}

func (f *fakeLlama) last() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil
	}
	return f.bodies[len(f.bodies)-1]
}

// fakeReplicas reparte siempre la misma réplica (el llama falso), o falla.
type fakeReplicas struct {
	addr     string
	fail     error
	acquired atomic.Int64
	released atomic.Int64
}

func (f *fakeReplicas) Acquire(ctx context.Context, snap string) (*Replica, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.acquired.Add(1)
	return &Replica{Addr: f.addr, Release: func() { f.released.Add(1) }, Drop: func() {}}, nil
}

// newTestGateway arma un gateway con un modelo JEV entrenado y el llama falso.
func newTestGateway(t *testing.T, mut func(*Config)) (*Gateway, *fakeLlama, *fakeReplicas) {
	t.Helper()
	ll := newFakeLlama(t)
	reps := &fakeReplicas{addr: strings.TrimPrefix(ll.srv.URL, "http://")}
	cfg := &Config{
		Models: map[string]*ModelConfig{
			"commits": {Kind: KindJEV, Path: trainedModel(t)},
			"smol":    {Kind: KindVON, Snapshot: "von-smol"},
		},
		Tasks: map[string]*TaskConfig{
			// Cascada forzada: aquí se prueba lo que hace, no la puerta que
			// la activa (eso es eval_test.go).
			"kind": {JEV: "commits", EscalateTo: "smol", EscalateForce: true},
		},
	}
	if mut != nil {
		mut(cfg)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Config: cfg, Replicas: reps, EvalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return g, ll, reps
}

// do hace una petición al handler.
func do(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		bs, _ := json.Marshal(b)
		rd = bytes.NewReader(bs)
	}
	req := httptest.NewRequest(method, path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return v
}
