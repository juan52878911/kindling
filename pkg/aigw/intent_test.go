package aigw

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/chispa"
	"github.com/juan52878911/kindling/pkg/chispa/train"
	"github.com/juan52878911/kindling/pkg/codificador"
	"github.com/juan52878911/kindling/pkg/intent"
)

// Una tarea de intención diminuta, de tickets de soporte: Chispa sabe «abre /
// cierra un ticket …» y todo lo demás es fuera de ámbito; el codificador
// falso pone «broken» cerca de open_ticket.

const testOOS = "other"

const ticketSchemaJSON = `{
  "out_of_scope": "other",
  "lang": "en",
  "intents": [
    {"name": "open_ticket", "slots": ["queue"]},
    {"name": "close_ticket", "slots": ["queue"]}
  ],
  "values": {"queue": {"billing": ["invoices"], "support": ["help desk"]}},
  "templates": [{"text": "open a ticket", "intent": "open_ticket"}]
}`

func ticketSchema(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tickets.json")
	if err := os.WriteFile(p, []byte(ticketSchemaJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func ticketChispa(t *testing.T) string {
	t.Helper()
	var exs []chispa.Example
	things := []string{"the printer", "the laptop", "the vpn", "the badge", "the monitor", "the phone"}
	for i := 0; i < 240; i++ {
		th := things[i%len(things)]
		switch i % 3 {
		case 0:
			exs = append(exs, chispa.Example{Text: "open a ticket for " + th, Label: "open_ticket"})
		case 1:
			exs = append(exs, chispa.Example{Text: "close the ticket of " + th, Label: "close_ticket"})
		default:
			exs = append(exs, chispa.Example{Text: []string{"what time is it", "tell me a joke", "book a flight", "call my mother"}[i%4] + " " + th, Label: testOOS})
		}
	}
	cfg := train.Config{Spec: chispa.DefaultSpec(), Seed: 1}
	cfg.Spec.Buckets = 1 << 12
	res, err := train.Train(exs, exs[:60], cfg)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "intent.chispa")
	if err := res.Model.Save(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// vecFor es el «codificador»: tres direcciones, una por clase.
func vecFor(text string) []float32 {
	switch {
	case strings.Contains(text, "broken"):
		return []float32{0, 0, 1, 0.1}
	case strings.Contains(text, "close"):
		return []float32{0, 1, 0, 0.1}
	}
	return []float32{1, 0, 0, 0.1}
}

func ticketHead(t *testing.T) string {
	t.Helper()
	var ds codificador.Dataset
	for i := 0; i < 90; i++ {
		v := vecFor([]string{"x", "close", "broken"}[i%3])
		v[3] = float32(i%7) / 10 // algo de ruido
		ds.X, ds.Y = append(ds.X, v), append(ds.Y, i%3)
	}
	res, err := codificador.Train([]string{testOOS, "close_ticket", "open_ticket"}, ds, ds,
		codificador.TrainConfig{Epochs: 30, TargetPrecision: 0.9, MinSupport: 3, Meta: codificador.Meta{Encoder: "fake:q8_0", Prefix: "query: "}})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "head.jenc")
	if err := res.Head.Save(p); err != nil {
		t.Fatal(err)
	}
	return p
}

func newIntentGateway(t *testing.T, force bool) (*Gateway, *fakeReplicas, *atomic.Int64) {
	t.Helper()
	calls := new(atomic.Int64)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var b struct{ Input []string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		var data []any
		for i, s := range b.Input {
			if !strings.HasPrefix(s, "query: ") {
				t.Errorf("the head's prefix is missing: %q", s)
			}
			data = append(data, map[string]any{"index": i, "embedding": vecFor(s)})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	reps := &fakeReplicas{addr: strings.TrimPrefix(srv.URL, "http://")}
	cfg := &Config{
		Models: map[string]*ModelConfig{
			"intent": {Kind: KindChispa, Path: ticketChispa(t)},
			"enc":    {Kind: KindEmbed, Snapshot: "enc-e5"},
		},
		Tasks: map[string]*TaskConfig{
			"tickets": {Intent: &IntentConfig{Model: "intent", Schema: ticketSchema(t), Encoder: "enc", Head: ticketHead(t), EncoderForce: force}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Config: cfg, Replicas: reps, EvalDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return g, reps, calls
}

func TestIntentConfig(t *testing.T) {
	jv := &ModelConfig{Kind: KindChispa, Path: "x.chispa"}
	emb := &ModelConfig{Kind: KindEmbed, Snapshot: "enc-e5"}
	for name, task := range map[string]*TaskConfig{
		"chispa too":        {Chispa: "intent", Intent: &IntentConfig{Model: "intent", Schema: "s.json"}},
		"not chispa":        {Intent: &IntentConfig{Model: "enc", Schema: "s.json"}},
		"no domain":         {Intent: &IntentConfig{Model: "intent"}},
		"domain and schema": {Intent: &IntentConfig{Model: "intent", Schema: "s.json", Domain: "d"}},
		"head alone":        {Intent: &IntentConfig{Model: "intent", Schema: "s.json", Head: "h.jenc"}},
		"encoder is chispa": {Intent: &IntentConfig{Model: "intent", Schema: "s.json", Encoder: "intent", Head: "h.jenc"}},
		"force alone":       {Intent: &IntentConfig{Model: "intent", Schema: "s.json", EncoderForce: true}},
	} {
		c := &Config{Models: map[string]*ModelConfig{"intent": jv, "enc": emb}, Tasks: map[string]*TaskConfig{"t": task}}
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (&Config{Models: map[string]*ModelConfig{"e": {Kind: KindEmbed}}}).Validate(); err == nil {
		t.Error("embed model without snapshot accepted")
	}
}

func TestIntentDecideAndGate(t *testing.T) {
	g, reps, calls := newIntentGateway(t, false)
	h := g.Handler("")
	decide := func(text string) DecideResponse {
		rec := do(t, h, http.MethodPost, "/v1/decide", "", map[string]any{"task": "tickets", "text": text})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", text, rec.Code, rec.Body)
		}
		return decode[DecideResponse](t, rec)
	}

	// La plantilla contesta sin modelos.
	if d := decide("Open a ticket!"); d.Layer != intent.LayerTemplate || !d.Confident || d.Label != "open_ticket" || d.Lang != "en" {
		t.Fatalf("template: %+v", d)
	}

	// Sin evaluación la capa 3 está rechazada: no se llama al codificador.
	d := decide("my screen is broken again")
	if d.Encoder == nil || d.Encoder.Status != "refused" || calls.Load() != 0 || d.Escalate != intent.EscalateTo {
		t.Fatalf("without an eval: %+v (%d calls)", d, calls.Load())
	}
	if rec := do(t, h, http.MethodPost, "/v1/classify", "", map[string]any{"task": "tickets", "text": "x"}); rec.Code != 400 {
		t.Fatalf("classify on an intent task: %d", rec.Code)
	}
	if rec := do(t, h, http.MethodPost, "/v1/decide", "", map[string]any{"task": "tickets", "text": "x", "lang": "../x"}); rec.Code != 400 {
		t.Fatalf("a bad lang: %d", rec.Code)
	}

	// La evaluación: la capa 3 contesta bien lo roto, que Chispa no sabe.
	rows := []intent.Row{}
	for i := 0; i < 12; i++ {
		rows = append(rows,
			intent.Row{Text: "my screen is broken again, case " + string(rune('a'+i)), Intent: "open_ticket", Slots: json.RawMessage(`{}`)},
			intent.Row{Text: "open a ticket for the printer", Intent: "open_ticket"})
	}
	bad := append([]intent.Row{{Text: "x", Intent: "open_ticket", Slots: json.RawMessage(`[1]`)}}, rows...)
	if rec := do(t, h, http.MethodPost, "/v1/admin/eval", "", EvalRequest{Task: "tickets", Rows: bad}); rec.Code != 400 {
		t.Fatalf("rows with bad slots: %d %s", rec.Code, rec.Body)
	}
	rec := do(t, h, http.MethodPost, "/v1/admin/eval", "", EvalRequest{Task: "tickets", Data: "t.jsonl", Rows: rows})
	if rec.Code != 200 {
		t.Fatalf("eval: %d %s", rec.Code, rec.Body)
	}
	out := decode[struct {
		Record  IntentEvalRecord `json:"record"`
		Cascade CascadeState     `json:"cascade"`
	}](t, rec)
	r := out.Record.Results
	if !out.Record.BeatsFast || out.Cascade.Status != "on" || r.CascadeOnlyRight != 12 || r.FastOnlyRight != 0 || r.ToEncoder != 12 ||
		!strings.HasPrefix(out.Record.Domain, "schema:") {
		t.Fatalf("eval: %+v %+v", out.Record, out.Cascade)
	}

	// Encendida: lo roto lo contesta el codificador.
	d = decide("my screen is broken again")
	if !d.Confident || d.Layer != intent.LayerEncoder || d.Label != "open_ticket" || d.Encoder.Status != "on" || reps.acquired.Load() == 0 {
		t.Fatalf("with the encoder: %+v", d)
	}
	// Cambiar la cabeza invalida la evaluación (la puerta mira su hash).
	if err := os.WriteFile(g.config().Tasks["tickets"].Intent.Head, []byte("otra cabeza"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.regate()
	if st := g.cascade("tickets"); st.Status != "refused" || !strings.Contains(st.Reason, "changed") {
		t.Fatalf("a changed head must refuse: %+v", st)
	}

	// Réplica caída con la capa forzada: escala a VON diciendo por qué.
	g2, reps2, _ := newIntentGateway(t, true)
	reps2.fail = errors.New("no capacity")
	rec = do(t, g2.Handler(""), http.MethodPost, "/v1/decide", "", map[string]any{"task": "tickets", "text": "the monitor is broken"})
	d = decode[DecideResponse](t, rec)
	if d.Confident || d.Escalate != intent.EscalateVON || d.EncoderError == "" || d.Encoder.Status != "forced" {
		t.Fatalf("encoder down: %+v", d)
	}
}

// Un dominio en Go lo registra el programa que embebe el gateway; uno que no
// está registrado no decide (503) y lo dice.
func TestIntentGoDomain(t *testing.T) {
	sch, err := intent.LoadSchema(ticketSchema(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		Models: map[string]*ModelConfig{"intent": {Kind: KindChispa, Path: ticketChispa(t)}},
		Tasks: map[string]*TaskConfig{
			"go":      {Intent: &IntentConfig{Model: "intent", Domain: "tickets"}},
			"missing": {Intent: &IntentConfig{Model: "intent", Domain: "nope"}},
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	g, err := New(Options{Config: cfg, Replicas: &fakeReplicas{}, EvalDir: t.TempDir(), Domains: map[string]intent.Domain{"tickets": sch}})
	if err != nil {
		t.Fatal(err)
	}
	h := g.Handler("")
	rec := do(t, h, http.MethodPost, "/v1/decide", "", map[string]any{"task": "go", "text": "open a ticket for the invoices"})
	d := decode[DecideResponse](t, rec)
	if rec.Code != 200 || d.Label != "open_ticket" || d.Layer != intent.LayerChispa {
		t.Fatalf("go domain: %d %+v", rec.Code, d)
	}
	rec = do(t, h, http.MethodPost, "/v1/decide", "", map[string]any{"task": "missing", "text": "x"})
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "not built into this gateway") {
		t.Fatalf("missing domain: %d %s", rec.Code, rec.Body)
	}
	rec = do(t, h, http.MethodGet, "/v1/tasks", "", nil)
	if !strings.Contains(rec.Body.String(), `"kind":"intent"`) {
		t.Fatalf("tasks: %s", rec.Body)
	}
}
