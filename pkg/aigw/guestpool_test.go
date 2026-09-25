package aigw

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingGuest es un kling-chispa de mentira que cuenta las conexiones TCP
// que acepta: es lo que agotaba los puertos efímeros del host.
type countingGuest struct {
	srv   *httptest.Server
	conns atomic.Int64
	calls atomic.Int64
	// killSecond, si está, corta SIN responder la segunda petición que llega
	// por una misma conexión, una sola vez: es la conexión que el host creía
	// viva y que un congelar/despertar dejó muerta.
	killSecond atomic.Bool
}

func newCountingGuest(t *testing.T) *countingGuest {
	f := &countingGuest{}
	type connKey struct{}
	var mu sync.Mutex
	perConn := map[net.Conn]int{}
	f.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		c := r.Context().Value(connKey{}).(net.Conn)
		mu.Lock()
		perConn[c]++
		n := perConn[c]
		mu.Unlock()
		if n == 2 && f.killSecond.CompareAndSwap(true, false) {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			conn.Close()
			return
		}
		var req chispaGuestRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chispaGuestResponse{Label: "fix", Prob: 0.9, Threshold: 0.5})
	}))
	f.srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		return context.WithValue(ctx, connKey{}, c)
	}
	f.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			f.conns.Add(1)
		}
	}
	f.srv.Start()
	t.Cleanup(f.srv.Close)
	return f
}

func guestGateway(t *testing.T, addr string) *Gateway {
	t.Helper()
	cfg := microVMConfig(t, &ModelConfig{Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-commits"}, &TaskConfig{Chispa: "commits"})
	g, err := New(Options{Config: cfg, Replicas: &fakeReplicas{addr: addr}})
	if err != nil {
		t.Fatal(err)
	}
	g.deploy = fakeDeployLookup{"chispa-commits": {Labels: []string{"fix", "feat", "docs"}}}
	return g
}

// classifyLoad hace n clasificaciones con conc a la vez y devuelve cuánto
// tardaron.
func classifyLoad(t testing.TB, g *Gateway, n, conc int) time.Duration {
	var wg sync.WaitGroup
	var next atomic.Int64
	var fails atomic.Int64
	t0 := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= int64(n) {
				if _, err := g.Classify(context.Background(), "classify", ClassifyRequest{Task: "kind", Text: "fix the bug"}); err != nil {
					fails.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if f := fails.Load(); f > 0 {
		t.Fatalf("%d of %d classifications failed", f, n)
	}
	return time.Since(t0)
}

// TestGuestReusesConnections: miles de decisiones contra una réplica no
// pueden abrir una conexión TCP cada una. Antes (DisableKeepAlives) abrían
// una por decisión y un conjunto de CI entero agotaba los puertos efímeros
// del Mac. Ahora las conexiones se reutilizan y no pasan del tope.
func TestGuestReusesConnections(t *testing.T) {
	fg := newCountingGuest(t)
	g := guestGateway(t, strings.TrimPrefix(fg.srv.URL, "http://"))
	const n, conc = 2000, 8
	d := classifyLoad(t, g, n, conc)
	t.Logf("%d classifications, %d concurrent: %v (%.0f/s), %d TCP connections",
		n, conc, d, float64(n)/d.Seconds(), fg.conns.Load())
	if c := fg.conns.Load(); c > guestConnsPerReplica {
		t.Fatalf("%d classifications opened %d TCP connections; want at most %d (keep-alive)", n, c, guestConnsPerReplica)
	}
}

// TestGuestRetriesStaleConnection: una conexión reutilizada que el invitado
// ya no reconoce (congelado y despertado, o la IP es de otra réplica) se corta
// sin respuesta. La petición se repite una vez por una conexión nueva en vez
// de devolver un error al cliente.
func TestGuestRetriesStaleConnection(t *testing.T) {
	fg := newCountingGuest(t)
	g := guestGateway(t, strings.TrimPrefix(fg.srv.URL, "http://"))
	ctx := context.Background()
	if _, err := g.Classify(ctx, "classify", ClassifyRequest{Task: "kind", Text: "one"}); err != nil {
		t.Fatal(err)
	}
	fg.killSecond.Store(true)
	resp, err := g.Classify(ctx, "classify", ClassifyRequest{Task: "kind", Text: "two"})
	if err != nil {
		t.Fatalf("a dead pooled connection should be retried on a fresh one: %v", err)
	}
	if resp.Label != "fix" {
		t.Fatalf("got %+v", resp)
	}
	if c := fg.conns.Load(); c != 2 {
		t.Fatalf("want 2 connections (the dead one and its replacement), got %d", c)
	}
}

// TestGuestPoolForgetOnSleep: al dormir una réplica se olvidan sus
// conexiones; la siguiente petición abre una nueva.
func TestGuestPoolForgetOnSleep(t *testing.T) {
	fg := newCountingGuest(t)
	addr := strings.TrimPrefix(fg.srv.URL, "http://")
	g := guestGateway(t, addr)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := g.Classify(ctx, "classify", ClassifyRequest{Task: "kind", Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if c := fg.conns.Load(); c != 1 {
		t.Fatalf("want 1 reused connection, got %d", c)
	}
	g.guests.forget(addr)
	if g.guests.size() != 0 {
		t.Fatal("transport not forgotten")
	}
	if _, err := g.Classify(ctx, "classify", ClassifyRequest{Task: "kind", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	if c := fg.conns.Load(); c != 2 {
		t.Fatalf("want a fresh connection after sleep, got %d total", c)
	}
}

// BenchmarkGuestClassify mide el caudal contra una réplica falsa (go test
// -bench GuestClassify ./pkg/aigw).
func BenchmarkGuestClassify(b *testing.B) {
	fg := newCountingGuest(&testing.T{})
	defer fg.srv.Close()
	cfg := &Config{
		Models: map[string]*ModelConfig{"commits": {Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-commits"}},
		Tasks:  map[string]*TaskConfig{"kind": {Chispa: "commits"}},
	}
	if err := cfg.Validate(); err != nil {
		b.Fatal(err)
	}
	g, err := New(Options{Config: cfg, Replicas: &fakeReplicas{addr: strings.TrimPrefix(fg.srv.URL, "http://")}})
	if err != nil {
		b.Fatal(err)
	}
	g.deploy = fakeDeployLookup{"chispa-commits": {Labels: []string{"fix", "feat", "docs"}}}
	b.ResetTimer()
	d := classifyLoad(b, g, b.N, 8)
	b.ReportMetric(float64(b.N)/d.Seconds(), "req/s")
	b.ReportMetric(float64(fg.conns.Load()), "conns")
}
