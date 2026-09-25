package domotica

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseLLMValid(t *testing.T) {
	d := ParseLLM(`{"kind":"command","reply":"Hecho.","actions":[
		{"intent":"turn_off","device":"light","area":"kitchen","value":null,"color":null},
		{"intent":"lock","device":null,"area":null,"value":null,"color":null}]}`, "apaga la luz de la cocina y cierra la puerta", "es")
	if !d.Confident || len(d.Actions) != 2 || d.Reply != "Hecho." || d.Kind != KindCommand {
		t.Fatalf("got %+v", d)
	}
	if d.Actions[0].Slots.Area != "kitchen" || d.Actions[1].Slots.Device != DevLock {
		t.Fatalf("slots not resolved: %+v", d.Actions)
	}
	if d.Intent != "turn_off" {
		t.Fatalf("first action is the decision's intent, got %s", d.Intent)
	}
}

func TestParseLLMNothingToDo(t *testing.T) {
	d := ParseLLM(`{"kind":"other","reply":"","actions":[]}`, "cuéntame un chiste", "es")
	if !d.Confident || d.Intent != OutOfScope || len(d.ActionList()) != 0 || d.Reply == "" {
		t.Fatalf("got %+v", d)
	}
}

func TestParseLLMInvalid(t *testing.T) {
	cases := map[string]string{
		"unknown intent":     `{"kind":"command","reply":"","actions":[{"intent":"make_coffee","device":null,"area":null,"value":null,"color":null}]}`,
		"unknown field":      `{"kind":"command","reply":"","actions":[],"extra":1}`,
		"trailing":           `{"kind":"other","reply":"","actions":[]} {"x":1}`,
		"value out of range": `{"kind":"command","reply":"","actions":[{"intent":"set_temperature","device":null,"area":null,"value":90,"color":null}]}`,
		"missing value":      `{"kind":"command","reply":"","actions":[{"intent":"set_temperature","device":null,"area":null,"value":null,"color":null}]}`,
		"wrong device":       `{"kind":"command","reply":"","actions":[{"intent":"cover_open","device":"tv","area":null,"value":null,"color":null}]}`,
		"unknown area":       `{"kind":"command","reply":"","actions":[{"intent":"turn_on","device":null,"area":"moon","value":null,"color":null}]}`,
		"other with action":  `{"kind":"other","reply":"","actions":[{"intent":"turn_on","device":null,"area":null,"value":null,"color":null}]}`,
		"command, none":      `{"kind":"command","reply":"","actions":[]}`,
		"unknown kind":       `{"kind":"maybe","reply":"","actions":[]}`,
		"not json":           `turn on the light`,
		"too many": `{"kind":"command","reply":"","actions":[` + strings.Repeat(`{"intent":"turn_on","device":null,"area":null,"value":null,"color":null},`, 4) +
			`{"intent":"turn_on","device":null,"area":null,"value":null,"color":null}]}`,
	}
	for name, c := range cases {
		d := ParseLLM(c, "enciende la luz", "en")
		if d.Confident || d.Reason != ReasonInvalidOutput || d.Intent != OutOfScope || len(d.ActionList()) != 0 || d.Reply == "" {
			t.Errorf("%s: got %+v", name, d)
		}
	}
}

func TestParseLLMGrounding(t *testing.T) {
	// Zona inventada: se quita.
	d := ParseLLM(`{"kind":"situation","reply":"","actions":[{"intent":"turn_off","device":"light","area":"bedroom","value":null,"color":null}]}`, "me voy a dormir", "es")
	if !d.Confident || d.Actions[0].Slots.Area != "" {
		t.Fatalf("ungrounded area kept: %+v", d)
	}
	// Zona nombrada: se queda.
	d = ParseLLM(`{"kind":"situation","reply":"","actions":[{"intent":"temperature_down","device":null,"area":"living_room","value":null,"color":null}]}`, "hace calor en el salón", "es")
	if d.Actions[0].Slots.Area != "living_room" {
		t.Fatalf("grounded area dropped: %+v", d)
	}
	// Un color o un número que la frase no dice: se pregunta.
	d = ParseLLM(`{"kind":"command","reply":"","actions":[{"intent":"set_color","device":null,"area":null,"value":null,"color":"blue"}]}`, "cambia el color de la luz", "es")
	if d.Confident {
		t.Fatalf("invented color accepted: %+v", d)
	}
	d = ParseLLM(`{"kind":"command","reply":"","actions":[{"intent":"set_color","device":null,"area":null,"value":null,"color":"blue"}]}`, "pon las luces azules", "es")
	if !d.Confident {
		t.Fatalf("named color rejected: %+v", d)
	}
	d = ParseLLM(`{"kind":"command","reply":"","actions":[{"intent":"cover_set_position","device":null,"area":null,"value":50,"color":null}]}`, "baja la persiana", "es")
	if d.Confident {
		t.Fatalf("invented value accepted: %+v", d)
	}
	d = ParseLLM(`{"kind":"command","reply":"","actions":[{"intent":"cover_set_position","device":null,"area":null,"value":50,"color":null}]}`, "pon la persiana a la mitad", "es")
	if !d.Confident {
		t.Fatalf("«mitad» is a value: %+v", d)
	}
	// Abrir la puerta sin nombrarla: nada.
	d = ParseLLM(`{"kind":"situation","reply":"","actions":[{"intent":"unlock","device":null,"area":null,"value":null,"color":null}]}`, "ya he llegado", "es")
	if d.Confident {
		t.Fatalf("unlock without naming the door accepted: %+v", d)
	}
	d = ParseLLM(`{"kind":"command","reply":"","actions":[{"intent":"unlock","device":null,"area":null,"value":null,"color":null}]}`, "abre la puerta", "es")
	if !d.Confident {
		t.Fatalf("explicit unlock rejected: %+v", d)
	}
}

func TestLLMSchema(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal(LLMSchema, &v); err != nil {
		t.Fatal(err)
	}
	s := string(LLMSchema)
	// El orden importa (llama-server genera en ese orden): kind, reply, actions.
	if !(strings.Index(s, `"kind"`) < strings.Index(s, `"reply"`) && strings.Index(s, `"reply"`) < strings.Index(s, `"actions"`)) {
		t.Fatalf("property order: %s", s)
	}
	if strings.Contains(s, `"`+OutOfScope+`"`) {
		t.Fatal("out_of_scope is not an action")
	}
	for _, it := range Intents {
		if it.Name != OutOfScope && !strings.Contains(s, `"`+it.Name+`"`) {
			t.Fatalf("intent %s missing from the schema", it.Name)
		}
	}
	if PromptID() == "" || !strings.Contains(LLMSystemPrompt, "temperature_up") {
		t.Fatal("prompt")
	}
}

func fakeLLM(t *testing.T, content string, check func(map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &req); err != nil {
			t.Errorf("request: %v", err)
		}
		if check != nil {
			check(req)
		}
		out, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
			"usage":   map[string]any{"prompt_tokens": 900, "completion_tokens": 40},
			"timings": map[string]any{"cache_n": 880, "prompt_ms": 12.5, "predicted_ms": 300},
		})
		_, _ = w.Write(out)
	}))
}

func TestVONLayer(t *testing.T) {
	srv := fakeLLM(t, `{"kind":"situation","reply":"Subo la temperatura.","actions":[{"intent":"temperature_up","device":null,"area":null,"value":null,"color":null}]}`,
		func(req map[string]any) {
			if req["json_schema"] == nil || req["temperature"] != 0.0 || req["model"] != "m" {
				t.Errorf("request without schema/temperature/model: %v", req)
			}
			msgs := req["messages"].([]any)
			if msgs[0].(map[string]any)["content"] != LLMSystemPrompt {
				t.Error("system prompt must be the fixed one (prefix cache)")
			}
		})
	defer srv.Close()
	v := &VON{Endpoint: srv.URL, Model: "m"}
	d, err := v.Decide(context.Background(), "aquí hace frío", "es")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Confident || d.Intent != "temperature_up" || d.Layer != LayerVON || d.LLM == nil || d.LLM.CachedTokens != 880 {
		t.Fatalf("got %+v", d)
	}

	// JEV dio «fuera de ámbito» a una frase en imperativo: el LLM no la
	// convierte en una orden de la habitación.
	srv2 := fakeLLM(t, `{"kind":"command","reply":"Alarma puesta.","actions":[{"intent":"alarm_arm","device":null,"area":null,"value":null,"color":null}]}`, nil)
	defer srv2.Close()
	v2 := &VON{Endpoint: srv2.URL}
	prev := Decision{Layer: LayerJEV, Intent: OutOfScope, Reason: ReasonOutOfScope}
	d, err = v2.Decide(WithPrev(context.Background(), prev), "pon una alarma a las siete", "es")
	if err != nil || !d.Confident || len(d.ActionList()) != 0 || d.Reason != ReasonVetoedByJEV {
		t.Fatalf("veto: %+v %v", d, err)
	}
	// Una situación sin verbo de orden (lo indirecto) pasa; si el LLM la
	// llama orden, no.
	d, _ = v2.Decide(WithPrev(context.Background(), prev), "me voy de casa", "es")
	if len(d.ActionList()) != 0 {
		t.Fatalf("kind command on a JEV out-of-scope phrase must be vetoed: %+v", d)
	}
	srv3 := fakeLLM(t, `{"kind":"situation","reply":"Activo la alarma.","actions":[{"intent":"alarm_arm","device":null,"area":null,"value":null,"color":null}]}`, nil)
	defer srv3.Close()
	d, _ = (&VON{Endpoint: srv3.URL}).Decide(WithPrev(context.Background(), prev), "me voy de casa", "es")
	if len(d.ActionList()) != 1 {
		t.Fatalf("indirect vetoed: %+v", d)
	}
}

func TestVONLayerErrors(t *testing.T) {
	if _, err := (&VON{}).Decide(context.Background(), "x", "es"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no endpoint: %v", err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no replica", http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	if _, err := (&VON{Endpoint: bad.URL}).Decide(context.Background(), "x", "es"); err == nil {
		t.Fatal("503 must be an error")
	}
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", maxLLMBody+10)))
	}))
	defer huge.Close()
	if _, err := (&VON{Endpoint: huge.URL}).Decide(context.Background(), "x", "es"); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("unbounded body: %v", err)
	}
}

func TestCascade(t *testing.T) {
	m, err := NewMatcher(DemoTemplates)
	if err != nil {
		t.Fatal(err)
	}
	fast := &Decider{Matcher: m} // sin JEV: lo que no es de la demo escala
	var called []string
	mock := func(name string, d Decision, err error) Layer {
		return LayerFunc(func(ctx context.Context, text, lang string) (Decision, error) {
			called = append(called, name)
			if _, ok := PrevFrom(ctx); !ok {
				t.Errorf("%s: no previous decision in the context", name)
			}
			return d, err
		})
	}
	von := Decision{Confident: true, Intent: "temperature_up", Actions: []Action{{Intent: "temperature_up", Slots: Slots{Device: DevThermostat}}}}
	c := &Cascade{Fast: fast, Slow: []NamedLayer{
		{Name: LayerEncoder, Layer: mock("encoder", Decision{}, ErrUnavailable)},
		{Name: LayerVON, Layer: mock("von", von, nil)},
	}}

	tr := c.Decide(context.Background(), "enciende la luz del salón", "es")
	if tr.Decided != LayerTemplate || len(tr.Steps) != 1 || len(called) != 0 || len(tr.Actions) != 1 {
		t.Fatalf("template: %+v called=%v", tr, called)
	}
	tr = c.Decide(context.Background(), "aquí hace frío", "es")
	if tr.Decided != LayerVON || len(tr.Steps) != 3 || tr.Steps[1].Status != StepUnavailable || tr.Actions[0].Intent != "temperature_up" {
		t.Fatalf("von: %+v", tr)
	}

	// Capa 4 apagada por su evaluación: no hace nada, y la traza lo dice.
	called = nil
	c.Slow[1].Layer = Disabled
	tr = c.Decide(context.Background(), "aquí hace frío", "es")
	if tr.Decided != "none" || len(tr.Actions) != 0 || tr.Steps[2].Status != StepDisabled {
		t.Fatalf("disabled: %+v", tr)
	}

	// Una capa que salta las órdenes múltiples.
	c.Slow[0].Skip = func(prev Decision) bool { return true }
	tr = c.Decide(context.Background(), "aquí hace frío", "es")
	if tr.Steps[1].Status != StepSkipped {
		t.Fatalf("skip: %+v", tr)
	}
	// Un error de una capa no para la cascada.
	c.Slow[0] = NamedLayer{Name: LayerEncoder, Layer: mock("encoder", Decision{}, errors.New("boom"))}
	c.Slow[1].Layer = mock("von", von, nil)
	tr = c.Decide(context.Background(), "aquí hace frío", "es")
	if tr.Steps[1].Status != StepError || tr.Decided != LayerVON {
		t.Fatalf("error: %+v", tr)
	}
}

func TestActionsMatch(t *testing.T) {
	g := []Action{act("turn_off", DevLight, "", nil, ""), act("lock", DevLock, "", nil, "")}
	p := []Action{act("lock", "", "", nil, ""), act("turn_off", "", "", nil, "")}
	if !ActionsMatch(g, p) {
		t.Fatal("order and implicit devices must not matter")
	}
	if ActionsMatch(g, p[:1]) || ActionsMatch(nil, p) {
		t.Fatal("different lengths")
	}
	// Multimedia sin dispositivo en el oro: cualquier reproductor.
	if !ActionsMatch([]Action{act("media_pause", "", "", nil, "")}, []Action{act("media_pause", DevTV, "", nil, "")}) {
		t.Fatal("media device leniency")
	}
	if ActionsMatch([]Action{act("turn_on", DevLight, "kitchen", nil, "")}, []Action{act("turn_on", DevLight, "", nil, "")}) {
		t.Fatal("area must match")
	}
}

func TestMcNemar(t *testing.T) {
	if p := mcnemarOneSided(0, 0); p != 1 {
		t.Fatal(p)
	}
	if p := mcnemarOneSided(10, 0); p > 0.001 {
		t.Fatal(p)
	}
	if p := mcnemarOneSided(5, 5); p < 0.5 {
		t.Fatal(p)
	}
}

func TestGateWeighsFalseActions(t *testing.T) {
	on := []Action{act("turn_on", DevLight, "", nil, "")}
	r := &Layer4Report{Groups: map[string]*L4Group{
		"in":  {N: 20, Weight: 1, Escalated: 20},
		"oos": {N: 50, Weight: 20, Escalated: 50, NothingOK: 50},
	}}
	// 15 órdenes que JEV dudaba y VON acierta.
	for i := 0; i < 15; i++ {
		r.Rows = append(r.Rows, L4Result{Group: "in", Gold: on, Pred: on, OK: true, FastReason: ReasonLowProb})
	}
	// En la muestra de lo que no es de la habitación (peso 20), actúa en 2.
	for i := 0; i < 50; i++ {
		row := L4Result{Group: "oos", Gold: []Action{}, Pred: []Action{}, OK: true, FastReason: ReasonOutOfScope}
		if i < 2 {
			row.Pred, row.OK = on, false
		}
		r.Rows = append(r.Rows, row)
	}
	if g := r.Gate(nil, ScopeAll); g.P >= 0.05 || g.Pass || g.Win != 15 || g.Loss != 2 {
		t.Fatalf("15 wins beat 2 losses, but 2 losses weigh 40 rows: %+v", g)
	}
	// Sin llamar a VON en lo que JEV da por fuera de ámbito, pasa.
	if g := r.Gate(nil, ScopeUncertain); !g.Pass || g.Loss != 0 || g.Total.WithWrong != 0 {
		t.Fatalf("uncertain scope: %+v", g)
	}
	r.Groups["oos"].Weight = 1
	if g := r.Gate(nil, ScopeAll); !g.Pass {
		t.Fatalf("unweighted it should pass: %+v", g)
	}
}
