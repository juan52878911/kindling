package aigw

import (
	"context"
	"strconv"
	"testing"

	"github.com/juan52878911/kindling/internal/chispa"
)

// MinVotes pide maestros distintos: varios votos del mismo maestro sobre un
// caso no cuentan como acuerdo.
func TestAgreementCuentaMaestrosDistintos(t *testing.T) {
	c := &learnCase{Text: "x", HasChispa: true, Chispa: CaptureChispa{Top: []chispa.ClassProb{{Label: "fix", Prob: 0.5}}}}
	lc := LearnConfig{MinVotes: 2, TopK: 3}
	labels := map[string]bool{"fix": true}

	c.Votes = []TeacherVote{{Name: "ext:a", Label: "fix"}, {Name: "ext:a", Label: "fix"}, {Name: "ext:a", Label: "fix"}}
	if _, ok, why := agreement(c, lc, labels, nil); ok || why != reasonFewVotes {
		t.Fatalf("un solo maestro con tres votos: ok %v, motivo %q", ok, why)
	}
	c.Votes = []TeacherVote{{Name: "ext:a", Label: "fix"}, {Name: "ext:b", Label: "fix"}}
	if _, ok, why := agreement(c, lc, labels, nil); !ok {
		t.Fatalf("dos maestros distintos de acuerdo: motivo %q", why)
	}
}

// La muestra de auditoría no puede salir de algo que quien manda el texto
// sepa calcular (su sha256): si no, un maestro externo podría portarse bien
// solo en los casos que se van a revisar.
func TestAuditoriaNoSaleDelHashDelTexto(t *testing.T) {
	f := newLearnGateway(t, nil)
	f.stream(t, f.traffic[:200])
	rv, err := f.g.Review(context.Background(), ReviewRequest{Task: "kind", N: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(rv.Cases) < 40 {
		t.Fatalf("pocos casos para comparar: %d", len(rv.Cases))
	}
	iguales := 0
	for _, c := range rv.Cases {
		h, err := strconv.ParseUint(textKey(c.Text)[:16], 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		if (h%100 < auditPct) == c.Audit {
			iguales++
		}
	}
	if iguales == len(rv.Cases) {
		t.Fatalf("los %d casos de la auditoría se deducen del sha256 de su texto", len(rv.Cases))
	}
}
