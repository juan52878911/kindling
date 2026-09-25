package train

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/juan52878911/kindling/pkg/chispa"
)

// synth genera un corpus sintético reproducible: cada clase tiene su
// vocabulario, todas comparten ruido, y un 8 % de las etiquetas están
// cambiadas para que el problema no sea trivial y la calibración tenga trabajo.
func synth(n int, seed uint64) []chispa.Example {
	vocab := map[string][]string{
		"bug":   {"crash", "panic", "null", "overflow", "segfault", "wrong", "broken", "regression"},
		"feat":  {"add", "support", "new", "implement", "introduce", "option", "flag", "api"},
		"docs":  {"readme", "typo", "documentation", "guide", "example", "comment", "clarify", "wording"},
		"chore": {"bump", "deps", "release", "version", "update", "lockfile", "cleanup", "rename"},
	}
	labels := []string{"bug", "chore", "docs", "feat"}
	noise := []string{"the", "in", "for", "parser", "server", "client", "cache", "config", "when", "with", "on", "of"}
	r := splitmix{s: seed}
	out := make([]chispa.Example, n)
	for i := range out {
		l := labels[r.next()%uint64(len(labels))]
		var b bytes.Buffer
		for j := 0; j < 2+int(r.next()%3); j++ {
			v := vocab[l]
			b.WriteString(v[r.next()%uint64(len(v))] + " ")
			b.WriteString(noise[r.next()%uint64(len(noise))] + " ")
		}
		b.WriteString(noise[r.next()%uint64(len(noise))])
		gold := l
		if r.next()%100 < 8 {
			gold = labels[r.next()%uint64(len(labels))]
		}
		fields := map[string]any{"files": float64(1 + r.next()%20)}
		if l == "docs" && r.next()%2 == 0 {
			fields["ext"] = ".md"
		}
		out[i] = chispa.Example{Text: b.String(), Label: gold, Fields: fields}
	}
	return out
}

func smallCfg() Config {
	spec := chispa.DefaultSpec()
	spec.Buckets = 1 << 12
	return Config{Spec: spec, Seed: 7, MinSupport: 5}
}

func modelDigest(t *testing.T, m *chispa.Model) string {
	t.Helper()
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, m.W)
	binary.Write(&b, binary.LittleEndian, m.Scales)
	binary.Write(&b, binary.LittleEndian, m.Bias)
	binary.Write(&b, binary.LittleEndian, m.Temperature)
	binary.Write(&b, binary.LittleEndian, m.Thresholds)
	return fmt.Sprintf("%x", sha256.Sum256(b.Bytes()))[:16]
}

func TestTrainLearnsAndCalibrates(t *testing.T) {
	tr, va, te := synth(2000, 1), synth(400, 2), synth(600, 3)
	res, err := Train(tr, va, smallCfg())
	if err != nil {
		t.Fatal(err)
	}
	rep := chispa.Evaluate(res.Model, te)
	t.Logf("test acc %.3f macroF1 %.3f ece %.3f coverage %.3f conf-prec %.3f T=%.3f quant-agree %.4f epochs %d",
		rep.Accuracy, rep.MacroF1, rep.ECE, rep.Coverage, rep.ConfidentPrecision, res.Model.Temperature, res.QuantAgreement, res.BestEpoch)
	// Con 8 % de etiquetas cambiadas al azar (2 % caen en la misma), el techo
	// está en ~0,94.
	if rep.Accuracy < 0.85 {
		t.Errorf("accuracy %.3f, want >= 0.85", rep.Accuracy)
	}
	if rep.ECE > 0.06 {
		t.Errorf("ECE %.3f, want <= 0.06 after temperature scaling", rep.ECE)
	}
	if res.QuantAgreement < 0.995 {
		t.Errorf("int16 vs float agreement on validation %.4f, want >= 0.995", res.QuantAgreement)
	}
	// El requisito de la cuantización, sobre un conjunto que el entrenamiento
	// no ha visto: la etiqueta int16 y la float64 coinciden en >= 99,5 %.
	if a := res.Agreement(te); a < 0.995 {
		t.Errorf("int16 vs float agreement on test %.4f, want >= 0.995", a)
	} else {
		t.Logf("int16 vs float agreement on test: %.4f", a)
	}
}

// El mismo corpus y la misma semilla dan exactamente los mismos pesos; otra
// semilla, otros. El valor dorado se comprueba también con GOARCH=amd64 (ver
// docs/chispa.md): si el compilador fundiera alguna operación en FMA en una
// arquitectura, este hash cambiaría allí.
func TestTrainDeterministic(t *testing.T) {
	tr, va := synth(800, 11), synth(200, 12)
	a, err := Train(tr, va, smallCfg())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Train(tr, va, smallCfg())
	if err != nil {
		t.Fatal(err)
	}
	da, db := modelDigest(t, a.Model), modelDigest(t, b.Model)
	if da != db {
		t.Fatalf("same seed, different models: %s vs %s", da, db)
	}
	cfg := smallCfg()
	cfg.Seed = 8
	c, err := Train(tr, va, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if modelDigest(t, c.Model) == da {
		t.Error("different seed produced an identical model")
	}
	const golden = "9a1965fa40560be2"
	if da != golden {
		t.Errorf("trained model digest %s, golden %s (cross-architecture drift?)", da, golden)
	}
}

func TestBinaryMode(t *testing.T) {
	var tr, va []chispa.Example
	for _, ex := range synth(1500, 21) {
		if ex.Label != "bug" {
			ex.Label = "other"
		}
		tr = append(tr, ex)
	}
	for _, ex := range synth(300, 22) {
		if ex.Label != "bug" {
			ex.Label = "other"
		}
		va = append(va, ex)
	}
	res, err := Train(tr, va, smallCfg())
	if err != nil {
		t.Fatal(err)
	}
	m := res.Model
	if !m.Binary || m.NumOutputs() != 1 {
		t.Fatalf("2 labels should train a binary model, got binary=%v outputs=%d", m.Binary, m.NumOutputs())
	}
	p := m.PredictFull(chispa.Input{Text: "segfault crash in parser"}, 3)
	if p.Label != "bug" {
		t.Errorf("predicted %q for an obvious bug", p.Label)
	}
	if s := p.Probs[0].Prob + p.Probs[1].Prob; math.Abs(s-1) > 1e-12 {
		t.Errorf("probabilities sum to %v", s)
	}
	if len(p.Evidence) == 0 || p.Evidence[0].Weight <= 0 {
		t.Errorf("expected positive evidence, got %+v", p.Evidence)
	}
}

func TestSplitValidationStable(t *testing.T) {
	exs := synth(1000, 5)
	tr1, va1 := SplitValidation(exs, 0.2, 3)
	rev := make([]chispa.Example, len(exs))
	for i := range exs {
		rev[len(exs)-1-i] = exs[i]
	}
	_, va2 := SplitValidation(rev, 0.2, 3)
	if len(va1) != len(va2) || len(tr1)+len(va1) != len(exs) {
		t.Fatalf("split depends on order: %d vs %d", len(va1), len(va2))
	}
	if f := float64(len(va1)) / float64(len(exs)); f < 0.15 || f > 0.25 {
		t.Errorf("validation fraction %.3f, want ~0.2", f)
	}
}
