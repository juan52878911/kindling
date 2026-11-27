package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// R8-05: un cuerpo JSON null decodifica sin error a un mapa nil; con un único
// modelo VON el proxy escribía en ese mapa y entraba en pánico.
func TestProxyNullBody400(t *testing.T) {
	g, _, _ := newTestGateway(t, nil)
	h := g.Handler("")
	for _, body := range []string{"null", " null ", "[]", `"x"`, "7"} {
		if rec := do(t, h, "POST", "/v1/chat/completions", "", body); rec.Code != 400 {
			t.Fatalf("%q: %d %s", body, rec.Code, rec.Body)
		}
	}
}

// R8-11: los 502/503 del proxy no enseñan el texto del error interno (la IP
// del invitado, los mensajes del daemon) a quien pregunta.
func TestProxyErroresSinTextoInterno(t *testing.T) {
	g, _, reps := newTestGateway(t, nil)
	h := g.Handler("")
	reps.fail = errors.New("dial tcp 172.16.0.5:8000: daemon says secret-detail")
	rec := do(t, h, "POST", "/v1/chat/completions", "", map[string]any{"model": "smol"})
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("wake failure: %d %v", rec.Code, rec.Header())
	}
	if b := rec.Body.String(); strings.Contains(b, "172.16") || strings.Contains(b, "secret-detail") {
		t.Fatalf("503 leaks internals: %s", b)
	}
	// Una réplica que no contesta: la conexión se rechaza dos veces (dial y
	// reintento) y el cliente recibe 502 sin la dirección.
	reps.fail = nil
	reps.addr = "127.0.0.1:1"
	rec = do(t, h, "POST", "/v1/chat/completions", "", map[string]any{"model": "smol"})
	if rec.Code != 502 && rec.Code != 503 {
		t.Fatalf("dead replica: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Fatalf("error leaks the replica address: %s", rec.Body)
	}
}

// R8-11: el Content-Type del invitado solo pasa si está en la lista blanca, y
// toda respuesta del proxy lleva nosniff: un invitado no sirve HTML bajo el
// origen del gateway.
func TestProxyContentTypeListaBlanca(t *testing.T) {
	g, _, reps := newTestGateway(t, nil)
	h := g.Handler("")
	for ct, want := range map[string]string{
		"text/html":                       "application/octet-stream",
		"application/json; charset=utf-8": "application/json",
		"text/event-stream":               "text/event-stream",
		"application/x-ndjson":            "application/x-ndjson",
		"image/svg+xml":                   "application/octet-stream",
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = io.WriteString(w, `{"x":1}`)
		}))
		reps.addr = strings.TrimPrefix(srv.URL, "http://")
		rec := do(t, h, "POST", "/v1/chat/completions", "", map[string]any{"model": "smol"})
		srv.Close()
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, want) {
			t.Errorf("%s: Content-Type %q, want %q", ct, got, want)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: no nosniff", ct)
		}
	}
}

// R8-11: pasado MaxProxyBytes, una respuesta sin streaming no puede acabar en
// un 200 limpio con el JSON a medias: el cliente tiene que ver un error.
func TestProxyCorteNoStreamDaError(t *testing.T) {
	g, ll, _ := newTestGateway(t, nil)
	g.opts.MaxProxyBytes = 64
	ll.set(strings.Repeat("x", 500))
	srv := httptest.NewServer(g.Handler(""))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"smol"}`))
	if err != nil {
		return // cortada antes de las cabeceras: también es un error visible
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil && resp.StatusCode == 200 {
		t.Fatalf("truncated answer delivered as a clean 200: %q", b)
	}
}

// R8-11: las métricas del proxy van por nombre de modelo (no por dorado) y el
// código por clase, no el que elija el invitado.
func TestMetricasPorModeloYClase(t *testing.T) {
	g, _, reps := newTestGateway(t, nil)
	h := g.Handler("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(299)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer srv.Close()
	reps.addr = strings.TrimPrefix(srv.URL, "http://")
	do(t, h, "POST", "/v1/chat/completions", "", map[string]any{"model": "smol"})
	reps.addr = "127.0.0.1:1"
	do(t, h, "POST", "/v1/chat/completions", "", map[string]any{"model": "smol"})
	m := do(t, h, "GET", "/metrics", "", nil).Body.String()
	if !strings.Contains(m, `kling_ai_proxy_requests_total{model="smol",code="2xx"} 1`) {
		t.Errorf("proxy code not grouped by class:\n%s", grepLines(m, "proxy_requests"))
	}
	if strings.Contains(m, `model="von-smol"`) {
		t.Errorf("metrics labelled with the snapshot:\n%s", grepLines(m, "von-smol"))
	}
	if !strings.Contains(m, `kling_ai_von_errors_total{model="smol",reason="dial"}`) {
		t.Errorf("dial error not labelled with the model:\n%s", grepLines(m, "von_errors"))
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// R8-11: /v1/models lista también los codificadores (contrato
// openai-embeddings), que se usan por /v1/embeddings.
func TestModelsIncluyeEmbed(t *testing.T) {
	g, _, _ := newTestGateway(t, func(c *Config) {
		c.Models["e5"] = &ModelConfig{Kind: KindEmbed, Snapshot: "embed-e5"}
	})
	rec := do(t, g.Handler(""), "GET", "/v1/models", "", nil)
	if !strings.Contains(rec.Body.String(), `"id":"e5"`) || !strings.Contains(rec.Body.String(), `"id":"smol"`) {
		t.Fatalf("models: %s", rec.Body)
	}
}

// R8-11: el recuento de réplicas de /metrics cuenta también las de embed y
// las de Chispa con backend microvm, no solo las de VON.
func TestReplicaCountsIncluyeEmbedYChispa(t *testing.T) {
	d := &fakeDaemon{machines: map[string]*api.Machine{}}
	client := d.serve(t)
	for id, snap := range map[string]string{"r1": "von-smol", "r2": "embed-e5", "r3": "chispa-c"} {
		d.machines[id] = &api.Machine{ID: id, State: api.StateRunning, From: snap,
			Labels: map[string]string{api.LabelService: snap, LabelGateway: "default"}}
		d.order = append(d.order, id)
	}
	cfg := &Config{Models: map[string]*ModelConfig{
		"smol": {Kind: KindVON, Snapshot: "von-smol"},
		"e5":   {Kind: KindEmbed, Snapshot: "embed-e5"},
		"c":    {Kind: KindChispa, Backend: BackendMicroVM, Snapshot: "chispa-c"},
	}}
	g, err := New(Options{Client: client, Config: cfg, EvalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got := g.replicaCounts(context.Background())
	for _, n := range []string{"smol", "e5", "c"} {
		if got[n].running != 1 {
			t.Errorf("%s: %+v (all: %+v)", n, got[n], got)
		}
	}
}

// Un repuesto (etiqueta kling.spare) no es una réplica en marcha: /metrics
// lo cuenta aparte para no inflar las réplicas que sirven.
func TestReplicaCountsRepuestosAparte(t *testing.T) {
	d := &fakeDaemon{machines: map[string]*api.Machine{}}
	client := d.serve(t)
	for id, spare := range map[string]bool{"r1": false, "r2": true, "r3": true} {
		lb := map[string]string{api.LabelService: "von-smol", LabelGateway: "default"}
		if spare {
			lb[scheduler.LabelSpare] = "true"
		}
		d.machines[id] = &api.Machine{ID: id, State: api.StateRunning, From: "von-smol", Labels: lb}
		d.order = append(d.order, id)
	}
	cfg := &Config{Models: map[string]*ModelConfig{"smol": {Kind: KindVON, Snapshot: "von-smol"}}}
	g, err := New(Options{Client: client, Config: cfg, EvalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.replicaCounts(context.Background())["smol"]; got.running != 1 || got.spare != 2 {
		t.Fatalf("counts %+v, want running 1 and spare 2", got)
	}
	var b strings.Builder
	g.met.write(&b, chispaStats{}, nil, g.replicaCounts(context.Background()), learnSnap{})
	if !strings.Contains(b.String(), `kling_ai_von_replicas{model="smol",state="running"} 1`) ||
		!strings.Contains(b.String(), `kling_ai_von_replicas{model="smol",state="spare"} 2`) {
		t.Fatalf("metrics:\n%s", grepLines(b.String(), "von_replicas"))
	}
}

// secrets[].file es relativo al ai.json, como auth.file y ca_file: se hace
// absoluto antes de validar (validateSecrets lo exige absoluto).
func TestSecretsFileRelativoAlRegistro(t *testing.T) {
	c := &Config{Models: map[string]*ModelConfig{
		"m": {Kind: KindHTTP, Snapshot: "s", Contract: ContractChispaDecide,
			Secrets: []SecretConfig{{Name: "a", File: "clave"}, {Name: "b", File: "/abs/clave"}}},
	}}
	c.resolveAuthFiles("/etc/kling")
	if got := c.Models["m"].Secrets; got[0].File != "/etc/kling/clave" || got[1].File != "/abs/clave" {
		t.Fatalf("secrets files: %+v", got)
	}
}

// TestLimitesLogprobs: top_logprobs (y logprobs entero de completions) y
// prompt_logprobs de vLLM se topan: por encima del tope, o negativos (en vLLM,
// -1 es «todo el vocabulario»), 400 sin llegar al backend. logprobs booleano
// de chat pasa.
func TestLimitesLogprobs(t *testing.T) {
	l := Limits{TopLogprobs: 5, PromptLogprobs: 2}
	malas := []string{
		`{"top_logprobs":6}`, `{"top_logprobs":-1}`, `{"logprobs":6}`, `{"prompt_logprobs":3}`,
		`{"prompt_logprobs":-1}`, `{"top_logprobs":"x"}`,
	}
	for _, m := range malas {
		var req map[string]json.RawMessage
		if err := json.Unmarshal([]byte(m), &req); err != nil {
			t.Fatal(err)
		}
		if err := aplicarLimites(req, l); err == nil {
			t.Errorf("accepted %s", m)
		}
	}
	for _, m := range []string{
		`{"top_logprobs":5,"logprobs":true}`, `{"logprobs":5,"prompt_logprobs":2}`, `{"logprobs":false}`,
		`{"prompt_logprobs":null}`, `{}`,
	} {
		var req map[string]json.RawMessage
		if err := json.Unmarshal([]byte(m), &req); err != nil {
			t.Fatal(err)
		}
		if err := aplicarLimites(req, l); err != nil {
			t.Errorf("rejected %s: %v", m, err)
		}
	}
	// Sin tope, nada se mira (lo de hoy).
	var req map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"top_logprobs":100,"prompt_logprobs":-1}`), &req)
	if err := aplicarLimites(req, Limits{}); err != nil {
		t.Errorf("without limits: %v", err)
	}
	// Y por el proxy: 400 y el backend no lo ve.
	gr := newGrabador(t, 200, `{"choices":[]}`)
	up := &Backend{Name: "qwen3-gpu", Upstream: gr.srv.URL, Contract: ContractOpenAI,
		Routes: []Route{{"POST", "/v1/chat/completions"}}, Limits: l}
	_, h, _ := pasarela(t, gr.srv, up)
	if rec := peticion(h, "POST", "/v1/chat/completions", tokPrincipal, []byte(`{"model":"qwen3-gpu","prompt_logprobs":20}`), nil); rec.Code != 400 {
		t.Fatalf("over prompt_logprobs: %d", rec.Code)
	}
	if gr.n() != 0 {
		t.Fatal("rejected request reached the backend")
	}
	for _, m := range []string{
		`{"kind":"http","upstream":"http://h:1","contract":"openai","limits":{"top_logprobs":-1}}`,
		`{"kind":"http","upstream":"http://h:1","contract":"openai","limits":{"prompt_logprobs":100000}}`,
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","limits":{"top_logprobs":5}}`,
	} {
		if _, err := cargaBloque(m, ""); err == nil {
			t.Errorf("accepted %s", m)
		}
	}
	if _, err := cargaBloque(`{"kind":"http","upstream":"http://h:1","contract":"openai","limits":{"top_logprobs":20,"prompt_logprobs":5}}`, ""); err != nil {
		t.Error(err)
	}
}

// TestLimiteTimeout: limits.timeout es el plazo de una petición a ese modelo
// en lugar del -proxy-timeout del gateway, en cualquier contrato.
func TestLimiteTimeout(t *testing.T) {
	lento := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer lento.Close()
	b := decideBackend()
	b.Limits.Timeout = "50ms"
	_, h, _ := pasarela(t, lento, b)
	t0 := time.Now()
	rec := peticion(h, "POST", "/v1/m/x/v1/decide", tokPrincipal, []byte(`{}`), nil)
	if d := time.Since(t0); rec.Code < 500 || d > time.Second {
		t.Fatalf("limits.timeout 50ms: %d after %v", rec.Code, d)
	}
	for _, m := range []string{
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","limits":{"timeout":"pronto"}}`,
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","limits":{"timeout":"0s"}}`,
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","limits":{"timeout":"3h"}}`,
	} {
		if _, err := cargaBloque(m, ""); err == nil {
			t.Errorf("accepted %s", m)
		}
	}
	for _, m := range []string{
		`{"kind":"http","snapshot":"s","contract":"chispa-decide","limits":{"timeout":"90s"}}`,
		`{"kind":"http","upstream":"http://h:1","contract":"openai","limits":{"timeout":"10m","max_tokens":5}}`,
		`{"kind":"von","snapshot":"s","limits":{"timeout":"30s"}}`,
	} {
		if _, err := cargaBloque(m, ""); err != nil {
			t.Errorf("rejected %s: %v", m, err)
		}
	}
}
