package domotica

import (
	"context"
	"errors"
	"testing"
)

// fakeEncoder contesta lo que se le diga y cuenta las llamadas.
type fakeEncoder struct {
	intent    string
	prob      float64
	confident bool
	err       error
	calls     int
}

func (f *fakeEncoder) ClassifyIntent(_ context.Context, _ string) (string, float64, bool, error) {
	f.calls++
	return f.intent, f.prob, f.confident, f.err
}

func TestEncoderLayer(t *testing.T) {
	m := newTestMatcher(t)
	ctx := context.Background()

	// Lo que la capa 1 contesta no llega al codificador.
	enc := &fakeEncoder{intent: "turn_on", prob: 0.99, confident: true}
	d := &Decider{Matcher: m, Encoder: enc}
	if got := d.DecideContext(ctx, "apaga la luz del baño", ""); got.Layer != LayerTemplate || enc.calls != 0 {
		t.Fatalf("template answer went to the encoder: %+v (%d calls)", got, enc.calls)
	}

	// Sin JEV todo lo demás llega; confiado y completo, contesta la capa 3.
	got := d.DecideContext(ctx, "no veo nada", "es")
	if !got.Confident || got.Layer != LayerEncoder || got.Intent != "turn_on" || got.Slots.Device != DevLight || got.Escalate != "" {
		t.Fatalf("confident encoder answer: %+v", got)
	}

	// Dos órdenes no se le preguntan: van a VON.
	enc.calls = 0
	d2 := &Decider{Matcher: m, Encoder: enc}
	fast := Decision{Intent: "turn_on", Prob: 0.9, Reason: ReasonMultiCommand, Layer: LayerJEV}
	if got := d2.encode(ctx, "enciende la luz y baja la persiana", fast); enc.calls != 0 || got.Escalate != EscalateVON {
		t.Fatalf("multi-command: %+v (%d calls)", got, enc.calls)
	}

	// «Fuera de ámbito» del codificador escala a VON, salvo FinalOOS.
	enc.intent, enc.prob = OutOfScope, 0.97
	if got := d.DecideContext(ctx, "pon una alarma a las siete", "es"); got.Confident || got.Escalate != EscalateVON || got.Reason != ReasonOutOfScope {
		t.Fatalf("encoder OOS: %+v", got)
	}
	final := &Decider{Matcher: m, Encoder: enc, FinalOOS: true}
	if got := final.DecideContext(ctx, "pon una alarma a las siete", "es"); !got.Confident || got.Intent != OutOfScope {
		t.Fatalf("encoder OOS, final: %+v", got)
	}

	// Sin confianza escala con la conjetura más probable de las dos capas.
	enc.intent, enc.prob, enc.confident = "volume_up", 0.4, false
	fast = Decision{Intent: "volume_down", Prob: 0.6, Layer: LayerJEV, Reason: ReasonLowProb}
	if got := d.encode(ctx, "esto suena rarísimo", fast); got.Intent != "volume_down" || got.Escalate != EscalateVON || got.FastIntent != "volume_down" {
		t.Fatalf("guess: %+v", got)
	}
	enc.prob = 0.7
	if got := d.encode(ctx, "esto suena rarísimo", fast); got.Intent != "volume_up" {
		t.Fatalf("guess, encoder more sure: %+v", got)
	}

	// Una intención confiada sin su valor no se ejecuta.
	enc.intent, enc.prob, enc.confident = "set_temperature", 0.99, true
	if got := d.DecideContext(ctx, "ponlo como antes", "es"); got.Confident || got.Reason != ReasonMissingSlot {
		t.Fatalf("missing slot: %+v", got)
	}

	// El codificador caído: escala a VON diciendo por qué.
	enc.err = errors.New("no replica available")
	got = d.DecideContext(ctx, "hace un frío tremendo", "es")
	if got.Confident || got.Escalate != EscalateVON || got.Reason != ReasonEncoderError || got.EncoderError == "" {
		t.Fatalf("encoder error: %+v", got)
	}

	// Solo el codificador (para evaluarlo): las plantillas no contestan.
	enc.err, enc.intent, enc.confident = nil, "turn_off", true
	only := &Decider{Matcher: m, Encoder: enc, OnlyEncoder: true}
	if got := only.DecideContext(ctx, "apaga la luz del baño", "es"); got.Layer != LayerEncoder {
		t.Fatalf("only encoder: %+v", got)
	}
}

func TestIndirectSplits(t *testing.T) {
	all := Indirect("")
	tr, va, te := Indirect("train"), Indirect("valid"), Indirect("test")
	if len(all) != 180 || len(tr) != 100 || len(va) != 40 || len(te) != 40 {
		t.Fatalf("splits: %d = %d + %d + %d", len(all), len(tr), len(va), len(te))
	}
	ch := map[string]bool{}
	for _, r := range Challenge() {
		ch[r.Text] = true
	}
	for _, r := range all {
		if ch[r.Text] || r.Class != "indirect" || Intent(r.Intent) == nil {
			t.Errorf("bad indirect row: %+v", r)
		}
	}
}
