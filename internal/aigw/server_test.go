package aigw

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
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
