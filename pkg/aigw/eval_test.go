package aigw

import (
	"context"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/chispa"
)

// ejemplos pasa el corpus sintético a ejemplos de evaluación; con relabel, todos
// llevan esa etiqueta (Chispa, que ve el texto, se equivoca en casi todos).
func ejemplos(n int, seed uint64, relabel string) []EvalExample {
	var out []EvalExample
	for _, e := range corpus(n, seed) {
		l := e.Label
		if relabel != "" {
			l = relabel
		}
		out = append(out, EvalExample{Text: e.Text, Label: l})
	}
	return out
}

// La puerta de la cascada: escalate_to sin evaluación se rechaza (Chispa contesta
// con escalate: true y VON no se toca); una evaluación en la que la cascada no
// gana la deja rechazada; una en la que gana la activa; cambiar lo evaluado
// (ajustes, modelo Chispa) la vuelve a rechazar; escalate_force la activa igual.
func TestPuertaDeLaCascada(t *testing.T) {
	g, ll, _ := newTestGateway(t, func(c *Config) {
		c.Tasks["kind"] = &TaskConfig{Chispa: "commits", EscalateTo: "smol",
			Thresholds: map[string]float64{"bug": 2, "chore": 2, "docs": 2, "feat": 2}}
	})
	h := g.Handler("")
	ask := func() ClassifyResponse {
		return decode[ClassifyResponse](t, do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "kind", "text": "crash panic segfault"}))
	}

	st := g.cascade("kind")
	if st.Status != "refused" || !strings.Contains(st.Reason, "kling ai eval kind") || !strings.Contains(st.Reason, "escalate_force") {
		t.Fatalf("without an eval = %+v", st)
	}
	if r := ask(); r.Source != "chispa" || !r.Escalate || ll.calls.Load() != 0 {
		t.Fatalf("refused cascade answered %+v, von calls %d", r, ll.calls.Load())
	}

	// VON contesta "feat" a todo en datos bien etiquetados: Chispa gana.
	ll.set("feat")
	rec, err := g.Eval(context.Background(), EvalRequest{Task: "kind", Examples: ejemplos(40, 3, ""), Data: "test.jsonl"})
	if err != nil {
		t.Fatal(err)
	}
	res := rec.Results
	if rec.BeatsChispa || res.ChispaAccuracy < 0.9 || res.Escalated != 40 || res.ChispaOnlyRight < 25 || rec.Stored == "" {
		t.Fatalf("losing eval = %+v", rec)
	}
	if st := g.cascade("kind"); st.Status != "refused" || !strings.Contains(st.Reason, "does not show") {
		t.Fatalf("after a losing eval = %+v", st)
	}

	// Datos en los que Chispa se equivoca y VON acierta: la cascada gana.
	ll.set("docs")
	calls := ll.calls.Load()
	rec, err = g.Eval(context.Background(), EvalRequest{Task: "kind", Examples: ejemplos(40, 4, "docs"), Concurrency: 4})
	if err != nil {
		t.Fatal(err)
	}
	res = rec.Results
	if !rec.BeatsChispa || res.CascadeAccuracy != 1 || res.PValue > 0.001 || ll.calls.Load()-calls != 40 {
		t.Fatalf("winning eval = %+v (von calls %d)", rec, ll.calls.Load()-calls)
	}
	if st := g.cascade("kind"); st.Status != "on" || !strings.Contains(st.Verdict, "the cascade wins") {
		t.Fatalf("after a winning eval = %+v", st)
	}
	if r := ask(); r.Source != "von" || r.Label != "docs" || !r.Escalate {
		t.Fatalf("active cascade answered %+v", r)
	}
	on, _ := os.ReadFile(g.evalPath("kind"))
	if !strings.Contains(string(on), `"beats_chispa": true`) {
		t.Fatalf("stored record = %s", on)
	}

	// Otra pregunta a VON no es lo que se evaluó.
	g.config().Tasks["kind"].Prompt = "Classify: {text}"
	g.regate()
	if st := g.cascade("kind"); st.Status != "refused" || !strings.Contains(st.Reason, "settings") {
		t.Fatalf("after changing the prompt = %+v", st)
	}
	g.config().Tasks["kind"].Prompt = ""
	g.regate()
	if st := g.cascade("kind"); st.Status != "on" {
		t.Fatalf("prompt restored = %+v", st)
	}

	// Ni otro modelo Chispa (reentrenado o recalibrado).
	path := g.config().Models["commits"].Path
	m, err := chispa.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m.Meta.Notes += "retrained"
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	g.regate()
	if st := g.cascade("kind"); st.Status != "refused" || !strings.Contains(st.Reason, "chispa model changed") {
		t.Fatalf("after retraining = %+v", st)
	}

	// escalate_force: activa, y dicho.
	g.config().Tasks["kind"].EscalateForce = true
	notes := g.regate()
	if st := g.cascade("kind"); st.Status != "forced" || !st.On() {
		t.Fatalf("forced = %+v", st)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "forced") {
		t.Fatalf("notes = %q", notes)
	}

	// En seco no se guarda nada; sin candidato, error claro.
	if rec, err := g.Eval(context.Background(), EvalRequest{Task: "kind", Examples: ejemplos(4, 5, ""), DryRun: true}); err != nil || rec.Stored != "" {
		t.Fatalf("dry run: %+v %v", rec, err)
	}
	g.config().Tasks["kind"].EscalateTo = ""
	if _, err := g.Eval(context.Background(), EvalRequest{Task: "kind", Examples: ejemplos(4, 5, "")}); err == nil || !strings.Contains(err.Error(), "-von") {
		t.Fatalf("eval without a candidate: %v", err)
	}
	if _, err := g.Eval(context.Background(), EvalRequest{Task: "kind", VON: "smol", Examples: ejemplos(4, 5, ""), VONAlone: true, DryRun: true}); err != nil {
		t.Fatalf("eval with -von: %v", err)
	}
}

// Una tarea de generación: plantilla con {input} y variables en un solo pase,
// max_tokens acotado por el de la tarea, semilla del host, sin gramática.
func TestGenerar(t *testing.T) {
	g, ll, _ := newTestGateway(t, func(c *Config) {
		c.Tasks["sum"] = &TaskConfig{VON: "smol", System: "Be brief.", Prompt: "Summarize in {lang}:\n{input}", MaxTokens: 64}
	})
	h := g.Handler("")
	ll.set("a short summary")
	rec := do(t, h, "POST", "/v1/generate", "", map[string]any{"task": "sum", "input": "long text {lang}", "vars": map[string]string{"lang": "Spanish"}})
	if rec.Code != 200 {
		t.Fatalf("generate: %d %s", rec.Code, rec.Body)
	}
	r := decode[GenerateResponse](t, rec)
	if r.Output != "a short summary" || r.Model != "smol" {
		t.Fatalf("generate = %+v", r)
	}
	body := ll.last()
	msgs, _ := body["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["content"] != "Be brief." ||
		msgs[1].(map[string]any)["content"] != "Summarize in Spanish:\nlong text {lang}" {
		t.Fatalf("messages = %v", msgs)
	}
	if body["max_tokens"] != 64.0 || body["temperature"] != 0.7 || body["grammar"] != nil || body["seed"] == nil {
		t.Fatalf("request to VON = %v", body)
	}
	do(t, h, "POST", "/v1/generate", "", map[string]any{"task": "sum", "input": "x", "max_tokens": 10, "temperature": 0})
	if b := ll.last(); b["max_tokens"] != 10.0 || b["temperature"] != 0.0 {
		t.Fatalf("overrides = %v", b)
	}
	for _, bad := range []map[string]any{
		{"task": "sum", "input": "x", "max_tokens": 65},
		{"task": "sum", "input": "x", "temperature": 3},
		{"task": "sum", "input": "x", "vars": map[string]string{"input": "y"}},
		{"task": "kind", "input": "x"},
	} {
		if rec := do(t, h, "POST", "/v1/generate", "", bad); rec.Code != 400 {
			t.Errorf("%v: %d %s", bad, rec.Code, rec.Body)
		}
	}
	if rec := do(t, h, "POST", "/v1/classify", "", map[string]any{"task": "sum", "text": "x"}); rec.Code != 400 {
		t.Errorf("classify on a generation task: %d", rec.Code)
	}
	if !strings.Contains(do(t, h, "GET", "/metrics", "", nil).Body.String(), `kling_ai_requests_total{endpoint="generate",task="sum",source="von"} 2`) {
		t.Error("generations not counted")
	}
}

func TestMcNemar(t *testing.T) {
	for _, c := range []struct {
		b, c int
		p    float64
	}{{0, 5, 1.0 / 32}, {5, 5, 0.623046875}, {0, 0, 1}, {10, 0, 1}} {
		if got := mcnemarOneSided(c.b, c.c); math.Abs(got-c.p) > 1e-9 {
			t.Errorf("mcnemar(%d,%d) = %g, want %g", c.b, c.c, got, c.p)
		}
	}
	// Miles de discrepancias no desbordan.
	if p := mcnemarOneSided(3000, 3200); !(p > 0 && p < 0.01) {
		t.Errorf("large n p = %g", p)
	}
}
