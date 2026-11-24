package aigw

import (
	"context"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// vllmFalso es un vLLM de mentira: mientras cargando, ni /health ni nada
// contestan (corta la conexión, como un puerto aún sin servidor); /v1/models
// lista modelos; el resto cuenta y contesta 200.
type vllmFalso struct {
	srv      *httptest.Server
	cargando atomic.Bool
	modelos  atomic.Value // string: el id que sirve
	calls    atomic.Int64
	listados atomic.Int64
}

func newVLLMFalso(t *testing.T, modelo string) *vllmFalso {
	f := &vllmFalso{}
	f.modelos.Store(modelo)
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.cargando.Load() {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		switch r.URL.Path {
		case "/health":
			return
		case "/v1/models":
			f.listados.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"`+f.modelos.Load().(string)+`","object":"model"}]}`)
			return
		}
		f.calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[]}`)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func vllmBackend(url string) *Backend {
	return &Backend{Name: "qwen3-gpu", Upstream: url, Contract: ContractOpenAI,
		Routes:        []Route{{"POST", "/v1/chat/completions"}},
		UpstreamModel: "Qwen/Qwen3-4B", MaxInflight: 2,
		Health: Health{Path: "/health", Timeout: time.Second}}
}

// esperaHasta repite f hasta que devuelva true o pase el plazo.
func esperaHasta(t *testing.T, que string, f func() bool) {
	t.Helper()
	limite := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(limite) {
			t.Fatalf("timed out waiting for %s", que)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestUpstreamModeloAusenteAbreCircuito: la sonda al cargar mira que el
// upstream sirve upstream_model; si no, el circuito se abre ya (sin esperar a
// tres fallos) y ninguna petición llega a un servidor que la rechazaría. Al
// servirlo, la sonda lo cierra.
func TestUpstreamModeloAusenteAbreCircuito(t *testing.T) {
	defer func(d time.Duration) { sondaAbierto = d }(sondaAbierto)
	sondaAbierto = 20 * time.Millisecond
	f := newVLLMFalso(t, "Qwen/Qwen2.5-7B")
	b := vllmBackend(f.srv.URL)
	g, h, _ := pasarela(t, f.srv, b)
	esperaHasta(t, "the load probe", func() bool { return g.upstreamDe(b).abierto() })
	if f.listados.Load() == 0 {
		t.Fatal("open without listing the models")
	}
	rec := peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu"}`), nil)
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" || f.calls.Load() != 0 {
		t.Fatalf("model missing upstream: %d %v, calls %d", rec.Code, rec.Header(), f.calls.Load())
	}
	f.modelos.Store("Qwen/Qwen3-4B")
	esperaHasta(t, "the circuit to close", func() bool {
		return peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu"}`), nil).Code == 200
	})
}

// Si el upstream sirve el modelo, la sonda al cargar no cambia nada.
func TestUpstreamModeloPresenteNoAbre(t *testing.T) {
	f := newVLLMFalso(t, "Qwen/Qwen3-4B")
	b := vllmBackend(f.srv.URL)
	g, h, _ := pasarela(t, f.srv, b)
	esperaHasta(t, "the load probe", func() bool { return f.listados.Load() > 0 })
	if g.upstreamDe(b).abierto() {
		t.Fatal("circuit open with the model served")
	}
	if rec := peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu"}`), nil); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// TestHealthGrace503: durante health.grace un upstream que aún carga recibe
// 503 con Retry-After al instante, sin reenviarle nada y sin abrir el
// circuito; cuando la sonda pasa, sirve. Sin gracia, lo de siempre: las
// peticiones lo intentan y tres fallos abren el circuito.
func TestHealthGrace503(t *testing.T) {
	defer func(d time.Duration) { sondaAbierto = d }(sondaAbierto)
	sondaAbierto = 20 * time.Millisecond
	f := newVLLMFalso(t, "Qwen/Qwen3-4B")
	f.cargando.Store(true)
	b := vllmBackend(f.srv.URL)
	b.Health.Grace = time.Minute
	g, h, _ := pasarela(t, f.srv, b)
	for i := 0; i < 5; i++ {
		t0 := time.Now()
		rec := peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu"}`), nil)
		if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
			t.Fatalf("loading %d: %d %v", i, rec.Code, rec.Header())
		}
		if d := time.Since(t0); d > 500*time.Millisecond {
			t.Errorf("loading answer took %v", d)
		}
	}
	if f.calls.Load() != 0 {
		t.Fatalf("requests reached a loading upstream: %d", f.calls.Load())
	}
	if g.upstreamDe(b).abierto() {
		t.Fatal("grace opened the circuit")
	}
	f.cargando.Store(false)
	esperaHasta(t, "the upstream to serve", func() bool {
		return peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu"}`), nil).Code == 200
	})
}

// Pasada la gracia sin que el upstream conteste, se vuelve a lo de siempre.
func TestHealthGraceVence(t *testing.T) {
	b := vllmBackend("http://127.0.0.1:1")
	b.Health.Grace = time.Minute
	u, err := newUpstream(b, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ahora := time.Now()
	u.ahora = func() time.Time { return ahora }
	u.creado = ahora
	if _, err := u.adquirir(context.Background()); !errors.Is(err, errCargando) {
		t.Fatalf("within grace: %v", err)
	}
	ahora = ahora.Add(2 * time.Minute)
	soltar, err := u.adquirir(context.Background())
	if err != nil {
		t.Fatalf("after grace: %v", err)
	}
	soltar()
	for i := 0; i < fallosParaAbrir; i++ {
		u.resultado(sondaFallo)
	}
	if !u.abierto() {
		t.Fatal("failures after grace did not open the circuit")
	}
}

// ca_file: el upstream https con un certificado propio se verifica con ese PEM
// y no con las raíces del sistema.
func TestUpstreamCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(ca, pemCert, 0o644); err != nil {
		t.Fatal(err)
	}
	b := upstreamBackend(srv.URL)
	b.Health = Health{}
	_, h, _ := pasarela(t, srv, b)
	if rec := peticion(h, "POST", "/v1/m/gpu/v1/decide", tokPrincipal, []byte(`{}`), nil); rec.Code == 200 {
		t.Fatal("unknown certificate accepted without ca_file")
	}
	b2 := upstreamBackend(srv.URL)
	b2.Health, b2.CAFile = Health{}, ca
	_, h2, _ := pasarela(t, srv, b2)
	if rec := peticion(h2, "POST", "/v1/m/gpu/v1/decide", tokPrincipal, []byte(`{}`), nil); rec.Code != 200 {
		t.Fatalf("with ca_file: %d %s", rec.Code, rec.Body)
	}
}

// ca_file en el registro: relativo al ai.json, tiene que ser un PEM con algún
// certificado y solo vale con un upstream https.
func TestCAFileValida(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	srv.Close()
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pemCert, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "basura.pem"), []byte("no soy un PEM"), 0o644); err != nil {
		t.Fatal(err)
	}
	carga := func(m string) (*Config, error) {
		p := filepath.Join(dir, "ai.json")
		if err := os.WriteFile(p, []byte(`{"models":{"m":`+m+`}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadConfig(p)
	}
	c, err := carga(`{"kind":"http","upstream":"https://gpu:8443","contract":"openai","ca_file":"ca.pem"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Backend("m").CAFile; got != filepath.Join(dir, "ca.pem") {
		t.Fatalf("ca_file = %q", got)
	}
	for _, m := range []string{
		`{"kind":"http","upstream":"https://gpu:8443","contract":"openai","ca_file":"basura.pem"}`,
		`{"kind":"http","upstream":"https://gpu:8443","contract":"openai","ca_file":"falta.pem"}`,
		`{"kind":"http","upstream":"http://gpu:8000","contract":"openai","ca_file":"ca.pem"}`,
		`{"kind":"http","snapshot":"s","contract":"openai","ca_file":"ca.pem"}`,
		`{"kind":"von","snapshot":"s","ca_file":"ca.pem"}`,
	} {
		if _, err := carga(m); err == nil {
			t.Errorf("accepted %s", m)
		}
	}
}

// health.grace: una duración acotada, solo de un upstream.
func TestHealthGraceValida(t *testing.T) {
	for _, m := range []string{
		`{"kind":"http","upstream":"http://h:1","contract":"openai","health":{"grace":"mucho"}}`,
		`{"kind":"http","upstream":"http://h:1","contract":"openai","health":{"grace":"-1s"}}`,
		`{"kind":"http","upstream":"http://h:1","contract":"openai","health":{"grace":"48h"}}`,
		`{"kind":"http","snapshot":"s","contract":"openai","health":{"grace":"5m"}}`,
	} {
		if _, err := cargaBloque(m, ""); err == nil {
			t.Errorf("accepted %s", m)
		}
	}
	c, err := cargaBloque(`{"kind":"http","upstream":"http://h:1","contract":"openai","health":{"path":"/health","grace":"10m"}}`, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Backend("m").Health.Grace; got != 10*time.Minute {
		t.Fatalf("grace = %v", got)
	}
}
