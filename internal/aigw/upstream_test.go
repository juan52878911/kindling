package aigw

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// inestable es un upstream de mentira: mientras caido, corta cada conexión
// sin contestar y su /health da 503; si no, contesta 200. Cuenta las
// peticiones que no son la sonda.
type inestable struct {
	srv     *httptest.Server
	caido   atomic.Bool
	calls   atomic.Int64
	sondas  atomic.Int64
	entered chan struct{}
	block   chan struct{}
}

func newInestable(t *testing.T) *inestable {
	f := &inestable{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			f.sondas.Add(1)
			if f.caido.Load() {
				w.WriteHeader(503)
			}
			return
		}
		f.calls.Add(1)
		if f.caido.Load() {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		if f.block != nil {
			f.entered <- struct{}{}
			<-f.block
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func upstreamBackend(url string) *Backend {
	return &Backend{Name: "gpu", Upstream: url, Contract: ContractRaw,
		Routes: []Route{{"POST", "/v1/decide"}}, MaxInflight: 2,
		// Sin every: con el circuito cerrado no hay sondas y los fallos que
		// cuenta el test son solo los de las peticiones.
		Health: Health{Path: "/health", Timeout: time.Second}}
}

// Un upstream caído: tres fallos seguidos abren el circuito y desde ahí cada
// petición recibe 503 con Retry-After al instante, sin tocar el upstream;
// cuando una sonda vuelve a pasar, el circuito se cierra solo.
func TestUpstreamCircuito(t *testing.T) {
	defer func(d time.Duration) { sondaAbierto = d }(sondaAbierto)
	sondaAbierto = 20 * time.Millisecond
	f := newInestable(t)
	f.caido.Store(true)
	g, h, _ := pasarela(t, f.srv, upstreamBackend(f.srv.URL))
	pedir := func() *httptest.ResponseRecorder {
		return peticion(h, "POST", "/v1/m/gpu/v1/decide", tokPrincipal, []byte(`{}`), nil)
	}
	for i := 0; i < 3; i++ {
		if rec := pedir(); rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("failure %d: %d %v", i, rec.Code, rec.Header())
		}
	}
	if f.calls.Load() != 3 {
		t.Fatalf("upstream calls = %d, want 3", f.calls.Load())
	}
	t0 := time.Now()
	rec := pedir()
	if d := time.Since(t0); d > 5*time.Millisecond {
		t.Errorf("open circuit took %v to answer", d)
	}
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" || f.calls.Load() != 3 {
		t.Fatalf("open circuit: %d %v, upstream calls %d", rec.Code, rec.Header(), f.calls.Load())
	}
	if !g.upstreamDe(upstreamBackend(f.srv.URL)).abierto() {
		t.Fatal("circuit not open")
	}

	// Vuelve: las peticiones (que siguen recibiendo 503) lanzan la sonda, y
	// cuando pasa el circuito se cierra.
	f.caido.Store(false)
	limite := time.Now().Add(5 * time.Second)
	for {
		rec := pedir()
		if rec.Code == 200 {
			break
		}
		if time.Now().After(limite) {
			t.Fatalf("circuit never closed: %d, probes %d", rec.Code, f.sondas.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if f.sondas.Load() == 0 {
		t.Fatal("closed without a probe")
	}
}

// max_inflight es un semáforo con cola acotada: con 2 en vuelo caben 4×2 en
// cola, y la siguiente recibe 503 al momento en vez de esperar sin fin.
func TestUpstreamSemaforoYCola(t *testing.T) {
	f := newInestable(t)
	f.entered, f.block = make(chan struct{}, 16), make(chan struct{})
	b := upstreamBackend(f.srv.URL)
	b.Health = Health{} // sin sondas: solo el semáforo
	g, h, _ := pasarela(t, f.srv, b)
	var wg sync.WaitGroup
	codes := make(chan int, 16)
	lanzar := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- peticion(h, "POST", "/v1/m/gpu/v1/decide", tokPrincipal, []byte(`{}`), nil).Code
		}()
	}
	lanzar()
	lanzar()
	<-f.entered
	<-f.entered
	u := g.upstreamDe(b)
	for i := 0; i < 8; i++ {
		lanzar()
	}
	limite := time.Now().Add(5 * time.Second)
	for u.enCola() != 8 {
		if time.Now().After(limite) {
			t.Fatalf("queued = %d, want 8", u.enCola())
		}
		time.Sleep(time.Millisecond)
	}
	if rec := peticion(h, "POST", "/v1/m/gpu/v1/decide", tokPrincipal, []byte(`{}`), nil); rec.Code != 503 {
		t.Fatalf("over the queue: %d", rec.Code)
	}
	close(f.block)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Fatalf("queued request: %d", c)
		}
	}
	if f.calls.Load() != 10 {
		t.Fatalf("upstream calls %d, want 10", f.calls.Load())
	}
}

// Una petición en cola sale cuando el cliente se va, sin ocupar hueco.
func TestUpstreamColaRespetaContexto(t *testing.T) {
	b := upstreamBackend("http://127.0.0.1:1")
	b.MaxInflight, b.Health = 1, Health{}
	u, err := newUpstream(b, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	soltar, err := u.adquirir(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := u.adquirir(ctx); err == nil {
		t.Fatal("acquired over max_inflight")
	}
	if u.enCola() != 0 {
		t.Fatalf("queue not released: %d", u.enCola())
	}
	soltar()
	if s, err := u.adquirir(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		s()
	}
}
