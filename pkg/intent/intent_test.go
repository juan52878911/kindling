package intent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// Un dominio de tickets de soporte, en datos: sirve de Domain de prueba para
// la cascada entera.
const ticketSchema = `{
  "out_of_scope": "other",
  "lang": "en",
  "intents": [
    {"name": "open_ticket", "slots": ["queue", "priority"], "required": ["queue"], "defaults": {"priority": "normal"}},
    {"name": "close_ticket", "slots": ["id"], "required": ["id"]},
    {"name": "list_tickets"}
  ],
  "values": {"queue": {"billing": ["invoices", "billing team"], "support": ["help desk"]}},
  "templates": [
    {"text": "Open a billing ticket!", "intent": "open_ticket", "slots": {"queue": "billing"}},
    {"text": "list my tickets", "intent": "list_tickets"}
  ],
  "multi_command": {"verbs": ["open", "close"], "joiners": ["and", "then"]}
}`

func ticketDomain(t testing.TB) *Schema {
	t.Helper()
	s, err := ParseSchema(strings.NewReader(ticketSchema))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func span(text, slot, sub string) slots.Span {
	i := strings.Index(text, sub)
	return slots.Span{Slot: slot, Start: i, End: i + len(sub), Text: sub}
}

type fakeRemote struct {
	ans RemoteAnswer
	err error
}

func (f *fakeRemote) ClassifyIntent(context.Context, string, string) (RemoteAnswer, error) {
	return f.ans, f.err
}

type fakeEncoder struct {
	intent    string
	prob      float64
	confident bool
	err       error
	calls     int
}

func (f *fakeEncoder) ClassifyIntent(context.Context, string) (string, float64, bool, error) {
	f.calls++
	return f.intent, f.prob, f.confident, f.err
}

func slotsOf(d Decision) map[string]string { m, _ := d.Slots.(map[string]string); return m }

func TestTemplateLayer(t *testing.T) {
	d := (&Decider{Domain: ticketDomain(t)}).Decide("open a BILLING ticket", "")
	if d.Layer != LayerTemplate || !d.Confident || d.Intent != "open_ticket" || d.Lang != "en" {
		t.Fatalf("got %+v", d)
	}
	if m := slotsOf(d); m["queue"] != "billing" || m["priority"] != "normal" {
		t.Fatalf("slots: %+v", m)
	}
	// Sin modelos, lo demás escala y los huecos son un objeto vacío, no null.
	d = (&Decider{Domain: ticketDomain(t)}).Decide("something else", "en")
	if d.Confident || d.Reason != ReasonNoModel || d.Escalate != EscalateTo {
		t.Fatalf("got %+v", d)
	}
	if b, _ := json.Marshal(d); !strings.Contains(string(b), `"slots":{}`) {
		t.Fatalf("empty slots must marshal as {}: %s", b)
	}
}

func TestRemoteChispaLayer(t *testing.T) {
	text := "please open one for the help desk"
	r := &fakeRemote{ans: RemoteAnswer{Label: "open_ticket", Prob: 0.9, Confident: true, HasSlots: true,
		Spans: []slots.Span{span(text, "queue", "help desk")}, Replica: &ReplicaInfo{Model: "m", State: ReplicaFrozen}}}
	d := (&Decider{Domain: ticketDomain(t), Remote: r}).Decide(text, "")
	if d.Layer != LayerChispa || !d.Confident || slotsOf(d)["queue"] != "support" || d.ChispaReplica.State != ReplicaFrozen {
		t.Fatalf("got %+v", d)
	}

	// Falta un hueco obligatorio: no es confiada.
	r.ans.Spans = nil
	if d = (&Decider{Domain: ticketDomain(t), Remote: r}).Decide(text, ""); d.Confident || d.Reason != ReasonMissingSlot {
		t.Fatalf("missing slot: %+v", d)
	}
	// Dos órdenes: tampoco.
	if d = (&Decider{Domain: ticketDomain(t), Remote: r}).Decide("open one and close 42", ""); d.Reason != ReasonMultiCommand {
		t.Fatalf("multi: %+v", d)
	}
	// Fuera de ámbito confiado escala salvo FinalOOS.
	r.ans = RemoteAnswer{Label: "other", Prob: 0.99, Confident: true}
	if d = (&Decider{Domain: ticketDomain(t), Remote: r}).Decide("hello", ""); d.Confident || d.Reason != ReasonOutOfScope {
		t.Fatalf("oos: %+v", d)
	}
	if d = (&Decider{Domain: ticketDomain(t), Remote: r, FinalOOS: true}).Decide("hello", ""); !d.Confident {
		t.Fatalf("final oos: %+v", d)
	}
	// La réplica falla: escala con el motivo.
	r.err = errors.New("down")
	if d = (&Decider{Domain: ticketDomain(t), Remote: r}).Decide("hello", ""); d.Reason != ReasonChispaError || d.ChispaError == "" {
		t.Fatalf("error: %+v", d)
	}
}

func TestEncoderLayer(t *testing.T) {
	text := "the invoices people need a new case"
	// Chispa dice fuera de ámbito pero la réplica marcó huecos: si el
	// codificador cambia la intención, se usan sin volver a preguntar.
	r := &fakeRemote{ans: RemoteAnswer{Label: "other", Prob: 0.8, Confident: true, HasSlots: true,
		Spans: []slots.Span{span(text, "queue", "invoices")}}}
	enc := &fakeEncoder{intent: "open_ticket", prob: 0.9, confident: true}
	d := (&Decider{Domain: ticketDomain(t), Remote: r, Encoder: enc}).Decide(text, "")
	if d.Layer != LayerEncoder || !d.Confident || slotsOf(d)["queue"] != "billing" || d.FastIntent != "other" {
		t.Fatalf("got %+v", d)
	}
	// Dos órdenes no pasan por el codificador.
	enc.calls = 0
	r.ans = RemoteAnswer{Label: "open_ticket", Prob: 0.9, Confident: true}
	d = (&Decider{Domain: ticketDomain(t), Remote: r, Encoder: enc}).Decide("open billing and close 7", "")
	if enc.calls != 0 || d.Escalate != EscalateVON || d.Reason != ReasonMultiCommand {
		t.Fatalf("multi: %+v (%d calls)", d, enc.calls)
	}
	// El codificador duda: escala a VON con la conjetura más segura.
	enc.confident, enc.prob = false, 0.4
	r.ans = RemoteAnswer{Label: "list_tickets", Prob: 0.6}
	d = (&Decider{Domain: ticketDomain(t), Remote: r, Encoder: enc}).Decide("hmm tickets", "")
	if d.Confident || d.Escalate != EscalateVON || d.Intent != "list_tickets" || d.Reason != ReasonLowProb {
		t.Fatalf("unsure: %+v", d)
	}
	// El codificador falla: escala a VON diciendo por qué.
	enc.err = errors.New("timeout")
	d = (&Decider{Domain: ticketDomain(t), Remote: r, Encoder: enc}).Decide("hmm tickets", "")
	if d.EncoderError == "" || d.Escalate != EscalateVON {
		t.Fatalf("encoder error: %+v", d)
	}
}

func TestSchemaRowsAndExact(t *testing.T) {
	s := ticketDomain(t)
	rows, err := ReadRows(strings.NewReader(`{"text":"open billing","intent":"open_ticket","slots":{"queue":"billing"},"source":"x"}
{"text":"close 7","intent":"close_ticket","slots":{"id":7}}

`))
	if err != nil || len(rows) != 2 {
		t.Fatalf("%v %d", err, len(rows))
	}
	g0, err := s.ParseSlots(rows[0].Slots)
	if err != nil {
		t.Fatal(err)
	}
	// El oro no dice la prioridad por defecto: se completa igual en los dos.
	if !s.Exact("open_ticket", g0, "open_ticket", map[string]string{"queue": "billing", "priority": "normal"}) {
		t.Fatal("defaults must be filled in on both sides")
	}
	if s.Exact("open_ticket", g0, "open_ticket", map[string]string{"queue": "support"}) {
		t.Fatal("a wrong slot is not exact")
	}
	g1, _ := s.ParseSlots(rows[1].Slots)
	if g1.(map[string]string)["id"] != "7" {
		t.Fatalf("numbers become strings: %+v", g1)
	}
	if _, err := ReadRows(strings.NewReader(`{"text":"x"}`)); err == nil {
		t.Fatal("a row without intent was accepted")
	}
}

func TestSchemaInvalid(t *testing.T) {
	for name, js := range map[string]string{
		"no oos":           `{"intents":[{"name":"a"}]}`,
		"unknown field":    `{"out_of_scope":"o","intents":[{"name":"a"}],"nope":1}`,
		"template intent":  `{"out_of_scope":"o","intents":[{"name":"a"}],"templates":[{"text":"x","intent":"b"}]}`,
		"required unknown": `{"out_of_scope":"o","intents":[{"name":"a","slots":["x"],"required":["y"]}]}`,
		"ambiguous":        `{"out_of_scope":"o","intents":[{"name":"a"},{"name":"b"}],"templates":[{"text":"x!","intent":"a"},{"text":"X","intent":"b"}]}`,
		"value clash":      `{"out_of_scope":"o","intents":[{"name":"a"}],"values":{"q":{"p":["same"],"r":["same"]}}}`,
	} {
		if _, err := ParseSchema(strings.NewReader(js)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestLangFields(t *testing.T) {
	if LangFields("") != nil || LangFields("es")["lang"] != "es" {
		t.Fatal("lang fields")
	}
	a, b := LangFields("en"), LangFields("en")
	a["probe"] = 1
	if b["probe"] != 1 {
		t.Fatal("the map is shared, not rebuilt per call")
	}
	delete(a, "probe")
}
