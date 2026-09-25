package domotica

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/chispa/slots"
)

// fakeRemote es una Chispa serverless de mentira (lo que pkg/aigw ya validó).
type fakeRemote struct {
	ans   RemoteAnswer
	err   error
	calls int
}

func (f *fakeRemote) ClassifyIntent(context.Context, string, string) (RemoteAnswer, error) {
	f.calls++
	return f.ans, f.err
}

func spanOf(text, slot, sub string) slots.Span {
	i := strings.Index(text, sub)
	return slots.Span{Slot: slot, Start: i, End: i + len(sub), Text: sub}
}

func TestRemoteIntent(t *testing.T) {
	m := newTestMatcher(t)
	ctx := context.Background()
	text := "oye quiero la luz de la cocina"
	rep := &ReplicaInfo{Model: "chispa-room", State: ReplicaFrozen, WakeMS: 27, RequestMS: 0.4}
	rem := &fakeRemote{ans: RemoteAnswer{Label: "turn_on", Prob: 0.9, Confident: true, HasSlots: true,
		Spans: []slots.Span{spanOf(text, SlotArea, "cocina")}, Replica: rep}}
	d := &Decider{Matcher: m, Remote: rem}

	// Las plantillas no despiertan la microVM.
	if got := d.DecideContext(ctx, "apaga la luz del baño", ""); got.Layer != LayerTemplate || rem.calls != 0 {
		t.Fatalf("template: %+v (%d calls)", got, rem.calls)
	}
	got := d.DecideContext(ctx, text, "es")
	if !got.Confident || got.Layer != LayerChispa || got.Slots.Area != "kitchen" || got.ChispaReplica != rep {
		t.Fatalf("remote: %+v", got)
	}
	if st := FastSteps(got, false); st[1].Replica != rep || st[1].Status != StepAnswered {
		t.Fatalf("steps: %+v", st)
	}

	// Un fallo de la réplica escala como una duda, con el motivo en la traza.
	rem.err = errors.New("no replica")
	got = d.DecideContext(ctx, text, "es")
	if got.Confident || got.Reason != ReasonChispaError || got.Escalate != EscalateTo || got.ChispaError == "" {
		t.Fatalf("error: %+v", got)
	}
	if st := FastSteps(got, false); st[1].Status != StepError || st[1].Error == "" {
		t.Fatalf("error steps: %+v", st)
	}
}

// Chispa dice «fuera de ámbito» y la capa 3 ve una orden: los huecos que la
// réplica marcó igualmente se usan, sin volver a preguntar.
func TestRemoteSpansReachEncoder(t *testing.T) {
	m := newTestMatcher(t)
	text := "qué oscuro está el pasillo"
	rep := &ReplicaInfo{Model: "chispa-room", State: ReplicaWarm}
	rem := &fakeRemote{ans: RemoteAnswer{Label: OutOfScope, Prob: 0.8, Confident: true, HasSlots: true,
		Spans: []slots.Span{spanOf(text, SlotArea, "pasillo")}, Replica: rep}}
	enc := &fakeEncoder{intent: "turn_on", prob: 0.99, confident: true}
	d := &Decider{Matcher: m, Remote: rem, Encoder: enc}
	got := d.DecideContext(context.Background(), text, "es")
	if got.Layer != LayerEncoder || got.Intent != "turn_on" || got.Slots.Area != "hallway" || rem.calls != 1 {
		t.Fatalf("got %+v", got)
	}
	if got.ChispaReplica != rep {
		t.Fatalf("the encoder's decision must keep the chispa replica: %+v", got)
	}
	if len(got.Spans) != 1 {
		t.Fatalf("spans: %+v", got.Spans)
	}
}
