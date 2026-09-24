package slots

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
)

// Sentence es un ejemplo de entrenamiento: texto y huecos de oro en bytes.
type Sentence struct {
	Text  string
	Spans []Span
}

// TrainConfig son los mandos del entrenamiento. Con la misma semilla y los
// mismos datos el .jevs sale idéntico.
type TrainConfig struct {
	Spec          Spec
	Lexicon       map[string]string // token normalizado → clase
	MaxEpochs     int               // 15
	Patience      int               // 3
	Seed          uint64            // 1
	CreatedAt     string
	DatasetSHA256 string
	Log           io.Writer
}

// TrainResult acompaña al modelo.
type TrainResult struct {
	Model          *Model
	BestEpoch      int
	Valid          *SpanReport // del modelo cuantizado
	QuantAgreement float64     // fracción de frases de validación con la misma salida float/int16
}

type encoded struct {
	feats []uint32
	foff  []int32
	gold  []int16
	os    []int32 // posición original de cada token (para alinear huecos)
	oe    []int32
}

// Align convierte huecos en bytes a etiquetas BIO sobre los tokens de text:
// un token entra en el hueco si se solapa con él. Los huecos de oro vienen de
// fuentes con criterios distintos (MASSIVE incluye a veces la preposición);
// alinearlos a tokens es lo que hace comparables oro y predicción.
func Align(tk *tokenizer, spans []Span, tagIdx map[string]int) []int16 {
	out := make([]int16, len(tk.toks))
	for _, sp := range spans {
		b, okB := tagIdx["B-"+sp.Slot]
		in, okI := tagIdx["I-"+sp.Slot]
		if !okB || !okI {
			continue
		}
		first := true
		for i, t := range tk.toks {
			if int(t.os) < sp.End && int(t.oe) > sp.Start && out[i] == 0 {
				if first {
					out[i] = int16(b)
					first = false
				} else {
					out[i] = int16(in)
				}
			}
		}
	}
	return out
}

func encodeAll(spec Spec, lex map[string]string, sents []Sentence, tagIdx map[string]int) []encoded {
	out := make([]encoded, 0, len(sents))
	var tk tokenizer
	for _, s := range sents {
		tk.run(s.Text, spec.MaxTextBytes, spec.MaxTokens)
		if len(tk.toks) == 0 {
			continue
		}
		var e encoded
		spec.features(&tk, lex, &e.feats, &e.foff)
		e.gold = Align(&tk, s.Spans, tagIdx)
		for _, t := range tk.toks {
			e.os = append(e.os, t.os)
			e.oe = append(e.oe, t.oe)
		}
		out = append(out, e)
	}
	return out
}

// Train entrena un perceptrón estructurado promediado (Collins 2002) con
// parada temprana sobre el F1 de huecos en validación, y lo cuantiza a int16.
// El perceptrón y no un CRF: sin gradientes ni tasas de aprendizaje que
// ajustar, converge en pocas épocas con estos datos y da el mismo resultado
// con la misma semilla.
func Train(train, valid []Sentence, cfg TrainConfig) (*TrainResult, error) {
	if cfg.MaxEpochs <= 0 {
		cfg.MaxEpochs = 15
	}
	if cfg.Patience <= 0 {
		cfg.Patience = 3
	}
	if cfg.Seed == 0 {
		cfg.Seed = 1
	}
	if err := cfg.Spec.Validate(); err != nil {
		return nil, err
	}
	if !cfg.Spec.Lexicon {
		cfg.Lexicon = nil
	}
	seen := map[string]bool{}
	for _, s := range train {
		for _, sp := range s.Spans {
			seen[sp.Slot] = true
		}
	}
	var names []string
	for k := range seen {
		names = append(names, k)
	}
	if len(names) == 0 {
		return nil, errors.New("no slots in the training data")
	}
	tags := TagsFor(names)
	if len(tags) > MaxTags {
		return nil, fmt.Errorf("too many slots (%d tags > %d)", len(tags), MaxTags)
	}
	tagIdx := map[string]int{}
	for i, t := range tags {
		tagIdx[t] = i
	}
	T := len(tags)
	B := int(cfg.Spec.Buckets)
	tr := encodeAll(cfg.Spec, cfg.Lexicon, train, tagIdx)
	va := encodeAll(cfg.Spec, cfg.Lexicon, valid, tagIdx)
	if len(va) == 0 { // sin validación: el 10 % del entrenamiento
		perm := identity(len(tr))
		(&splitmix{cfg.Seed ^ 0x5eed}).shuffle(perm)
		nv := len(tr) / 10
		var a, b []encoded
		for i, p := range perm {
			if i < nv {
				b = append(b, tr[p])
			} else {
				a = append(a, tr[p])
			}
		}
		tr, va = a, b
	}
	allowed := allowedTransitions(tags)

	w := make([]float64, B*T)
	u := make([]float64, B*T) // suma de c·Δ para el promedio perezoso
	tw := make([]float64, (T+1)*T)
	tu := make([]float64, (T+1)*T)
	c := 1.0
	var emis, dp []float64
	var back, path []int16
	decode := func(e *encoded, W, TW []float64) []int16 {
		n := len(e.gold)
		emis = growF(emis, n*T)
		for i := 0; i < n; i++ {
			row := emis[i*T : i*T+T]
			for t := range row {
				row[t] = 0
			}
			for _, b := range e.feats[e.foff[i]:e.foff[i+1]] {
				ws := W[int(b)*T : int(b)*T+T]
				for t := range row {
					row[t] += ws[t]
				}
			}
		}
		dp = growF(dp, n*T)
		back = growI16(back, n*T)
		path = growI16(path, n)
		return viterbi(emis, TW, allowed, n, T, dp, back, path)
	}
	avg := func() ([]float64, []float64) {
		aw := make([]float64, len(w))
		for i := range w {
			aw[i] = w[i] - u[i]/c
		}
		at := make([]float64, len(tw))
		for i := range tw {
			at[i] = tw[i] - tu[i]/c
		}
		return aw, at
	}
	evalF := func(W, TW []float64) *SpanReport {
		r := newSpanReport(tags)
		for i := range va {
			p := decode(&va[i], W, TW)
			r.add(va[i].gold, p)
		}
		return r.finish()
	}

	rng := &splitmix{cfg.Seed}
	order := identity(len(tr))
	bestF, bestEpoch := -1.0, 0
	var bestW, bestT []float64
	for epoch := 1; epoch <= cfg.MaxEpochs; epoch++ {
		rng.shuffle(order)
		mistakes := 0
		for _, idx := range order {
			e := &tr[idx]
			p := decode(e, w, tw)
			diff := false
			for i := range p {
				if p[i] != e.gold[i] {
					diff = true
					break
				}
			}
			if diff {
				mistakes++
				prevG, prevP := -1, -1
				for i := range p {
					g, q := int(e.gold[i]), int(p[i])
					if g != q {
						for _, b := range e.feats[e.foff[i]:e.foff[i+1]] {
							w[int(b)*T+g]++
							u[int(b)*T+g] += c
							w[int(b)*T+q]--
							u[int(b)*T+q] -= c
						}
					}
					if g != q || prevG != prevP {
						tw[(prevG+1)*T+g]++
						tu[(prevG+1)*T+g] += c
						tw[(prevP+1)*T+q]--
						tu[(prevP+1)*T+q] -= c
					}
					prevG, prevP = g, q
				}
			}
			c++
		}
		aw, at := avg()
		f := evalF(aw, at).MicroF1
		if cfg.Log != nil {
			fmt.Fprintf(cfg.Log, "epoch %d: %d/%d sentences wrong, valid span F1 %.4f\n", epoch, mistakes, len(tr), f)
		}
		if f > bestF {
			bestF, bestEpoch, bestW, bestT = f, epoch, aw, at
		} else if epoch-bestEpoch >= cfg.Patience {
			break
		}
		if mistakes == 0 {
			break
		}
	}

	// Cuantización: una escala común a emisiones y transiciones, porque
	// Viterbi las suma.
	maxAbs := 0.0
	for _, v := range bestW {
		maxAbs = math.Max(maxAbs, math.Abs(v))
	}
	for _, v := range bestT {
		maxAbs = math.Max(maxAbs, math.Abs(v))
	}
	scale := 1.0
	if maxAbs > 0 {
		scale = maxAbs / 32767
	}
	q := func(v float64) int16 {
		x := math.Round(v / scale)
		return int16(math.Max(-32767, math.Min(32767, x)))
	}
	m := &Model{Spec: cfg.Spec, Tags: tags, Lexicon: cfg.Lexicon, Scale: scale,
		W: make([]int16, B*T), Trans: make([]int16, (T+1)*T)}
	for i, v := range bestW {
		m.W[i] = q(v)
	}
	for i, v := range bestT {
		m.Trans[i] = q(v)
	}
	m.SpecHash = Hash(m.Spec, m.Tags, m.Lexicon)
	if err := m.Init(); err != nil {
		return nil, err
	}
	// Informe del modelo cuantizado (el que se sirve) y acuerdo con el float.
	qw := make([]float64, len(m.W))
	for i, v := range m.W {
		qw[i] = float64(v)
	}
	agree := 0
	rep := newSpanReport(tags)
	for i := range va {
		pf := append([]int16(nil), decode(&va[i], bestW, bestT)...)
		pq := decode(&va[i], qw, m.transF)
		rep.add(va[i].gold, pq)
		same := true
		for k := range pf {
			if pf[k] != pq[k] {
				same = false
				break
			}
		}
		if same {
			agree++
		}
	}
	res := &TrainResult{Model: m, BestEpoch: bestEpoch, Valid: rep.finish()}
	if len(va) > 0 {
		res.QuantAgreement = float64(agree) / float64(len(va))
	}
	m.Meta = Meta{CreatedAt: cfg.CreatedAt, DatasetSHA256: cfg.DatasetSHA256, TrainSentences: len(tr),
		ValidSentences: len(va), Epochs: bestEpoch, QuantAgreement: res.QuantAgreement,
		ValidMetrics: map[string]float64{"span_f1": res.Valid.MicroF1, "span_precision": res.Valid.MicroP, "span_recall": res.Valid.MicroR}}
	return res, nil
}

// SpanReport: precisión, cobertura y F1 de huecos exactos (mismo hueco, mismos
// tokens de inicio y fin), por hueco y en total (micro).
type SpanReport struct {
	PerSlot                 map[string]*SlotScore `json:"per_slot"`
	MicroP, MicroR, MicroF1 float64
	TP, FP, FN              int
	tags                    []string
}

// SlotScore son las cuentas de un hueco.
type SlotScore struct {
	TP, FP, FN int
	P, R, F1   float64
}

func newSpanReport(tags []string) *SpanReport {
	return &SpanReport{PerSlot: map[string]*SlotScore{}, tags: tags}
}

type tspan struct {
	slot       string
	start, end int
}

func tokenSpans(tags []string, seq []int16) []tspan {
	var out []tspan
	for i := 0; i < len(seq); i++ {
		t := tags[seq[i]]
		if t == "O" || t[0] != 'B' {
			continue
		}
		j := i + 1
		for j < len(seq) && tags[seq[j]] == "I-"+t[2:] {
			j++
		}
		out = append(out, tspan{t[2:], i, j})
	}
	return out
}

func (r *SpanReport) add(gold, pred []int16) {
	g, p := tokenSpans(r.tags, gold), tokenSpans(r.tags, pred)
	r.AddSpans(toKeys(g), toKeys(p))
}

func toKeys(s []tspan) [][3]string {
	out := make([][3]string, len(s))
	for i, x := range s {
		out[i] = [3]string{x.slot, fmt.Sprint(x.start), fmt.Sprint(x.end)}
	}
	return out
}

// AddSpans suma una frase: huecos de oro y predichos como (hueco, inicio, fin).
func (r *SpanReport) AddSpans(gold, pred [][3]string) {
	gs := map[[3]string]int{}
	for _, k := range gold {
		gs[k]++
	}
	for _, k := range pred {
		sc := r.slot(k[0])
		if gs[k] > 0 {
			gs[k]--
			sc.TP++
		} else {
			sc.FP++
		}
	}
	for k, n := range gs {
		r.slot(k[0]).FN += n
	}
}

func (r *SpanReport) slot(name string) *SlotScore {
	s := r.PerSlot[name]
	if s == nil {
		s = &SlotScore{}
		r.PerSlot[name] = s
	}
	return s
}

// NewSpanReport crea un informe vacío para sumar con AddSpans.
func NewSpanReport() *SpanReport { return newSpanReport(nil) }

func prf(tp, fp, fn int) (p, r, f float64) {
	if tp+fp > 0 {
		p = float64(tp) / float64(tp+fp)
	}
	if tp+fn > 0 {
		r = float64(tp) / float64(tp+fn)
	}
	if p+r > 0 {
		f = 2 * p * r / (p + r)
	}
	return
}

// Finish calcula las métricas a partir de las cuentas.
func (r *SpanReport) Finish() *SpanReport { return r.finish() }

func (r *SpanReport) finish() *SpanReport {
	r.TP, r.FP, r.FN = 0, 0, 0
	for _, s := range r.PerSlot {
		s.P, s.R, s.F1 = prf(s.TP, s.FP, s.FN)
		r.TP += s.TP
		r.FP += s.FP
		r.FN += s.FN
	}
	r.MicroP, r.MicroR, r.MicroF1 = prf(r.TP, r.FP, r.FN)
	return r
}

// SlotNamesSorted: los huecos del informe en orden.
func (r *SpanReport) SlotNamesSorted() []string {
	var out []string
	for k := range r.PerSlot {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func identity(n int) []int {
	p := make([]int, n)
	for i := range p {
		p[i] = i
	}
	return p
}

// splitmix64: el PRNG del entrenador de pkg/jev; igual en todas partes.
type splitmix struct{ s uint64 }

func (r *splitmix) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *splitmix) shuffle(p []int) {
	for i := len(p) - 1; i > 0; i-- {
		j := int(r.next() % uint64(i+1))
		p[i], p[j] = p[j], p[i]
	}
}
