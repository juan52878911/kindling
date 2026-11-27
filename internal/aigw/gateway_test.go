package aigw

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/chispa"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// Si Chispa está seguro contesta él y VON ni se entera; si duda, contesta VON y
// su respuesta se traduce a una etiqueta del conjunto de Chispa.
func TestCascada(t *testing.T) {
	g, ll, reps := newTestGateway(t, nil)
	h := g.Handler("")

	// Todo confiado: umbral 0 en todas las clases.
	g.config().Tasks["kind"].Thresholds = map[string]float64{"bug": 0, "chore": 0, "docs": 0, "feat": 0}
	rec := do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash the parser segfault"})
	if rec.Code != 200 {
		t.Fatalf("classify: %d %s", rec.Code, rec.Body)
	}
	r := decode[ClassifyResponse](t, rec)
	if r.Source != "chispa" || r.Label != "bug" || r.Chispa == nil || r.Chispa.Decision != chispa.DecisionConfident {
		t.Fatalf("confident answer = %+v", r)
	}
	if n := ll.calls.Load(); n != 0 {
		t.Fatalf("a confident answer called VON %d times", n)
	}

	// Todo escala: umbral 2 (nunca).
	g.config().Tasks["kind"].Thresholds = map[string]float64{"bug": 2, "chore": 2, "docs": 2, "feat": 2}
	ll.set("Docs.")
	rec = do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash the parser segfault"})
	r = decode[ClassifyResponse](t, rec)
	if r.Source != "von" || r.Label != "docs" || r.VON == nil || r.Chispa.Decision != chispa.DecisionEscalate {
		t.Fatalf("escalated answer = %+v", r)
	}
	if len(r.Chispa.Candidates) == 0 || len(r.Evidence) == 0 {
		t.Fatalf("escalation without candidates/evidence: %+v", r)
	}
	if reps.acquired.Load() != 1 || reps.released.Load() != 1 {
		t.Fatalf("replica acquired %d released %d", reps.acquired.Load(), reps.released.Load())
	}
	body := ll.last()
	gram, _ := body["grammar"].(string)
	if !strings.HasPrefix(gram, "root ::= ") || !strings.Contains(gram, `"docs"`) {
		t.Fatalf("grammar = %q", gram)
	}
	if _, ok := body["seed"]; !ok || body["temperature"] != 0.0 {
		t.Fatalf("request to VON without seed or with temperature: %v", body)
	}
	// Muestra guardada para recalibrar.
	if n := g.rings["kind"].len(); n != 1 {
		t.Fatalf("samples = %d, want 1", n)
	}

	// Una respuesta que no es exactamente una etiqueta es unknown, no se guarda.
	ll.set("I think it is a docs change")
	r = decode[ClassifyResponse](t, do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash"}))
	if r.Label != Unknown || r.Source != "von" {
		t.Fatalf("free-text answer = %+v", r)
	}
	if n := g.rings["kind"].len(); n != 1 {
		t.Fatalf("an unknown answer was recorded as a sample")
	}

	// Con top_k, la gramática y la pregunta solo llevan los candidatos de Chispa.
	g.config().Tasks["kind"].TopK = 2
	ll.set("bug")
	r = decode[ClassifyResponse](t, do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash panic segfault"}))
	gram, _ = ll.last()["grammar"].(string)
	if strings.Count(gram, "|") != 1 || !strings.Contains(gram, `"bug"`) || r.Label != "bug" {
		t.Fatalf("top_k grammar = %q, answer %+v", gram, r)
	}
	g.config().Tasks["kind"].TopK = 0
	g.rings["kind"] = newRing(10)
	g.rings["kind"].add(sample{pred: "bug", prob: 0.5, teacher: "docs", weight: 1})

	// /v1/decide es lo mismo con decision.
	ll.set("feat")
	r = decode[ClassifyResponse](t, do(t, h, "POST", "/v1/decide", "", map[string]any{"task": "kind", "text": "x"}))
	if r.Decision != "feat" || r.Label != "feat" {
		t.Fatalf("decide = %+v", r)
	}

	// Métricas: requests por fuente, cobertura, escalado.
	m := do(t, h, "GET", "/metrics", "", nil).Body.String()
	for _, want := range []string{
		`kling_ai_requests_total{endpoint="classify",task="kind",source="chispa"} 1`,
		`kling_ai_requests_total{endpoint="classify",task="kind",source="von"} 3`,
		`kling_ai_von_unknown_total{task="kind"} 1`,
		`kling_ai_chispa_coverage{task="kind"} 0.2`,
		`kling_ai_escalation_rate{task="kind"} 0.8`,
		`kling_ai_latency_seconds_count{task="kind",source="von"} 4`,
		`kling_ai_samples{task="kind"} 2`,
		`kling_ai_chispa_models_loaded 1`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// mode=chispa contesta Chispa aunque dude y aunque la cascada esté activa; VON solo
// ya no es un modo de clasificar (se mide con kling ai eval -von-alone).
func TestModos(t *testing.T) {
	g, ll, _ := newTestGateway(t, func(c *Config) {
		c.Tasks["kind"].Thresholds = map[string]float64{"bug": 2, "chore": 2, "docs": 2, "feat": 2}
	})
	h := g.Handler("")
	r := decode[ClassifyResponse](t, do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash panic", "mode": "chispa"}))
	if r.Source != "chispa" || r.Label != "bug" || !r.Escalate || r.Degraded != "" || ll.calls.Load() != 0 {
		t.Fatalf("mode chispa = %+v (von calls %d)", r, ll.calls.Load())
	}
	for _, mode := range []string{"von", "nope"} {
		if rec := do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "x", "mode": mode}); rec.Code != 400 {
			t.Fatalf("mode %s: %d", mode, rec.Code)
		}
	}
	if rec := do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "nope", "text": "x"}); rec.Code != 404 {
		t.Fatalf("unknown task: %d", rec.Code)
	}
}

// Sin cascada (sin escalate_to) la duda de Chispa vuelve al cliente marcada
// escalate: true, con los candidatos y la evidencia para decidir; VON no se
// toca, ni para auditar.
func TestSinCascadaLaDudaVuelveAlCliente(t *testing.T) {
	g, ll, reps := newTestGateway(t, func(c *Config) {
		c.Tasks["kind"] = &TaskConfig{Chispa: "commits", Thresholds: map[string]float64{"bug": 2}}
	})
	h := g.Handler("")
	r := decode[ClassifyResponse](t, do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash panic segfault"}))
	if r.Source != "chispa" || r.Label != "bug" || !r.Escalate || r.VON != nil || r.Degraded != "" ||
		len(r.Chispa.Candidates) == 0 || len(r.Evidence) == 0 {
		t.Fatalf("unsure answer without a cascade = %+v", r)
	}
	r = decode[ClassifyResponse](t, do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "readme typo documentation"}))
	if r.Escalate || r.Label != "docs" {
		t.Fatalf("confident answer = %+v", r)
	}
	if ll.calls.Load() != 0 || reps.acquired.Load() != 0 {
		t.Fatalf("VON was called %d times without a cascade", ll.calls.Load())
	}
	m := do(t, h, "GET", "/metrics", "", nil).Body.String()
	for _, want := range []string{
		`kling_ai_requests_total{endpoint="classify",task="kind",source="escalated"} 1`,
		`kling_ai_requests_total{endpoint="classify",task="kind",source="chispa"} 1`,
		`kling_ai_escalation_rate{task="kind"} 0.5`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %q", want)
		}
	}
}

// Si VON no contesta, la cascada responde con Chispa marcado como degradado, o
// con 503 si la tarea lo pide.
func TestVONCaido(t *testing.T) {
	g, _, reps := newTestGateway(t, func(c *Config) {
		c.Tasks["kind"].Thresholds = map[string]float64{"bug": 2, "chore": 2, "docs": 2, "feat": 2}
	})
	reps.fail = errors.New("insufficient memory")
	h := g.Handler("")
	r := decode[ClassifyResponse](t, do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash"}))
	if r.Source != "chispa" || r.Label != "bug" || !strings.Contains(r.Degraded, "insufficient memory") {
		t.Fatalf("degraded = %+v", r)
	}
	g.config().Tasks["kind"].OnVONError = "error"
	rec := do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash"})
	if rec.Code != 503 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("on_von_error=error: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(do(t, h, "GET", "/metrics", "", nil).Body.String(), `kling_ai_von_errors_total{model="smol",reason="wake"} 2`) {
		t.Fatal("wake errors not counted")
	}
}

// Token: sin él 401, con él 200; /healthz abierto; un tenant con cuota da 429
// y no puede usar las rutas de administración.
func TestAuthYCuotas(t *testing.T) {
	g, _, _ := newTestGateway(t, func(c *Config) {
		c.Tenants = []TenantConfig{{Name: "bot", Token: "tenant-token-0123456789", MaxInflight: 1}}
		c.Tasks["kind"].Thresholds = map[string]float64{"bug": 2, "chore": 2, "docs": 2, "feat": 2}
	})
	h := g.Handler("main-token-0123456789")
	if rec := do(t, h, "GET", "/v1/models", "", nil); rec.Code != 401 {
		t.Fatalf("no token: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/models", "wrong", nil); rec.Code != 401 {
		t.Fatalf("wrong token: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/healthz", "", nil); rec.Code != 200 {
		t.Fatalf("healthz: %d", rec.Code)
	}
	rec := do(t, h, "GET", "/v1/models", "main-token-0123456789", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"smol"`) {
		t.Fatalf("models: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/v1/admin/calibrate", "tenant-token-0123456789", map[string]any{"task": "kind"}); rec.Code != 403 {
		t.Fatalf("tenant on admin: %d", rec.Code)
	}
	// Cuota: una petición del tenant retenida en VON ocupa su único hueco y la
	// siguiente recibe 429; el token principal no tiene cuota.
	fl := newFakeLlama(t)
	fl.entered, fl.block = make(chan struct{}, 1), make(chan struct{})
	g.replicas = &fakeReplicas{addr: strings.TrimPrefix(fl.srv.URL, "http://")}
	done := make(chan int)
	go func() {
		done <- do(t, h, "POST", "/v1/classify", "tenant-token-0123456789", map[string]any{"task": "kind", "text": "crash"}).Code
	}()
	<-fl.entered
	if rec := do(t, h, "GET", "/v1/tasks", "tenant-token-0123456789", nil); rec.Code != 429 {
		t.Fatalf("second request of the tenant: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/tasks", "main-token-0123456789", nil); rec.Code != 200 {
		t.Fatalf("main token while tenant busy: %d", rec.Code)
	}
	close(fl.block)
	if c := <-done; c != 200 {
		t.Fatalf("held request: %d", c)
	}
	if rec := do(t, h, "GET", "/v1/tasks", "tenant-token-0123456789", nil); rec.Code != 200 {
		t.Fatalf("tenant after release: %d", rec.Code)
	}
}

// Límites: cuerpo grande 413; rutas de control del agente 404; JSON con
// campos desconocidos 400.
func TestLimites(t *testing.T) {
	g, _, _ := newTestGateway(t, nil)
	g.opts.MaxBody = 1024
	h := g.Handler("")
	big := `{"task":"kind","text":"` + strings.Repeat("a", 4096) + `"}`
	if rec := do(t, h, "POST", "/v1/classify", "", big); rec.Code != 413 {
		t.Fatalf("big body: %d %s", rec.Code, rec.Body)
	}
	for _, p := range []string{"/exec", "/files/etc/passwd", "/resync", "/volume/x"} {
		if rec := do(t, h, "POST", p, "", "{}"); rec.Code != 404 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	if rec := do(t, h, "POST", "/v1/classify", "", `{"task":"kind","text":"x","evil":1}`); rec.Code != 400 {
		t.Fatalf("unknown field: %d", rec.Code)
	}
}

// El proxy OpenAI: elige réplica por "model", añade semilla si falta, no pasa
// el token al invitado, hace streaming y corta respuestas desmesuradas.
func TestProxyOpenAI(t *testing.T) {
	g, ll, reps := newTestGateway(t, nil)
	h := g.Handler("main-token-0123456789")
	ll.set("hello there friend")
	rec := do(t, h, "POST", "/v1/chat/completions", "main-token-0123456789",
		map[string]any{"model": "smol", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello there friend") {
		t.Fatalf("chat: %d %s", rec.Code, rec.Body)
	}
	if _, ok := ll.last()["seed"]; !ok {
		t.Fatal("no seed added")
	}
	ll.mu.Lock()
	if ll.auth[len(ll.auth)-1] != "" {
		t.Fatal("the gateway token reached the guest")
	}
	ll.mu.Unlock()

	// Por el nombre del dorado también, y en streaming.
	rec = do(t, h, "POST", "/v1/chat/completions", "main-token-0123456789",
		map[string]any{"model": "von-smol", "stream": true, "seed": 7, "messages": []any{}})
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/event-stream" ||
		strings.Count(rec.Body.String(), "data: ") != 4 || !rec.Flushed {
		t.Fatalf("stream: %d %q flushed=%v", rec.Code, rec.Body, rec.Flushed)
	}
	if ll.last()["seed"] != 7.0 {
		t.Fatalf("client seed overwritten: %v", ll.last()["seed"])
	}
	if reps.acquired.Load() != reps.released.Load() {
		t.Fatal("replica not released")
	}

	if rec := do(t, h, "POST", "/v1/chat/completions", "main-token-0123456789", map[string]any{"model": "gpt-4"}); rec.Code != 404 {
		t.Fatalf("unknown model: %d", rec.Code)
	}

	g.opts.MaxProxyBytes = 64
	ll.set(strings.Repeat("x", 500))
	rec = do(t, h, "POST", "/v1/chat/completions", "main-token-0123456789", map[string]any{"model": "smol"})
	if rec.Body.Len() > 64 {
		t.Fatalf("answer not capped: %d bytes", rec.Body.Len())
	}
}

// La caché de Chispa respeta el presupuesto: al cargar un segundo modelo con el
// presupuesto de uno, el primero sale.
func TestCacheChispa(t *testing.T) {
	p1, p2 := trainedModel(t), trainedModel(t)
	m, err := chispa.LoadFile(p1)
	if err != nil {
		t.Fatal(err)
	}
	c := newChispaCache(modelBytes(m) + 10)
	if _, err := c.get(p1); err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(p2); err != nil {
		t.Fatal(err)
	}
	st := c.stats()
	if st.Loaded != 1 || st.Evictions != 1 || st.Loads != 2 {
		t.Fatalf("stats = %+v", st)
	}
	if _, err := c.get(p1); err != nil { // vuelve a cargarse
		t.Fatal(err)
	}
	if c.stats().Loads != 3 {
		t.Fatal("evicted model was not reloaded")
	}
	if _, err := c.get("/no/such.chispa"); err == nil || c.stats().Failures != 1 {
		t.Fatal("missing model did not fail")
	}
	_ = os.Remove(p2)
}

func TestConfig(t *testing.T) {
	bad := []string{
		`{"models":{"a":{"kind":"gpt"}}}`,
		`{"models":{"A":{"kind":"chispa","path":"x"}}}`,
		`{"models":{"a":{"kind":"von"}}}`,
		`{"models":{"a":{"kind":"chispa","path":"x"}},"tasks":{"t":{"von":"a"}}}`,
		`{"models":{"a":{"kind":"chispa","path":"x"}},"tasks":{"t":{}}}`,
		// chispa y von juntos: la escalada es escalate_to, von es para generar.
		`{"models":{"j":{"kind":"chispa","path":"x"},"v":{"kind":"von","snapshot":"s"}},"tasks":{"t":{"chispa":"j","von":"v"}}}`,
		`{"models":{"j":{"kind":"chispa","path":"x"}},"tasks":{"t":{"chispa":"j","escalate_to":"j"}}}`,
		`{"models":{"j":{"kind":"chispa","path":"x"}},"tasks":{"t":{"chispa":"j","escalate_force":true}}}`,
		`{"models":{"j":{"kind":"chispa","path":"x"}},"tasks":{"t":{"chispa":"j","audit":0.1}}}`,
		`{"models":{"j":{"kind":"chispa","path":"x"}},"tasks":{"t":{"chispa":"j","labels":["a","a"]}}}`,
		`{"models":{"j":{"kind":"chispa","path":"x"}},"tasks":{"t":{"chispa":"j","temperature":0.5}}}`,
		// Lo de clasificar no vale en una generación.
		`{"models":{"v":{"kind":"von","snapshot":"s"}},"tasks":{"t":{"von":"v","labels":["a"]}}}`,
		`{"models":{"v":{"kind":"von","snapshot":"s"}},"tasks":{"t":{"von":"v","top_k":3}}}`,
		`{"models":{"v":{"kind":"von","snapshot":"s"}},"tasks":{"t":{"von":"v","max_tokens":5000}}}`,
		`{"models":{"v":{"kind":"von","snapshot":"s"}},"tasks":{"t":{"von":"v","temperature":3}}}`,
		`{"models":{},"typo":1}`,
		`{"tenants":[{"name":"x","token":"short"}]}`,
		`{"tenants":[{"name":"default","token":"0123456789abcdefgh"}]}`,
	}
	for _, b := range bad {
		if _, err := ParseConfig(strings.NewReader(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
	dir := t.TempDir()
	p := dir + "/ai.json"
	_ = os.WriteFile(p, []byte(`{"models":{"j":{"kind":"chispa","path":"m.chispa"},"v":{"kind":"von","snapshot":"von-smol"}},
		"tasks":{"t":{"chispa":"j","escalate_to":"v","top_k":3},"sum":{"von":"v","prompt":"Summarize:\n{input}","max_tokens":200,"temperature":0.2}}}`), 0o600)
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Models["j"].Path != dir+"/m.chispa" {
		t.Fatalf("relative path = %q", c.Models["j"].Path)
	}
	if n, m := c.vonModel("von-smol"); n != "v" || m == nil {
		t.Fatal("von model not found by snapshot")
	}
	if !c.Tasks["sum"].IsGenerate() || c.Tasks["t"].IsGenerate() {
		t.Fatal("task kinds")
	}
}

func TestParseLabel(t *testing.T) {
	labels := []string{"fix", "feat", "docs"}
	for in, want := range map[string]string{
		"fix":               "fix",
		"  Fix.\n":          "fix",
		"`feat`":            "feat",
		"**docs**":          "docs",
		"Label: docs":       "docs",
		"\"feat\"\nbecause": "feat",
		"it's a fix":        Unknown,
		"fixes":             Unknown,
		"":                  Unknown,
		"fix, feat":         Unknown,
	} {
		if got := parseLabel(in, labels); got != want {
			t.Errorf("parseLabel(%q) = %q, want %q", in, got, want)
		}
	}
	if g := grammarFor([]string{`a"b`, `c\d`, "e\x01"}); g != `root ::= "a\"b" | "c\\d" | "e\x01"` {
		t.Errorf("grammar = %s", g)
	}
	// Lo que trae el texto no se vuelve a expandir.
	got := renderPrompt("{labels}|{text}", []string{"x"}, chispa.Input{Text: "{labels}"}, nil)
	if got != "x|{labels}" {
		t.Errorf("prompt = %q", got)
	}
	if s := truncUTF8("añb", 2); s != "a" {
		t.Errorf("truncUTF8 = %q", s)
	}
}

// ---- ciclo de vida (Z4 de la ronda 8)

// escribirRegistro escribe un ai.json 0600 con un modelo VON (smol) más los
// modelos e inquilinos que se den.
func escribirRegistro(t *testing.T, p string, modelos map[string]any, tenants []TenantConfig) {
	t.Helper()
	ms := map[string]any{"smol": map[string]any{"kind": "von", "snapshot": "von-smol"}}
	for k, v := range modelos {
		ms[k] = v
	}
	doc := map[string]any{"models": ms, "tasks": map[string]any{}}
	if len(tenants) > 0 {
		doc["tenants"] = tenants
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Quitar un inquilino de ai.json y recargar da 401 a su token sin reiniciar;
// uno añadido entra; y el token principal sigue valiendo (R8-02).
func TestReloadRevocaInquilino(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ai.json")
	escribirRegistro(t, p, nil, []TenantConfig{{Name: "acme", Token: "tenant-acme-0123456789"}})
	g, err := New(Options{ConfigPath: p, Replicas: &fakeReplicas{}})
	if err != nil {
		t.Fatal(err)
	}
	h := g.Handler("main-token-0123456789")
	if rec := do(t, h, "GET", "/v1/models", "tenant-acme-0123456789", nil); rec.Code != 200 {
		t.Fatalf("acme before reload: %d", rec.Code)
	}
	escribirRegistro(t, p, nil, []TenantConfig{{Name: "beta", Token: "tenant-beta-0123456789"}})
	if _, err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "GET", "/v1/models", "tenant-acme-0123456789", nil); rec.Code != 401 {
		t.Fatalf("acme after being removed and reloaded: %d, want 401", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/models", "tenant-beta-0123456789", nil); rec.Code != 200 {
		t.Fatalf("beta after being added and reloaded: %d, want 200", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/models", "main-token-0123456789", nil); rec.Code != 200 {
		t.Fatalf("main token after reload: %d", rec.Code)
	}
}

// Un ai.json con tokens de inquilino legible por otros no se carga en un
// reload, igual que al arrancar (kling ai serve): se queda el registro de antes.
func TestReloadRechazaInquilinosLegibles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ai.json")
	escribirRegistro(t, p, nil, []TenantConfig{{Name: "acme", Token: "tenant-acme-0123456789"}})
	g, err := New(Options{ConfigPath: p, Replicas: &fakeReplicas{}})
	if err != nil {
		t.Fatal(err)
	}
	h := g.Handler("main-token-0123456789")
	escribirRegistro(t, p, nil, []TenantConfig{{Name: "beta", Token: "tenant-beta-0123456789"}})
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Reload(); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("reload of a world-readable registry with tenant tokens: %v", err)
	}
	if rec := do(t, h, "GET", "/v1/models", "tenant-acme-0123456789", nil); rec.Code != 200 {
		t.Fatalf("acme after a refused reload: %d, want 200 (old registry kept)", rec.Code)
	}
}

// Los ganchos del planificador salen de Backends y se recalculan en Reload:
// puerto, en vuelo por réplica, política de ociosidad y sonda cambian sin
// reiniciar. kind http porque port no vale para el atajo von. Un dorado que no es de ningún backend da los ceros (lo de hoy).
func TestHooksSeRecalculanEnReload(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ai.json")
	escribirRegistro(t, p, map[string]any{"smol": map[string]any{"kind": "http", "contract": "openai", "snapshot": "von-smol",
		"port": 8090, "max_inflight": 4, "idle": "pause", "health": map[string]any{"path": "/healthz", "timeout": "2s"}}}, nil)
	g, err := New(Options{ConfigPath: p, Replicas: &fakeReplicas{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	s := g.sched
	if s.PortFor == nil || s.MaxInflightFor == nil || s.IdleFor == nil || s.ReadyFor == nil || s.OnForget == nil {
		t.Fatal("scheduler hooks not wired")
	}
	if got := s.PortFor("von-smol"); got != 8090 {
		t.Fatalf("PortFor = %d, want 8090", got)
	}
	if got := s.MaxInflightFor("von-smol"); got != 4 {
		t.Fatalf("MaxInflightFor = %d, want 4", got)
	}
	if got := s.IdleFor("von-smol"); got != scheduler.IdlePause {
		t.Fatalf("IdleFor = %q, want pause", got)
	}
	if pr := s.ReadyFor("von-smol"); pr == nil || pr.Path != "/healthz" || pr.Timeout != 2*time.Second {
		t.Fatalf("ReadyFor = %+v", pr)
	}

	escribirRegistro(t, p, map[string]any{"smol": map[string]any{"kind": "http", "contract": "openai", "snapshot": "von-smol", "port": 9000}}, nil)
	if _, err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := s.PortFor("von-smol"); got != 9000 {
		t.Fatalf("PortFor after reload = %d, want 9000", got)
	}
	if got := s.MaxInflightFor("von-smol"); got != 0 {
		t.Fatalf("MaxInflightFor after reload = %d, want 0 (the gateway's)", got)
	}
	if got := s.IdleFor("von-smol"); got != scheduler.IdleRemove {
		t.Fatalf("IdleFor after reload = %q, want remove", got)
	}
	if pr := s.ReadyFor("von-smol"); pr == nil || pr.Path != "/health" {
		t.Fatalf("ReadyFor without health = %+v, want the contract's /health", pr)
	}
	if s.PortFor("otro") != 0 || s.MaxInflightFor("otro") != 0 || s.IdleFor("otro") != scheduler.IdleDefault || s.ReadyFor("otro") != nil {
		t.Fatal("a snapshot of no backend must get the zero values")
	}
}

// daemonCiclo es un daemon falso con lo justo para Start y Close: info,
// listado, congelar y borrar, apuntando qué se hizo con cada máquina.
type daemonCiclo struct {
	mu       sync.Mutex
	machines []*api.Machine
	frozen   []string
	removed  []string
}

func (d *daemonCiclo) serve(t *testing.T) *api.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "aigw") // corto: un socket Unix de macOS no pasa de 104 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	send := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) { send(w, api.Info{}) })
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		out := []*api.Machine{}
		for _, m := range d.machines {
			c := *m
			out = append(out, &c)
		}
		send(w, out)
	})
	find := func(ref string) *api.Machine {
		for _, m := range d.machines {
			if m.ID == ref {
				return m
			}
		}
		return nil
	}
	mux.HandleFunc("POST /machines/{ref}/freeze", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		m := find(r.PathValue("ref"))
		if m == nil {
			http.Error(w, `{"error":"no such machine"}`, 404)
			return
		}
		m.State = api.StateWarm
		d.frozen = append(d.frozen, m.ID)
		c := *m
		send(w, &c)
	})
	mux.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		ref := r.PathValue("ref")
		for i, m := range d.machines {
			if m.ID == ref {
				d.machines = append(d.machines[:i], d.machines[i+1:]...)
				d.removed = append(d.removed, ref)
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.Error(w, `{"error":"no such machine"}`, 404)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return api.NewClient(sock)
}

func (d *daemonCiclo) hecho() (frozen, removed []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	frozen, removed = append([]string(nil), d.frozen...), append([]string(nil), d.removed...)
	sort.Strings(frozen)
	sort.Strings(removed)
	return frozen, removed
}

func maquina(id, from, gw string, st api.State) *api.Machine {
	m := &api.Machine{ID: id, Name: "gw-" + id, State: st, From: from, Labels: map[string]string{}}
	if from != "" {
		m.Labels[api.LabelService] = from
	}
	if gw != "" {
		m.Labels[LabelGateway] = gw
	}
	return m
}

// Al arrancar se borran las réplicas con la etiqueta de ESTE gateway cuyo
// dorado ya no es de ningún modelo del registro (R8-06b), en cualquier estado;
// las de un modelo vigente siguen como hoy (la que corre se congela). Nada de
// otro gateway, de un humano o sin dorado conocido se toca.
func TestBarridoReplicasDeModeloRetirado(t *testing.T) {
	d := &daemonCiclo{machines: []*api.Machine{
		maquina("vieja-fria", "von-retirado", "default", api.StateWarm),
		maquina("vieja-viva", "von-retirado", "default", api.StateRunning),
		maquina("vigente", "von-smol", "default", api.StateRunning),
		maquina("otro-gw", "von-retirado", "otro", api.StateWarm),
		maquina("humano", "von-retirado", "", api.StateWarm),
		maquina("sin-dorado", "", "default", api.StateWarm),
	}}
	client := d.serve(t)
	cfg := &Config{Models: map[string]*ModelConfig{"smol": {Kind: KindVON, Snapshot: "von-smol", Idle: IdleFreeze}}, Tasks: map[string]*TaskConfig{}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Client: client, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	g.setConfig(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.Start(ctx)
	frozen, removed := d.hecho()
	if strings.Join(removed, ",") != "vieja-fria,vieja-viva" {
		t.Fatalf("removed %v, want the two replicas of the retired model", removed)
	}
	if strings.Join(frozen, ",") != "vigente" {
		t.Fatalf("frozen %v, want only the running replica of a registered model", frozen)
	}
}

// Al cerrar, una réplica de un modelo con idle remove se borra en vez de
// congelarse; la de un modelo freeze se congela como hoy.
func TestCierreBorraReplicasRemove(t *testing.T) {
	d := &daemonCiclo{machines: []*api.Machine{
		maquina("quitar", "von-smol", "default", api.StateRunning),
		maquina("congelar", "von-big", "default", api.StateRunning),
	}}
	client := d.serve(t)
	cfg := &Config{Models: map[string]*ModelConfig{
		"smol": {Kind: KindVON, Snapshot: "von-smol", Idle: IdleRemove},
		"big":  {Kind: KindVON, Snapshot: "von-big", Idle: IdleFreeze},
	}, Tasks: map[string]*TaskConfig{}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Client: client, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	g.setConfig(cfg)
	g.Close(context.Background())
	frozen, removed := d.hecho()
	if strings.Join(removed, ",") != "quitar" || strings.Join(frozen, ",") != "congelar" {
		t.Fatalf("removed %v frozen %v, want remove=quitar freeze=congelar", removed, frozen)
	}
}

// Al cerrar no se congela debajo de una petición que sigue en vuelo dentro de
// una réplica (una escalada, una auditoría): esa réplica se salta y la recoge
// el siguiente arranque (R8-03).
func TestCierreSaltaReplicasEnVuelo(t *testing.T) {
	fl := newFakeLlama(t)
	fl.entered, fl.block = make(chan struct{}, 1), make(chan struct{})
	soltar := sync.OnceFunc(func() { close(fl.block) })
	t.Cleanup(soltar) // un fallo no deja el llama falso bloqueado al cerrarlo
	d := &fakeDaemon{llama: strings.TrimPrefix(fl.srv.URL, "http://"), machines: map[string]*api.Machine{}}
	client := d.serve(t)
	cfg := &Config{
		Models: map[string]*ModelConfig{
			"commits": {Kind: KindChispa, Path: trainedModel(t)},
			// freeze: el test mira que Close no congele bajo una petición
			// en vuelo (con el defecto remove, no la borraría tampoco).
			"smol": {Kind: KindVON, Snapshot: "von-smol", Idle: IdleFreeze},
		},
		Tasks: map[string]*TaskConfig{"kind": {Chispa: "commits", EscalateTo: "smol", EscalateForce: true,
			Thresholds: map[string]float64{"bug": 2, "chore": 2, "docs": 2, "feat": 2}}},
	}
	g, err := New(Options{Client: client, Config: cfg, Idle: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h := g.Handler("")
	done := make(chan int, 1)
	go func() {
		done <- do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash"}).Code
	}()
	<-fl.entered
	g.Close(context.Background())
	if _, _, freezes, _ := d.counts(); freezes != 0 {
		t.Fatalf("Close froze %d replica(s) with a request in flight", freezes)
	}
	soltar()
	if c := <-done; c != 200 {
		t.Fatalf("request in flight during Close: %d", c)
	}
	g.Close(context.Background())
	if _, _, freezes, running := d.counts(); freezes != 1 || running != 0 {
		t.Fatalf("Close once idle: freezes=%d running=%d, want 1 and 0", freezes, running)
	}
}

// Un gateway arrancado en el socket sin token sigue atendiendo sin token a su
// dueño después de que un reload traiga inquilinos: el registro nuevo no le
// cierra el socket. Un inquilino sigue entrando solo con su token, y un token
// que no es de nadie es un 401. En TCP (-no-auth) no hay excepción.
func TestSocketSinTokenSobreviveAInquilinos(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ai.json")
	escribirRegistro(t, p, nil, nil)
	g, err := New(Options{ConfigPath: p, Replicas: &fakeReplicas{}})
	if err != nil {
		t.Fatal(err)
	}
	h := g.ServeHandler("", false)
	if rec := do(t, h, "GET", "/v1/models", "", nil); rec.Code != 200 {
		t.Fatalf("owner before reload: %d", rec.Code)
	}
	escribirRegistro(t, p, nil, []TenantConfig{{Name: "acme", Token: "tenant-acme-0123456789"}})
	if _, err := g.Reload(); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, "GET", "/v1/models", "", nil); rec.Code != 200 {
		t.Fatalf("socket owner without a token after tenants arrived: %d, want 200", rec.Code)
	}
	if rec := do(t, h, "GET", "/metrics", "", nil); rec.Code != 200 {
		t.Fatalf("socket owner on an admin route: %d, want 200", rec.Code)
	}
	if rec := do(t, h, "GET", "/metrics", "tenant-acme-0123456789", nil); rec.Code != 403 {
		t.Fatalf("tenant on an admin route: %d, want 403", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/models", "tenant-acme-0123456789", nil); rec.Code != 200 {
		t.Fatalf("tenant with its token: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/v1/models", "nadie-0123456789abcdef", nil); rec.Code != 401 {
		t.Fatalf("unknown token on the socket: %d, want 401", rec.Code)
	}
	tcp := g.ServeHandler("", true)
	if rec := do(t, tcp, "GET", "/v1/models", "", nil); rec.Code != 401 {
		t.Fatalf("TCP without a token once tenants exist: %d, want 401", rec.Code)
	}
}
