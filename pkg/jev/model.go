package jev

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
)

// Decisiones frente al umbral. Son cadenas porque viajan tal cual en JSON al
// gateway de la cascada.
const (
	DecisionConfident = "confident" // p >= τ de la clase: JEV contesta
	DecisionEscalate  = "escalate"  // p < τ: que decida un modelo mayor
)

// NeverConfident es el umbral de una clase para la que el entrenamiento no
// encontró un corte que diera la precisión pedida: siempre escala.
const NeverConfident = 2.0

// GuestPort es el puerto en el que kling-jev (cmd/kling-jev) sirve
// /v1/classify dentro de una microVM, cuando una tarea se despliega como
// backend "microvm" (docs/jev-serverless.md) en vez de en el propio proceso
// del gateway. Es el mismo número que von.Port: los puertos de una microVM no
// chocan entre máquinas distintas, así que cualquier invitado puede usar 8000
// sin coordinarse con los demás.
const GuestPort = 8000

// Model es un clasificador lineal cuantizado. Es inmutable tras cargarlo o
// construirlo, así que Predict se puede llamar desde muchas goroutines a la vez.
type Model struct {
	Spec     FeatureSpec
	SpecHash uint64
	Labels   []string
	// Binary: una sola salida (la logit de Labels[1] contra Labels[0]). Es la
	// regresión logística de siempre; se sirve como softmax de [0, z], que es
	// exactamente la sigmoide.
	Binary bool
	// Scales[k] convierte la suma entera de la salida k en logit.
	Scales []float64
	Bias   []float64
	// W está ordenado por cubo: W[b*nOut+k]. Una característica toca nOut
	// int16 contiguos, una sola línea de caché.
	W []int16
	// Temperature divide las logits antes de la softmax (calibración).
	Temperature float64
	// Thresholds[c] es τ de la etiqueta c: por debajo, se escala.
	Thresholds []float64
	Meta       Meta

	nOut int
	pool sync.Pool
}

// Meta es lo que se sabe del entrenamiento. No afecta a la inferencia; se
// guarda para que `kling jev inspect` pueda contar de dónde sale un modelo.
type Meta struct {
	CreatedAt       string             `json:"created_at,omitempty"`
	DatasetSHA256   string             `json:"dataset_sha256,omitempty"`
	TrainExamples   int                `json:"train_examples,omitempty"`
	ValidExamples   int                `json:"valid_examples,omitempty"`
	ClassCounts     map[string]int     `json:"class_counts,omitempty"`
	Optimizer       string             `json:"optimizer,omitempty"`
	Params          map[string]string  `json:"params,omitempty"`
	Epochs          int                `json:"epochs,omitempty"`
	TargetPrecision float64            `json:"target_precision,omitempty"`
	QuantAgreement  float64            `json:"quant_agreement,omitempty"`
	ValidMetrics    map[string]float64 `json:"valid_metrics,omitempty"`
	// OneVsRest: el modelo es binario «X contra el resto» sacado de datos
	// multiclase; al evaluar, toda etiqueta que no sea X cuenta como RestLabel(X).
	OneVsRest string `json:"one_vs_rest,omitempty"`
	Notes     string `json:"notes,omitempty"`
}

// RestLabel es el nombre de la clase «el resto» de un modelo uno-contra-resto.
func RestLabel(positive string) string { return "not-" + positive }

// GoldIndex es el índice de la etiqueta correcta de un ejemplo, o -1 si el
// modelo no la conoce.
func (m *Model) GoldIndex(label string) int {
	if m.Meta.OneVsRest != "" && label != m.Meta.OneVsRest {
		label = RestLabel(m.Meta.OneVsRest)
	}
	for i, l := range m.Labels {
		if l == label {
			return i
		}
	}
	return -1
}

// NumOutputs es cuántas filas de pesos tiene el modelo: 1 en binario, una por
// etiqueta si no.
func (m *Model) NumOutputs() int { return m.nOut }

// Init comprueba la coherencia interna y prepara el modelo para servir. La
// llaman el cargador y el entrenador; quien construya un Model a mano también.
func (m *Model) Init() error {
	if err := m.Spec.Validate(); err != nil {
		return err
	}
	if h := m.Spec.Hash(); h != m.SpecHash {
		return fmt.Errorf("feature spec hash mismatch: file says %016x, spec hashes to %016x", m.SpecHash, h)
	}
	nl := len(m.Labels)
	if nl < 2 || nl > MaxLabels {
		return fmt.Errorf("need between 2 and %d labels, got %d", MaxLabels, nl)
	}
	seen := make(map[string]bool, nl)
	for _, l := range m.Labels {
		if l == "" || len(l) > MaxLabelBytes || seen[l] {
			return fmt.Errorf("invalid or duplicate label %q", l)
		}
		seen[l] = true
	}
	m.nOut = nl
	if m.Binary {
		if nl != 2 {
			return errors.New("binary model needs exactly 2 labels")
		}
		m.nOut = 1
	}
	if len(m.Scales) != m.nOut || len(m.Bias) != m.nOut {
		return errors.New("scales/bias length does not match outputs")
	}
	if len(m.W) != int(m.Spec.Buckets)*m.nOut {
		return errors.New("weights length does not match buckets × outputs")
	}
	for k := 0; k < m.nOut; k++ {
		if !finite(m.Scales[k]) || m.Scales[k] < 0 || !finite(m.Bias[k]) || math.Abs(m.Bias[k]) > 1e6 {
			return errors.New("non-finite or out-of-range scale/bias")
		}
	}
	if !finite(m.Temperature) || m.Temperature < 1e-3 || m.Temperature > 1e3 {
		return fmt.Errorf("temperature out of range: %v", m.Temperature)
	}
	if len(m.Thresholds) != nl {
		return errors.New("thresholds length does not match labels")
	}
	for _, t := range m.Thresholds {
		if !finite(t) || t < 0 || t > NeverConfident {
			return fmt.Errorf("threshold out of range: %v", t)
		}
	}
	return nil
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// scratch son los búferes de una predicción. Van en un sync.Pool: el modelo es
// compartido y cada llamada concurrente toma los suyos.
type scratch struct {
	ex     extractor
	accT   []int64
	accF   []int64
	logits []float64
	probs  []float64
}

func (m *Model) getScratch() *scratch {
	if s, ok := m.pool.Get().(*scratch); ok {
		return s
	}
	nl := len(m.Labels)
	return &scratch{
		accT:   make([]int64, m.nOut),
		accF:   make([]int64, m.nOut),
		logits: make([]float64, nl),
		probs:  make([]float64, nl),
	}
}

// Prediction es la respuesta de JEV, pensada para la cascada: si Confident, el
// gateway usa Label; si no, escala con la entrada (y puede pasar Probs y
// Evidence como pista al modelo mayor).
type Prediction struct {
	Label     string  `json:"label"`
	Index     int     `json:"index"`
	Prob      float64 `json:"prob"`      // calibrada
	Threshold float64 `json:"threshold"` // τ de Label
	Confident bool    `json:"confident"`
	Decision  string  `json:"decision"` // DecisionConfident | DecisionEscalate
	// Solo con PredictFull:
	Probs    []ClassProb `json:"probs,omitempty"`
	Evidence []Evidence  `json:"evidence,omitempty"`
}

// ClassProb es la probabilidad calibrada de una etiqueta.
type ClassProb struct {
	Label string  `json:"label"`
	Prob  float64 `json:"prob"`
}

// Evidence es cuánto empuja una característica la logit de la etiqueta
// predicha (w·x, antes de la temperatura). Positivo = a favor.
type Evidence struct {
	Feature string  `json:"feature"`
	Weight  float64 `json:"weight"`
}

// Predict clasifica in. No reserva memoria (salvo la primera vez de cada
// búfer del pool) y es segura para uso concurrente.
func (m *Model) Predict(in Input) Prediction {
	s := m.getScratch()
	m.Spec.extract(&s.ex, in.Text, in.Fields, false)
	p := m.decide(s)
	m.pool.Put(s)
	return p
}

// PredictFull es Predict con todas las probabilidades y las top características
// a favor de la etiqueta elegida. Reserva memoria: es para depurar, para la
// CLI y para pasarle contexto al modelo al que se escala.
func (m *Model) PredictFull(in Input, topFeatures int) Prediction {
	s := m.getScratch()
	m.Spec.extract(&s.ex, in.Text, in.Fields, topFeatures > 0)
	p := m.decide(s)
	p.Probs = make([]ClassProb, len(m.Labels))
	for c, l := range m.Labels {
		p.Probs[c] = ClassProb{Label: l, Prob: s.probs[c]}
	}
	sort.SliceStable(p.Probs, func(i, j int) bool { return p.Probs[i].Prob > p.Probs[j].Prob })
	if topFeatures > 0 {
		p.Evidence = m.evidence(&s.ex, p.Index, topFeatures)
	}
	s.ex.names = nil
	m.pool.Put(s)
	return p
}

// RawLogits devuelve las logits de in sin calibrar (temperatura 1), una por
// etiqueta. Las usa el entrenador para ajustar la temperatura sobre el modelo
// ya cuantizado, que es el que se sirve.
func (m *Model) RawLogits(in Input, dst []float64) []float64 {
	s := m.getScratch()
	m.Spec.extract(&s.ex, in.Text, in.Fields, false)
	m.logits(s, 1)
	dst = append(dst[:0], s.logits...)
	m.pool.Put(s)
	return dst
}

func (m *Model) decide(s *scratch) Prediction {
	m.logits(s, m.Temperature)
	best := softmaxInto(s.logits, s.probs)
	p := Prediction{
		Label:     m.Labels[best],
		Index:     best,
		Prob:      s.probs[best],
		Threshold: m.Thresholds[best],
	}
	p.Confident = p.Prob >= p.Threshold
	p.Decision = DecisionEscalate
	if p.Confident {
		p.Decision = DecisionConfident
	}
	return p
}

// logits calcula s.logits a partir de s.ex.feats: acumulación entera (int64,
// orden irrelevante porque la suma entera es asociativa) y una sola pasada en
// coma flotante al final, sin FMA posibles:
//
//	z_k = ((accT_k · 1/√nText) + accF_k) · scale_k + bias_k,  luego / T
func (m *Model) logits(s *scratch, temp float64) {
	K := m.nOut
	for k := 0; k < K; k++ {
		s.accT[k], s.accF[k] = 0, 0
	}
	W := m.W
	for _, f := range s.ex.feats {
		row := W[int(f.bucket)*K : int(f.bucket)*K+K]
		acc := s.accF
		if f.text {
			acc = s.accT
		}
		if f.sign > 0 {
			for k, w := range row {
				acc[k] += int64(w)
			}
		} else {
			for k, w := range row {
				acc[k] -= int64(w)
			}
		}
	}
	inv := invNorm(s.ex.nText)
	for k := 0; k < K; k++ {
		x := float64(float64(s.accT[k])*inv) + float64(s.accF[k])
		s.logits[k] = float64(x*m.Scales[k]) + m.Bias[k]
	}
	if m.Binary {
		s.logits[1] = s.logits[0]
		s.logits[0] = 0
	}
	if temp != 1 {
		for k := range s.logits {
			s.logits[k] /= temp
		}
	}
}

// invNorm es 1/√n: el texto se normaliza por su número de apariciones para que
// un mensaje largo no tenga logits más extremas solo por ser largo. sqrt y la
// división son operaciones IEEE con redondeo exacto: iguales en todas partes.
func invNorm(n int) float64 {
	if n == 0 {
		return 0
	}
	return 1 / math.Sqrt(float64(n))
}

// evidence suma, por nombre de característica, su aportación a la logit de la
// etiqueta c y devuelve las n más positivas.
func (m *Model) evidence(e *extractor, c, n int) []Evidence {
	K := m.nOut
	out, sign := c, 1.0
	if m.Binary {
		out = 0
		if c == 0 {
			sign = -1
		}
	}
	inv := invNorm(e.nText)
	agg := map[string]float64{}
	for i, f := range e.feats {
		w := float64(m.W[int(f.bucket)*K+out]) * m.Scales[out] * float64(f.sign) * sign
		if f.text {
			w *= inv
		}
		agg[e.names[i]] += w
	}
	ev := make([]Evidence, 0, len(agg))
	for name, w := range agg {
		if w > 0 {
			ev = append(ev, Evidence{Feature: name, Weight: w})
		}
	}
	sort.Slice(ev, func(i, j int) bool {
		if ev[i].Weight != ev[j].Weight {
			return ev[i].Weight > ev[j].Weight
		}
		return ev[i].Feature < ev[j].Feature
	})
	if len(ev) > n {
		ev = ev[:n]
	}
	return ev
}
