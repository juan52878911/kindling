package domotica

import (
	"context"
	"encoding/json"
	"errors"
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

func fakeGen(out string, err error, check func(string)) Generator {
	return func(_ context.Context, input string) (string, string, error) {
		if check != nil {
			check(input)
		}
		return out, "m", err
	}
}

func TestVONLayer(t *testing.T) {
	v := &VON{Generate: fakeGen(`{"kind":"situation","reply":"Subo la temperatura.","actions":[{"intent":"temperature_up","device":null,"area":null,"value":null,"color":null}]}`, nil,
		func(in string) {
			if in != "Request: aquí hace frío" {
				t.Errorf("input %q", in)
			}
		})}
	d, err := v.Decide(context.Background(), "aquí hace frío", "es")
	if err != nil {
		t.Fatal(err)
	}
	if !d.Confident || d.Intent != "temperature_up" || d.Layer != LayerVON || d.Model != "m" || d.LLM == nil {
		t.Fatalf("got %+v", d)
	}

	// Chispa dio «fuera de ámbito» a una frase en imperativo: el LLM no la
	// convierte en una orden de la habitación.
	cmd := `{"kind":"command","reply":"Alarma puesta.","actions":[{"intent":"alarm_arm","device":null,"area":null,"value":null,"color":null}]}`
	v2 := &VON{Generate: fakeGen(cmd, nil, nil)}
	prev := Decision{Layer: LayerChispa, Intent: OutOfScope, Reason: ReasonOutOfScope}
	d, err = v2.Decide(WithPrev(context.Background(), prev), "pon una alarma a las siete", "es")
	if err != nil || !d.Confident || len(d.ActionList()) != 0 || d.Reason != ReasonVetoedByChispa {
		t.Fatalf("veto: %+v %v", d, err)
	}
	// Una situación sin verbo de orden (lo indirecto) pasa; si el LLM la
	// llama orden, no.
	d, _ = v2.Decide(WithPrev(context.Background(), prev), "me voy de casa", "es")
	if len(d.ActionList()) != 0 {
		t.Fatalf("kind command on a Chispa out-of-scope phrase must be vetoed: %+v", d)
	}
	sit := `{"kind":"situation","reply":"Activo la alarma.","actions":[{"intent":"alarm_arm","device":null,"area":null,"value":null,"color":null}]}`
	d, _ = (&VON{Generate: fakeGen(sit, nil, nil)}).Decide(WithPrev(context.Background(), prev), "me voy de casa", "es")
	if len(d.ActionList()) != 1 {
		t.Fatalf("indirect vetoed: %+v", d)
	}
	// Lo que Chispa dudaba (no «fuera de ámbito») no tiene veto.
	d, _ = v2.Decide(WithPrev(context.Background(), Decision{Reason: ReasonLowProb}), "pon la alarma", "es")
	if len(d.ActionList()) != 1 {
		t.Fatalf("low-probability escalation vetoed: %+v", d)
	}
}

func TestVONLayerErrors(t *testing.T) {
	if _, err := (&VON{}).Decide(context.Background(), "x", "es"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no generator: %v", err)
	}
	if _, err := (&VON{Generate: fakeGen("", errors.New("503 no replica"), nil)}).Decide(context.Background(), "x", "es"); err == nil {
		t.Fatal("a failed call is an error of the layer")
	}
	// JSON inválido: la capa contesta, pero no hace nada y pregunta.
	d, err := (&VON{Generate: fakeGen("", ErrInvalidJSON, nil)}).Decide(context.Background(), "x", "es")
	if err != nil || d.Confident || d.Reason != ReasonInvalidOutput || d.Reply == "" {
		t.Fatalf("invalid json: %+v %v", d, err)
	}
	d, _ = (&VON{Generate: fakeGen(strings.Repeat("x", maxLLMOutput+1), nil, nil)}).Decide(context.Background(), "x", "es")
	if d.Confident {
		t.Fatal("an oversized answer is not parsed")
	}
}

func TestCascade(t *testing.T) {
	m, err := NewMatcher(DemoTemplates)
	if err != nil {
		t.Fatal(err)
	}
	fast := &Decider{Matcher: m} // sin Chispa: lo que no es de la demo escala
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
	c := &Cascade{Fast: InProcess(fast), Slow: []NamedLayer{
		{Name: LayerVON, Layer: mock("von", von, nil)},
	}}

	tr := c.Decide(context.Background(), "enciende la luz del salón", "es")
	if tr.Decided != LayerTemplate || len(tr.Steps) != 1 || len(called) != 0 || len(tr.Actions) != 1 {
		t.Fatalf("template: %+v called=%v", tr, called)
	}
	status := func(tr Trace) string {
		var out []string
		for _, s := range tr.Steps {
			out = append(out, s.Layer+":"+s.Status)
		}
		return strings.Join(out, " ")
	}
	tr = c.Decide(context.Background(), "aquí hace frío", "es")
	if got := status(tr); tr.Decided != LayerVON || got != "template:nomatch chispa:unavailable encoder:unavailable von:answered" || tr.Actions[0].Intent != "temperature_up" {
		t.Fatalf("von: %s %+v", got, tr)
	}

	// Capa 4 apagada por su evaluación: no hace nada, y la traza lo dice.
	c.Slow[0].Layer = Disabled
	tr = c.Decide(context.Background(), "aquí hace frío", "es")
	if tr.Decided != "none" || len(tr.Actions) != 0 || tr.Steps[3].Status != StepDisabled {
		t.Fatalf("disabled: %+v", tr)
	}
	// Fuera de su alcance: ni se llama.
	called = nil
	c.Slow[0] = NamedLayer{Name: LayerVON, Layer: mock("von", von, nil), Skip: func(Decision) bool { return true }}
	tr = c.Decide(context.Background(), "aquí hace frío", "es")
	if tr.Steps[3].Status != StepSkipped || len(called) != 0 {
		t.Fatalf("skip: %+v", tr)
	}
	// Un error de la capa: no se hace nada.
	c.Slow[0] = NamedLayer{Name: LayerVON, Layer: mock("von", Decision{}, errors.New("boom"))}
	tr = c.Decide(context.Background(), "aquí hace frío", "es")
	if tr.Steps[3].Status != StepError || tr.Decided != "none" {
		t.Fatalf("error: %+v", tr)
	}
	// Sin capas rápidas (el gateway no contesta): nada, con el error.
	c.Fast = func(context.Context, string, string) (Decision, error) { return Decision{}, errors.New("gateway down") }
	tr = c.Decide(context.Background(), "enciende la luz", "es")
	if tr.Decided != "none" || tr.Steps[0].Status != StepError {
		t.Fatalf("fast error: %+v", tr)
	}
}

// FastSteps reparte en pasos lo que devuelve /v1/decide con el codificador.
func TestFastStepsEncoder(t *testing.T) {
	d := Decision{Layer: LayerEncoder, Intent: "temperature_up", Confident: true, Prob: 0.9, FastIntent: OutOfScope, FastProb: 0.97,
		LatencyUS: 12000, EncoderUS: 11000}
	s := FastSteps(d, true)
	if len(s) != 3 || s[0].Status != StepNoMatch || s[1].Layer != LayerChispa || s[1].Status != StepEscalated || s[1].LatencyUS != 1000 ||
		s[2].Layer != LayerEncoder || s[2].Status != StepAnswered || s[2].LatencyUS != 11000 {
		t.Fatalf("%+v", s)
	}
	// El codificador duda: sigue la conjetura del modelo rápido y escala.
	d = Decision{Layer: LayerChispa, Intent: "turn_on", Reason: ReasonLowProb, LatencyUS: 9000, EncoderUS: 8000}
	if s := FastSteps(d, true); s[1].Status != StepEscalated || s[2].Status != StepEscalated {
		t.Fatalf("%+v", s)
	}
	// Chispa confiado: el codificador no hizo falta.
	if s := FastSteps(Decision{Layer: LayerChispa, Intent: "turn_on", Confident: true}, true); s[2].Status != StepNotReached {
		t.Fatalf("%+v", s)
	}
	if s := FastSteps(Decision{Layer: LayerChispa, Reason: ReasonMultiCommand}, true); s[2].Status != StepSkipped {
		t.Fatalf("%+v", s)
	}
	if s := FastSteps(Decision{Layer: LayerChispa, EncoderError: "timeout", EncoderUS: 2e6}, true); s[2].Status != StepError {
		t.Fatalf("%+v", s)
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
	// 15 órdenes que Chispa dudaba y VON acierta.
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
	// Sin llamar a VON en lo que Chispa da por fuera de ámbito, pasa.
	if g := r.Gate(nil, ScopeUncertain); !g.Pass || g.Loss != 0 || g.Total.WithWrong != 0 {
		t.Fatalf("uncertain scope: %+v", g)
	}
	r.Groups["oos"].Weight = 1
	if g := r.Gate(nil, ScopeAll); !g.Pass {
		t.Fatalf("unweighted it should pass: %+v", g)
	}
}
