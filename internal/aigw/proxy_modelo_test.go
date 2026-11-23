package aigw

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// sumidero es un servidor de mentira que lee el cuerpo entero sin guardarlo y
// contesta un JSON fijo: lo que cuesta es solo el camino del gateway.
func sumidero(tb testing.TB) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	tb.Cleanup(srv.Close)
	return srv
}

// cuerpoOpenAI es una petición de chat con un mensaje de n bytes.
func cuerpoOpenAI(n int) []byte {
	return []byte(`{"model":"smol","messages":[{"role":"user","content":"` + strings.Repeat("a", n) + `"}]}`)
}

func benchHandler(b *testing.B, h http.Handler, path string, body []byte) {
	b.Helper()
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			b.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
}

// BenchmarkProxyOpenAI mide el proxy OpenAI de hoy: lee el cuerpo entero, lo
// decodifica a un mapa y lo vuelve a serializar, así que asigna en proporción
// al cuerpo.
func BenchmarkProxyOpenAI(b *testing.B) {
	for _, n := range []int{1 << 10, 512 << 10} {
		b.Run(tamano(n), func(b *testing.B) {
			srv := sumidero(b)
			g, err := New(Options{
				Config:   &Config{Models: map[string]*ModelConfig{"smol": {Kind: KindVON, Snapshot: "von-smol"}}},
				Replicas: &fakeReplicas{addr: strings.TrimPrefix(srv.URL, "http://")},
				EvalDir:  b.TempDir(),
			})
			if err != nil {
				b.Fatal(err)
			}
			benchHandler(b, g.Handler(""), "/v1/chat/completions", cuerpoOpenAI(n))
		})
	}
}

func tamano(n int) string { return strconv.Itoa(n>>10) + "KiB" }

// puertoReplicas reparte siempre la misma dirección y apunta el puerto que le
// piden: el del backend, no el 8000 de VON.
type puertoReplicas struct {
	addr    string
	mu      sync.Mutex
	puertos []int
}

func (p *puertoReplicas) Acquire(ctx context.Context, snap string) (*Replica, error) {
	return p.AcquirePort(ctx, snap, DefaultBackendPort)
}

func (p *puertoReplicas) AcquirePort(_ context.Context, _ string, port int) (*Replica, error) {
	p.mu.Lock()
	p.puertos = append(p.puertos, port)
	p.mu.Unlock()
	return &Replica{Addr: p.addr, Release: func() {}, Drop: func() {}}, nil
}

// conBackends sustituye el catálogo de backends del gateway: Config.Backends
// es de otra zona, y aquí se prueba el proxy con backends hechos a mano.
func conBackends(g *Gateway, bs ...*Backend) {
	g.guests.backendsDe = func(*Config) []*Backend { return bs }
}

const (
	tokPrincipal = "main-token-0123456789"
	tokAcme      = "acme-token-0123456789"
	tokOtro      = "otro-token-0123456789"
)

// pasarela arma un gateway con dos inquilinos, el catálogo bs y las réplicas
// apuntando a srv.
func pasarela(t *testing.T, srv *httptest.Server, bs ...*Backend) (*Gateway, http.Handler, *puertoReplicas) {
	t.Helper()
	g, _, _ := newTestGateway(t, func(c *Config) {
		c.Tenants = []TenantConfig{{Name: "acme", Token: tokAcme}, {Name: "otro", Token: tokOtro}}
	})
	reps := &puertoReplicas{addr: strings.TrimPrefix(srv.URL, "http://")}
	g.replicas = reps
	conBackends(g, bs...)
	return g, g.Handler(tokPrincipal), reps
}

func decideBackend() *Backend {
	return &Backend{Name: "x", Snapshot: "chispa-x", Port: 8090, Contract: ContractChispaDecide,
		Routes: []Route{{Method: "POST", Path: "/v1/decide"}, {Method: "GET", Path: "/v1/info"}}, Idle: IdleRemove}
}

// peticion hace una petición con cuerpo crudo y cabeceras.
func peticion(h http.Handler, method, path, token string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// grabador es un backend de mentira que guarda lo que recibe y contesta
// status y body tal cual.
type grabador struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	hdrs   []http.Header
	paths  []string
	status int
	body   []byte
}

func newGrabador(t *testing.T, status int, body string) *grabador {
	gr := &grabador{status: status, body: []byte(body)}
	gr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gr.mu.Lock()
		gr.bodies = append(gr.bodies, b)
		gr.hdrs = append(gr.hdrs, r.Header.Clone())
		gr.paths = append(gr.paths, r.Method+" "+r.URL.RequestURI())
		st, out := gr.status, gr.body
		gr.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_, _ = w.Write(out)
	}))
	t.Cleanup(gr.srv.Close)
	return gr
}

func (gr *grabador) n() int {
	gr.mu.Lock()
	defer gr.mu.Unlock()
	return len(gr.bodies)
}

func (gr *grabador) set(status int, body string) {
	gr.mu.Lock()
	gr.status, gr.body = status, []byte(body)
	gr.mu.Unlock()
}

// Un servidor de mentira con POST /v1/decide contesta idéntico, en bytes y en
// código, por /v1/m/x/v1/decide: el cuerpo no se analiza ni se re-serializa
// (chispa-server rechaza campos que no conoce), la réplica se pide con el
// puerto del backend y el X-Request-Id va y vuelve.
func TestProxyModeloPasaCuerpoIntacto(t *testing.T) {
	resp := "{\"decision\" : \"allow\",\n \"z\":[1, 2.50]}"
	gr := newGrabador(t, 201, resp)
	_, h, reps := pasarela(t, gr.srv, decideBackend())
	in := []byte("{ \"text\":\"hola\",  \"b\":1.0, \"a\":{} }\n")
	rec := peticion(h, "POST", "/v1/m/x/v1/decide", tokPrincipal, in, map[string]string{"X-Request-Id": "req-42"})
	if rec.Code != 201 || rec.Body.String() != resp {
		t.Fatalf("answer: %d %q", rec.Code, rec.Body)
	}
	if gr.n() != 1 || !bytes.Equal(gr.bodies[0], in) || gr.paths[0] != "POST /v1/decide" {
		t.Fatalf("backend got %v %q", gr.paths, gr.bodies)
	}
	if gr.hdrs[0].Get("X-Request-Id") != "req-42" || rec.Header().Get("X-Request-Id") != "req-42" {
		t.Fatalf("request id: sent %q, answered %q", gr.hdrs[0].Get("X-Request-Id"), rec.Header().Get("X-Request-Id"))
	}
	if len(reps.puertos) != 1 || reps.puertos[0] != 8090 {
		t.Fatalf("replica acquired with ports %v, want [8090]", reps.puertos)
	}
	// Sin X-Request-Id válido el gateway pone uno, y el raro no se reenvía.
	rec = peticion(h, "POST", "/v1/m/x/v1/decide", tokPrincipal, in, map[string]string{"X-Request-Id": "a b"})
	id := rec.Header().Get("X-Request-Id")
	if id == "" || id == "a b" || gr.hdrs[1].Get("X-Request-Id") != id {
		t.Fatalf("generated request id: answered %q, backend %q", id, gr.hdrs[1].Get("X-Request-Id"))
	}
	// Los errores del backend pasan tal cual (Chispa tiene códigos estables).
	gr.set(422, `{"code":"E_BANK"}`)
	if rec := peticion(h, "POST", "/v1/m/x/v1/decide", tokPrincipal, in, nil); rec.Code != 422 || rec.Body.String() != `{"code":"E_BANK"}` {
		t.Fatalf("backend error: %d %q", rec.Code, rec.Body)
	}
	// Una ruta GET declarada, con su consulta.
	gr.set(200, `{}`)
	if rec := peticion(h, "GET", "/v1/m/x/v1/info?v=1", tokPrincipal, nil, nil); rec.Code != 200 || gr.paths[len(gr.paths)-1] != "GET /v1/info?v=1" {
		t.Fatalf("GET route: %d %v", rec.Code, gr.paths)
	}
}

// Solo se reenvía lo declarado en routes: otro método, otra ruta, un modelo
// que no existe o uno de contrato chispa-classify son 404 del gateway, y el
// backend no ve nada.
func TestProxyModeloRutaNoDeclarada404(t *testing.T) {
	gr := newGrabador(t, 200, `{}`)
	clasico := &Backend{Name: "c", Snapshot: "chispa-c", Port: 8000, Contract: ContractChispaClassify, Idle: IdleFreeze}
	_, h, _ := pasarela(t, gr.srv, decideBackend(), clasico)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/m/x/v1/decide"},
		{"POST", "/v1/m/x/v1/other"},
		{"POST", "/v1/m/x/v1/decide/"},
		{"POST", "/v1/m/x/v1"},
		{"POST", "/v1/m/nadie/v1/decide"},
		{"POST", "/v1/m/c/v1/classify"},
		{"POST", "/v1/m/smol/v1/chat/completions"},
	} {
		rec := peticion(h, c.method, c.path, tokPrincipal, []byte(`{}`), nil)
		if rec.Code != 404 {
			t.Errorf("%s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	if gr.n() != 0 {
		t.Fatalf("backend reached %d times: %v", gr.n(), gr.paths)
	}
}

// La credencial del cliente nunca llega al backend; la que pone auth, sí.
func TestProxyNoReenviaAuthorization(t *testing.T) {
	gr := newGrabador(t, 200, `{}`)
	f := filepath.Join(t.TempDir(), "k.key")
	if err := os.WriteFile(f, []byte("sk-backend\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	conAuth := decideBackend()
	conAuth.Name, conAuth.Auth = "y", &Auth{Header: "Authorization", Scheme: "Bearer", File: f}
	_, h, _ := pasarela(t, gr.srv, decideBackend(), conAuth)
	hdr := map[string]string{"Cookie": "s=1", "X-Api-Key": "cliente"}
	if rec := peticion(h, "POST", "/v1/m/x/v1/decide", tokPrincipal, []byte(`{}`), hdr); rec.Code != 200 {
		t.Fatalf("x: %d", rec.Code)
	}
	if rec := peticion(h, "POST", "/v1/m/y/v1/decide", tokAcme, []byte(`{}`), hdr); rec.Code != 200 {
		t.Fatalf("y: %d", rec.Code)
	}
	if a := gr.hdrs[0].Get("Authorization"); a != "" {
		t.Fatalf("client credential forwarded: %q", a)
	}
	if a := gr.hdrs[1].Get("Authorization"); a != "Bearer sk-backend" {
		t.Fatalf("backend credential: %q", a)
	}
	for _, hd := range gr.hdrs {
		if hd.Get("Cookie") != "" || hd.Get("X-Api-Key") != "" {
			t.Fatalf("client headers forwarded: %v", hd)
		}
	}
}

// tenants acota quién usa un modelo: otro inquilino recibe 403 (en /v1/m y
// por el proxy OpenAI) y no lo ve en /v1/models; el token principal siempre
// puede.
func TestTenantsModelo403(t *testing.T) {
	gr := newGrabador(t, 200, `{"choices":[]}`)
	b := decideBackend()
	b.Tenants = []string{"acme"}
	llm := &Backend{Name: "solo-acme", Snapshot: "von-a", Port: 8000, Contract: ContractOpenAI,
		Routes: []Route{{"POST", "/v1/chat/completions"}}, Tenants: []string{"acme"}}
	_, h, _ := pasarela(t, gr.srv, b, llm)
	for tok, want := range map[string]int{tokPrincipal: 200, tokAcme: 200, tokOtro: 403} {
		if rec := peticion(h, "POST", "/v1/m/x/v1/decide", tok, []byte(`{}`), nil); rec.Code != want {
			t.Errorf("/v1/m with %s: %d, want %d", tok[:4], rec.Code, want)
		}
		if rec := peticion(h, "POST", "/v1/chat/completions", tok, []byte(`{"model":"solo-acme"}`), nil); rec.Code != want {
			t.Errorf("chat with %s: %d, want %d", tok[:4], rec.Code, want)
		}
	}
	if gr.n() != 4 {
		t.Fatalf("backend calls %d, want 4", gr.n())
	}
	if rec := peticion(h, "GET", "/v1/models", tokOtro, nil, nil); strings.Contains(rec.Body.String(), "solo-acme") {
		t.Fatalf("forbidden model listed: %s", rec.Body)
	}
	if rec := peticion(h, "GET", "/v1/models", tokAcme, nil, nil); !strings.Contains(rec.Body.String(), `"id":"solo-acme"`) {
		t.Fatalf("allowed model not listed: %s", rec.Body)
	}
}

// Un backend openai con upstream: el modelo se reescribe con upstream_model,
// no se inyecta seed (no es un dorado) y limits topa max_tokens y rechaza un
// n mayor (R8-04a).
func TestProxyOpenAIUpstreamYLimites(t *testing.T) {
	gr := newGrabador(t, 200, `{"choices":[]}`)
	up := &Backend{Name: "qwen3-gpu", Upstream: gr.srv.URL, Contract: ContractOpenAI,
		Routes:        []Route{{"POST", "/v1/chat/completions"}, {"POST", "/v1/completions"}},
		UpstreamModel: "Qwen/Qwen3-4B", Limits: Limits{MaxTokens: 100, N: 1}, MaxInflight: 4}
	_, h, _ := pasarela(t, gr.srv, up)
	body := func(i int) map[string]any {
		var m map[string]any
		if err := json.Unmarshal(gr.bodies[i], &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if rec := peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu","max_tokens":5000}`), nil); rec.Code != 200 {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body)
	}
	if m := body(0); m["model"] != "Qwen/Qwen3-4B" || m["max_tokens"] != 100.0 || m["seed"] != nil {
		t.Fatalf("forwarded %v", m)
	}
	if rec := peticion(h, "POST", "/v1/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu","prompt":"x","max_tokens":7}`), nil); rec.Code != 200 {
		t.Fatalf("completions: %d", rec.Code)
	}
	if m := body(1); m["max_tokens"] != 7.0 || gr.paths[1] != "POST /v1/completions" {
		t.Fatalf("small max_tokens changed: %v %s", m, gr.paths[1])
	}
	// Sin max_tokens se pone el tope: si no, la generación no tiene fin.
	peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu"}`), nil)
	if m := body(2); m["max_tokens"] != 100.0 {
		t.Fatalf("missing max_tokens not capped: %v", m)
	}
	peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu","max_completion_tokens":900}`), nil)
	if m := body(3); m["max_completion_tokens"] != 100.0 {
		t.Fatalf("max_completion_tokens not capped: %v", m)
	}
	for _, bad := range []string{`{"model":"qwen3-gpu","n":2}`, `{"model":"qwen3-gpu","max_tokens":"x"}`} {
		if rec := peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(bad), nil); rec.Code != 400 {
			t.Fatalf("%s: %d", bad, rec.Code)
		}
	}
	if gr.n() != 4 {
		t.Fatalf("rejected requests reached the backend: %d", gr.n())
	}
}

// /v1/embeddings va por el campo model a los backends openai-embeddings, y
// /v1/models los lista junto a los openai (de dorado o de upstream), no los
// de otros contratos.
func TestEmbeddingsYModelos(t *testing.T) {
	gr := newGrabador(t, 200, `{"data":[]}`)
	emb := &Backend{Name: "e5", Snapshot: "embed-e5", Port: 8000, Contract: ContractOpenAIEmbeddings,
		Routes: []Route{{"POST", "/v1/embeddings"}}}
	up := &Backend{Name: "qwen3-gpu", Upstream: gr.srv.URL, Contract: ContractOpenAI,
		Routes: []Route{{"POST", "/v1/chat/completions"}}}
	_, h, _ := pasarela(t, gr.srv, emb, up, decideBackend())
	if rec := peticion(h, "POST", "/v1/embeddings", tokPrincipal, []byte(`{"model":"e5","input":"hola"}`), nil); rec.Code != 200 {
		t.Fatalf("embeddings: %d %s", rec.Code, rec.Body)
	}
	if gr.paths[0] != "POST /v1/embeddings" || strings.Contains(string(gr.bodies[0]), "seed") {
		t.Fatalf("embeddings forwarded as %s %s", gr.paths[0], gr.bodies[0])
	}
	// Un modelo de chat no es un codificador.
	if rec := peticion(h, "POST", "/v1/embeddings", tokPrincipal, []byte(`{"model":"qwen3-gpu","input":"x"}`), nil); rec.Code != 404 {
		t.Fatalf("embeddings to a chat model: %d", rec.Code)
	}
	rec := peticion(h, "GET", "/v1/models", tokPrincipal, nil, nil)
	for _, id := range []string{"e5", "qwen3-gpu", "smol"} {
		if !strings.Contains(rec.Body.String(), `"id":"`+id+`"`) {
			t.Errorf("%s not listed: %s", id, rec.Body)
		}
	}
	if strings.Contains(rec.Body.String(), `"id":"x"`) {
		t.Errorf("chispa-decide model listed: %s", rec.Body)
	}
}

// BenchmarkProxyModelo mide /v1/m/{modelo}/…: el cuerpo pasa sin leerse
// entero, así que lo asignado no debe crecer con él.
func BenchmarkProxyModelo(b *testing.B) {
	for _, n := range []int{1 << 10, 512 << 10} {
		b.Run(tamano(n), func(b *testing.B) {
			srv := sumidero(b)
			g, err := New(Options{
				Config:   &Config{Models: map[string]*ModelConfig{"smol": {Kind: KindVON, Snapshot: "von-smol"}}},
				Replicas: &puertoReplicas{addr: strings.TrimPrefix(srv.URL, "http://")},
				EvalDir:  b.TempDir(),
			})
			if err != nil {
				b.Fatal(err)
			}
			conBackends(g, decideBackend())
			benchHandler(b, g.Handler(""), "/v1/m/x/v1/decide", cuerpoOpenAI(n))
		})
	}
}
