package jev_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/jev"
	"github.com/juan52878911/kindling/pkg/jev/train"
)

type rng struct{ s uint64 }

func (r *rng) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *rng) pick(s []string) string { return s[r.next()%uint64(len(s))] }

var classes = []string{"build", "chore", "ci", "docs", "feat", "fix", "perf", "refactor", "style", "test"}

// corpus: diez clases con vocabulario propio y ruido común, 10 % de etiquetas
// cambiadas. Solo para ejercitar la maquinaria; los números reales están en
// docs/JEV-EVAL.md.
func corpus(n int, seed uint64) []jev.Example {
	noise := strings.Fields("the a in for of to with on when from parser server client cache config bundler runtime module api")
	r := rng{s: seed}
	out := make([]jev.Example, n)
	for i := range out {
		c := r.next() % uint64(len(classes))
		var words []string
		for j := 0; j < 3+int(r.next()%5); j++ {
			words = append(words, fmt.Sprintf("%s%d", classes[c][:min(3, len(classes[c]))], r.next()%6), r.pick(noise))
		}
		label := classes[c]
		if r.next()%10 == 0 {
			label = r.pick(classes)
		}
		out[i] = jev.Example{Text: strings.Join(words, " "), Label: label,
			Fields: map[string]any{"files": float64(r.next() % 30), "ext": r.pick([]string{".go", ".md", ".ts"})}}
	}
	return out
}

var (
	modelOnce sync.Once
	testModel *jev.Model
	modelErr  error
)

// trained devuelve un modelo de 10 clases a 2^18 cubos (el tamaño por
// defecto), entrenado una vez para todo el paquete.
func trained(t testing.TB) *jev.Model {
	t.Helper()
	modelOnce.Do(func() {
		cfg := train.Config{Spec: jev.DefaultSpec(), Seed: 3, MinSupport: 5}
		cfg.Spec.CharMin, cfg.Spec.CharMax = 3, 5
		res, err := train.Train(corpus(3000, 1), corpus(600, 2), cfg)
		if err != nil {
			modelErr = err
			return
		}
		testModel = res.Model
	})
	if modelErr != nil {
		t.Fatal(modelErr)
	}
	return testModel
}

func TestFitTemperature(t *testing.T) {
	// Logits «demasiado seguras»: las etiquetas salen de softmax(z/2), así que
	// la temperatura correcta es 2.
	r := rng{s: 9}
	var logits [][]float64
	var gold []int
	for i := 0; i < 4000; i++ {
		z := make([]float64, 3)
		for k := range z {
			z[k] = float64(r.next()%2000)/100 - 10
		}
		p := make([]float64, 3)
		s := 0.0
		for k := range z {
			p[k] = math.Exp(z[k] / 2)
			s += p[k]
		}
		u := float64(r.next()%1_000_000) / 1_000_000 * s
		g := 0
		for acc := p[0]; acc < u && g < 2; acc += p[g] {
			g++
		}
		logits = append(logits, z)
		gold = append(gold, g)
	}
	if T := jev.FitTemperature(logits, gold); T < 1.8 || T > 2.2 {
		t.Errorf("fitted temperature %.3f, want ~2", T)
	}
}

func TestChooseThresholds(t *testing.T) {
	// Clase 0: aciertos por encima de 0.8, fallos por debajo → τ = 0.8.
	// Clase 1: siempre falla → nunca confiada.
	var pred, gold []int
	var prob []float64
	for i := 0; i < 100; i++ {
		p := 0.5 + float64(i)/200 // 0.5 .. 0.995
		pred, prob = append(pred, 0), append(prob, p)
		if p >= 0.8 {
			gold = append(gold, 0)
		} else {
			gold = append(gold, 1)
		}
		pred, prob, gold = append(pred, 1), append(prob, p), append(gold, 0)
	}
	th := jev.ChooseThresholds(2, pred, gold, prob, jev.ThresholdParams{TargetPrecision: 0.95, MinSupport: 5})
	if th[0] < 0.79 || th[0] > 0.82 {
		t.Errorf("τ for the separable class = %.3f, want ~0.8", th[0])
	}
	if th[1] != jev.NeverConfident {
		t.Errorf("τ for a class that is always wrong = %.3f, want NeverConfident", th[1])
	}
	// Con soporte insuficiente no se fía aunque acierte todo.
	th = jev.ChooseThresholds(1, []int{0, 0, 0}, []int{0, 0, 0}, []float64{0.99, 0.98, 0.97},
		jev.ThresholdParams{TargetPrecision: 0.5, MinSupport: 10})
	if th[0] != jev.NeverConfident {
		t.Errorf("τ with 3 examples and MinSupport 10 = %v", th[0])
	}
}

var probeInputs = []jev.Input{
	{Text: "fix0 crash in the parser when cache is cold"},
	{Text: "doc2 doc3 readme for the api", Fields: map[string]any{"ext": ".md", "files": 1.0}},
	{Text: "tes1 tes4 flaky module", Fields: map[string]any{"files": 12.0}},
	{Text: ""},
	{Text: "Übergrößenträger 日本語 ok", Fields: map[string]any{"ext": ".go"}},
}

func probDigest(m *jev.Model) string {
	h := sha256.New()
	for _, in := range probeInputs {
		p := m.PredictFull(in, 0)
		for _, c := range p.Probs {
			binary.Write(h, binary.LittleEndian, math.Float64bits(c.Prob))
		}
		fmt.Fprintf(h, "%s|%v|", p.Label, p.Confident)
	}
	return fmt.Sprintf("%x", h.Sum(nil))[:16]
}

// El dorado se genera en arm64 (donde Go funde FMA si se le deja) y se
// comprueba igual con GOARCH=amd64: las probabilidades son bit a bit iguales.
func TestPredictGoldenBits(t *testing.T) {
	m := trained(t)
	if got := probDigest(m); got != predictGolden {
		t.Errorf("prediction digest %s, golden %s (cross-architecture drift?)", got, predictGolden)
	}
}

const predictGolden = "f8b4ad07ddf5647c"

func TestRoundTrip(t *testing.T) {
	m := trained(t)
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := jev.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if probDigest(m) != probDigest(m2) {
		t.Error("predictions changed after a save/load round trip")
	}
	b2, _ := m2.Marshal()
	if !bytes.Equal(b, b2) {
		t.Error("re-marshal is not byte-identical")
	}
	dense := int(m.Spec.Buckets) * m.NumOutputs() * 2
	t.Logf("artifact %d bytes (dense weights would be %d)", len(b), dense)
	if len(b) >= dense {
		t.Errorf("a sparsely trained model should be stored sparse: %d >= %d", len(b), dense)
	}
}

func TestDenseRoundTrip(t *testing.T) {
	spec := jev.DefaultSpec()
	spec.Buckets = 16
	r := rng{s: 4}
	w := make([]int16, 16*3)
	for i := range w {
		w[i] = int16(r.next()%2000) - 1000
	}
	m := &jev.Model{Spec: spec, SpecHash: spec.Hash(), Labels: []string{"a", "b", "c"},
		Scales: []float64{1e-3, 2e-3, 3e-3}, Bias: []float64{0, 0.1, -0.1}, W: w,
		Temperature: 1.5, Thresholds: []float64{0.5, 0.6, jev.NeverConfident}}
	if err := m.Init(); err != nil {
		t.Fatal(err)
	}
	b, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	m2, err := jev.Unmarshal(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range probeInputs {
		if a, b := m.Predict(in), m2.Predict(in); !reflect.DeepEqual(a, b) {
			t.Errorf("%+v vs %+v", a, b)
		}
	}
}

// fixCRC recalcula la suma de un fichero manipulado: así el cargador tiene que
// defenderse con sus validaciones y no solo con el CRC.
func fixCRC(b []byte) []byte {
	b = append([]byte(nil), b...)
	binary.LittleEndian.PutUint32(b[len(b)-4:], crc32.Checksum(b[:len(b)-4], crc32.MakeTable(crc32.Castagnoli)))
	return b
}

func TestLoadRejectsCorrupt(t *testing.T) {
	good, err := trained(t).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < len(good); n += 1 + n/7 {
		if _, err := jev.Unmarshal(good[:n]); err == nil {
			t.Fatalf("truncated to %d bytes: accepted", n)
		}
	}
	for i := 0; i < len(good); i += 1 + i/5 {
		bad := append([]byte(nil), good...)
		bad[i] ^= 0x40
		if _, err := jev.Unmarshal(bad); err == nil {
			t.Fatalf("flipped byte %d: accepted", i)
		}
	}
	// Con el CRC recalculado: cada campo del cuerpo manipulado.
	hlen := int(binary.LittleEndian.Uint32(good[12:]))
	body := 16 + hlen
	for _, c := range []struct {
		name string
		off  int
		val  []byte
	}{
		{"spec hash", body, []byte{1}},
		{"buckets", body + 8, []byte{0xff, 0xff, 0xff, 0x7f}},
		{"outputs", body + 12, []byte{0xff, 0xff, 0, 0}},
		{"temperature NaN", body + 16, []byte{0, 0, 0, 0, 0, 0, 0xf8, 0x7f}},
		{"version", 8, []byte{9}},
		{"flags", 10, []byte{0x80}},
		{"header length", 12, []byte{0xff, 0xff, 0xff, 0xff}},
	} {
		bad := append([]byte(nil), good...)
		copy(bad[c.off:], c.val)
		if _, err := jev.Unmarshal(fixCRC(bad)); err == nil {
			t.Errorf("%s tampered: accepted", c.name)
		}
	}
	if _, err := jev.Load(bytes.NewReader(make([]byte, jev.MaxFileBytes+10))); err == nil {
		t.Error("oversized file accepted")
	}
}

// FuzzLoad: ningún fichero hace que el cargador entre en pánico, y lo que
// carga se puede usar para predecir sin pánico.
func FuzzLoad(f *testing.F) {
	spec := jev.DefaultSpec()
	spec.Buckets = 16
	m := &jev.Model{Spec: spec, SpecHash: spec.Hash(), Labels: []string{"a", "b"}, Binary: true,
		Scales: []float64{1e-3}, Bias: []float64{0}, W: make([]int16, 16),
		Temperature: 1, Thresholds: []float64{0.9, 0.9}}
	m.W[3] = 100
	good, err := m.Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(good)
	f.Add(good[:len(good)/2])
	f.Add([]byte("\x89JEV\r\n\x1a\n"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, d := range [][]byte{data, fixCRC(append(append([]byte(nil), data...), 0, 0, 0, 0))} {
			if len(d) > 1<<20 {
				continue
			}
			m, err := jev.Unmarshal(d)
			if err != nil {
				continue
			}
			for _, in := range probeInputs {
				p := m.Predict(in)
				if p.Prob < 0 || p.Prob > 1 || math.IsNaN(p.Prob) {
					t.Fatalf("loaded model gives prob %v", p.Prob)
				}
			}
		}
	})
}

func TestPredictAPI(t *testing.T) {
	m := trained(t)
	p := m.PredictFull(jev.Input{Text: "fix1 fix2 fix3 crash in the cache", Fields: map[string]any{"ext": ".go"}}, 5)
	if p.Label != "fix" {
		t.Errorf("label %q, want fix", p.Label)
	}
	if p.Threshold != m.Thresholds[p.Index] || p.Confident != (p.Prob >= p.Threshold) {
		t.Errorf("inconsistent decision: %+v", p)
	}
	want := jev.DecisionEscalate
	if p.Confident {
		want = jev.DecisionConfident
	}
	if p.Decision != want {
		t.Errorf("decision %q, want %q", p.Decision, want)
	}
	sum := 0.0
	for _, c := range p.Probs {
		sum += c.Prob
	}
	if math.Abs(sum-1) > 1e-12 || p.Probs[0].Label != p.Label {
		t.Errorf("probs %+v", p.Probs)
	}
	if len(p.Evidence) == 0 || !strings.HasPrefix(p.Evidence[0].Feature, "w:fix") && !strings.HasPrefix(p.Evidence[0].Feature, "c:") {
		t.Errorf("evidence %+v", p.Evidence)
	}
	short := m.Predict(jev.Input{Text: "fix1 fix2 fix3 crash in the cache", Fields: map[string]any{"ext": ".go"}})
	if short.Label != p.Label || short.Prob != p.Prob {
		t.Error("Predict and PredictFull disagree")
	}
	// Un texto sin nada conocido no puede ser confiado.
	if q := m.Predict(jev.Input{Text: "zzz qqq"}); q.Confident {
		t.Errorf("unknown input answered confidently: %+v", q)
	}
}

func TestPredictConcurrent(t *testing.T) {
	m := trained(t)
	inputs := corpus(200, 77)
	want := make([]jev.Prediction, len(inputs))
	for i, ex := range inputs {
		want[i] = m.Predict(ex.Input())
	}
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rep := 0; rep < 5; rep++ {
				for i, ex := range inputs {
					if got := m.Predict(ex.Input()); !reflect.DeepEqual(got, want[i]) {
						errs <- fmt.Sprintf("input %d: %+v vs %+v", i, got, want[i])
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// text200 es un mensaje realista de ~200 caracteres.
const text200 = "Fix a crash in the bundler when a CSS module imports a file that was deleted " +
	"during watch mode; the resolver kept a stale entry in the cache and dereferenced it on the next rebuild."

func TestPredictZeroAlloc(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector allocates")
	}
	m := trained(t)
	in := jev.Input{Text: text200, Fields: map[string]any{"files": 3.0, "ext": ".ts"}}
	m.Predict(in)
	if a := testing.AllocsPerRun(200, func() { m.Predict(in) }); a != 0 {
		t.Errorf("Predict allocates %.1f times per call", a)
	}
}

func benchModel(b *testing.B, char bool) *jev.Model {
	cfg := train.Config{Spec: jev.DefaultSpec(), Seed: 3, MinSupport: 5}
	if char {
		cfg.Spec.CharMin, cfg.Spec.CharMax = 3, 5
	}
	res, err := train.Train(corpus(3000, 1), corpus(600, 2), cfg)
	if err != nil {
		b.Fatal(err)
	}
	return res.Model
}

func BenchmarkPredict(b *testing.B) {
	for _, c := range []struct {
		name   string
		char   bool
		fields map[string]any
	}{
		{"words", false, nil},
		{"words+fields", false, map[string]any{"files": 3.0, "ext": ".ts", "service": "api"}},
		{"words+char3-5", true, nil},
	} {
		m := benchModel(b, c.char)
		in := jev.Input{Text: text200, Fields: c.fields}
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				m.Predict(in)
			}
		})
	}
}

func BenchmarkPredictParallel(b *testing.B) {
	m := benchModel(b, false)
	in := jev.Input{Text: text200}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Predict(in)
		}
	})
}
