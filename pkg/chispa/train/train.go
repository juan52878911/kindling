// Package train entrena modelos Chispa. Vive aparte de pkg/chispa para que quien solo
// sirve (un gateway, un daemon) no arrastre el entrenador, pero usa la MISMA
// extracción de características (chispa.FeatureSpec.Extract): si el entrenador
// tokenizara por su cuenta, cualquier diferencia sería deriva silenciosa.
//
// Optimizador: AdaGrad por ejemplo (SGD con paso adaptativo por parámetro),
// con L2 perezosa sobre los pesos tocados, pesos por clase y parada temprana
// por pérdida de validación. Por qué no L-BFGS: el gradiente completo y la
// historia de L-BFGS son vectores densos del tamaño del modelo (2^18 cubos ×
// 10 clases × m=10 pares × 2 = ~420 MB en float64), mientras que AdaGrad solo
// toca lo que aparece en cada ejemplo, necesita 2× el modelo y da un punto de
// control natural por época para la parada temprana. Y el paso por parámetro
// de AdaGrad es justo lo que pide el texto hasheado: los n-gramas raros dan
// pasos grandes y los frecuentes, pequeños.
//
// Determinista con la semilla: el barajado usa un splitmix64 propio, las
// características salen ordenadas, y toda la aritmética evita FMA (ver
// pkg/chispa/detmath.go), así que el mismo corpus y la misma semilla dan los
// mismos pesos también en otra arquitectura.
package train

import (
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"

	"github.com/juan52878911/kindling/pkg/chispa"
)

// Config son los hiperparámetros. Los ceros toman el valor por defecto.
type Config struct {
	Spec      chispa.FeatureSpec
	MaxEpochs int // 30
	Patience  int // épocas sin mejorar antes de parar: 3
	// LearningRate: paso de AdaGrad, 0.05. Ojo: el primer paso de AdaGrad mide
	// exactamente lr en cada peso tocado, sea cual sea el gradiente; con lr
	// alto y pocos miles de ejemplos, el modelo memoriza en la primera época.
	LearningRate    float64
	L2              float64 // 1e-4 por actualización; negativo = sin L2
	ClassWeight     string  // "balanced" (defecto), "sqrt" o "none"
	MaxClassWeight  float64 // tope del peso de una clase: 10
	Seed            uint64  // 1
	ValidFraction   float64 // si no hay validación aparte: 0.1
	TargetPrecision float64 // precisión del subconjunto confiado: 0.95
	MinSupport      int     // predicciones confiadas mínimas para fiarse de τ: 10
	DatasetSHA256   string  // para los metadatos
	CreatedAt       string  // RFC 3339; vacío = sin fecha (artefacto reproducible)
	// OneVsRest convierte datos multiclase en un binario «X contra el resto»:
	// el filtro típico («¿es un bug?») sin tener que reetiquetar el fichero.
	OneVsRest string
	Log       io.Writer
}

func (c *Config) defaults() {
	if c.Spec.Buckets == 0 {
		c.Spec = chispa.DefaultSpec()
	}
	if c.MaxEpochs <= 0 {
		c.MaxEpochs = 30
	}
	if c.Patience <= 0 {
		c.Patience = 3
	}
	if c.LearningRate <= 0 {
		c.LearningRate = 0.05
	}
	if c.L2 == 0 {
		c.L2 = 1e-4
	} else if c.L2 < 0 {
		c.L2 = 0
	}
	if c.ClassWeight == "" {
		c.ClassWeight = "balanced"
	}
	if c.MaxClassWeight <= 0 {
		c.MaxClassWeight = 10
	}
	if c.Seed == 0 {
		c.Seed = 1
	}
	if c.ValidFraction <= 0 || c.ValidFraction >= 1 {
		c.ValidFraction = 0.1
	}
	if c.TargetPrecision <= 0 || c.TargetPrecision > 1 {
		c.TargetPrecision = 0.95
	}
	if c.MinSupport <= 0 {
		c.MinSupport = 10
	}
}

// EpochStat es el progreso de una época.
type EpochStat struct {
	Epoch     int
	TrainLoss float64
	ValidLoss float64
}

// Result es lo que devuelve Train.
type Result struct {
	Model     *chispa.Model
	BestEpoch int
	Epochs    []EpochStat
	// QuantAgreement: fracción de validación en la que el modelo int16 predice
	// la misma etiqueta que el float64 del que sale.
	QuantAgreement float64
	Valid          *chispa.Report

	// Los pesos en float64 antes de cuantizar, para medir la concordancia en
	// cualquier conjunto (Agreement).
	floatW, floatB []float64
	labelIdx       map[string]int
}

// Agreement es la fracción de exs en la que el modelo cuantizado (int16, el que
// se sirve) predice la misma etiqueta que el float64 del que sale.
func (r *Result) Agreement(exs []chispa.Example) float64 {
	if len(exs) == 0 {
		return 1
	}
	m := r.Model
	K := m.NumOutputs()
	d := buildDataset(m.Spec, exs, r.labelIdx)
	z := make([]float64, K)
	agree := 0
	for i := 0; i < d.n(); i++ {
		copy(z, r.floatB)
		for j := d.start[i]; j < d.start[i+1]; j++ {
			row := r.floatW[int(d.idx[j])*K : int(d.idx[j])*K+K]
			for k := range row {
				z[k] += float64(d.x[j] * row[k])
			}
		}
		if argmax(m.RawLogits(exs[i].Input(), nil)) == argmaxOut(z, m.Binary) {
			agree++
		}
	}
	return float64(agree) / float64(len(exs))
}

// dataset son los ejemplos ya convertidos en vectores dispersos.
type dataset struct {
	start []int     // ejemplo i: características start[i]..start[i+1]
	idx   []uint32  // cubo
	x     []float64 // valor (ya normalizado)
	y     []int     // etiqueta; -1 si no está en el entrenamiento
}

func (d *dataset) n() int { return len(d.y) }

func buildDataset(spec chispa.FeatureSpec, exs []chispa.Example, labelIdx map[string]int) *dataset {
	d := &dataset{start: make([]int, 1, len(exs)+1)}
	for _, ex := range exs {
		feats, nText := spec.Extract(ex.Input())
		inv := 0.0
		if nText > 0 {
			inv = 1 / math.Sqrt(float64(nText))
		}
		for _, f := range feats {
			v := float64(f.Value)
			if f.Text {
				v = float64(v * inv)
			}
			d.idx = append(d.idx, f.Bucket)
			d.x = append(d.x, v)
		}
		d.start = append(d.start, len(d.idx))
		y, ok := labelIdx[ex.Label]
		if !ok {
			y = -1
		}
		d.y = append(d.y, y)
	}
	return d
}

// splitmix64: generador pequeño y estable. math/rand podría cambiar de
// algoritmo entre versiones de Go; este no.
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

// SplitValidation separa una fracción de validación por hash del contenido:
// estable aunque cambie el orden del fichero, y un duplicado exacto cae
// siempre del mismo lado.
func SplitValidation(exs []chispa.Example, frac float64, seed uint64) (train, valid []chispa.Example) {
	cut := uint64(frac * 10000)
	for _, ex := range exs {
		h := chispa.FNV1a64(ex.Label + "\x00" + ex.Text)
		r := splitmix{s: h ^ seed}
		if r.next()%10000 < cut {
			valid = append(valid, ex)
		} else {
			train = append(train, ex)
		}
	}
	return train, valid
}

// Train entrena, cuantiza, calibra y elige umbrales. validEx puede ir vacío: se
// separa cfg.ValidFraction del entrenamiento.
func Train(trainEx, validEx []chispa.Example, cfg Config) (*Result, error) {
	cfg.defaults()
	if err := cfg.Spec.Validate(); err != nil {
		return nil, err
	}
	switch cfg.ClassWeight {
	case "balanced", "sqrt", "none":
	default:
		return nil, fmt.Errorf("unknown class weighting %q (balanced, sqrt, none)", cfg.ClassWeight)
	}
	if cfg.OneVsRest != "" {
		trainEx, validEx = relabel(trainEx, cfg.OneVsRest), relabel(validEx, cfg.OneVsRest)
	}
	if len(validEx) == 0 {
		trainEx, validEx = SplitValidation(trainEx, cfg.ValidFraction, cfg.Seed)
	}
	if len(trainEx) == 0 || len(validEx) == 0 {
		return nil, errors.New("need training and validation examples")
	}
	logf := func(format string, a ...any) {
		if cfg.Log != nil {
			fmt.Fprintf(cfg.Log, format, a...)
		}
	}

	// Etiquetas ordenadas: el índice de cada una no depende del orden del fichero.
	counts := map[string]int{}
	for _, ex := range trainEx {
		counts[ex.Label]++
	}
	labels := make([]string, 0, len(counts))
	for l := range counts {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	if len(labels) < 2 {
		return nil, errors.New("need at least 2 distinct labels")
	}
	if len(labels) > chispa.MaxLabels {
		return nil, fmt.Errorf("too many labels: %d (max %d)", len(labels), chispa.MaxLabels)
	}
	labelIdx := make(map[string]int, len(labels))
	for i, l := range labels {
		labelIdx[l] = i
	}
	binary := len(labels) == 2
	K := len(labels)
	if binary {
		K = 1
	}
	B := int(cfg.Spec.Buckets)

	tr := buildDataset(cfg.Spec, trainEx, labelIdx)
	va := buildDataset(cfg.Spec, validEx, labelIdx)

	// Peso por clase: "balanced" = N/(L·n_c), el de scikit-learn; "sqrt" su
	// raíz, más suave. Con tope: una clase con 3 ejemplos no debe pesar 50×.
	cw := make([]float64, len(labels))
	for c, l := range labels {
		w := 1.0
		if cfg.ClassWeight != "none" {
			w = float64(len(trainEx)) / float64(len(labels)*counts[l])
			if cfg.ClassWeight == "sqrt" {
				w = math.Sqrt(w)
			}
		}
		cw[c] = math.Min(w, cfg.MaxClassWeight)
	}

	W := make([]float64, B*K)
	G := make([]float64, B*K)
	bias := make([]float64, K)
	Gb := make([]float64, K)
	bestW := make([]float64, B*K)
	bestB := make([]float64, K)
	z := make([]float64, K)
	p := make([]float64, len(labels))
	g := make([]float64, K)
	lr, l2 := cfg.LearningRate, cfg.L2
	const eps = 1e-8

	// forward calcula las logits del ejemplo i de d en z.
	forward := func(d *dataset, i int, W, bias []float64) {
		copy(z, bias)
		for j := d.start[i]; j < d.start[i+1]; j++ {
			row := W[int(d.idx[j])*K : int(d.idx[j])*K+K]
			x := d.x[j]
			for k := range row {
				z[k] += float64(x * row[k])
			}
		}
	}
	// loss devuelve -log p(y) y deja en g el gradiente respecto a z (sin peso).
	loss := func(y int) float64 {
		if binary {
			pp := sigmoid(z[0])
			t := 0.0
			if y == 1 {
				t = 1
			}
			g[0] = pp - t
			if y == 1 {
				return -safeLog(pp)
			}
			return -safeLog(1 - pp)
		}
		best := 0
		for k := 1; k < K; k++ {
			if z[k] > z[best] {
				best = k
			}
		}
		sum := 0.0
		for k := range z {
			p[k] = detExpF(z[k] - z[best])
			sum += p[k]
		}
		for k := range z {
			p[k] /= sum
			g[k] = p[k]
		}
		g[y] -= 1
		return -safeLog(p[y])
	}
	validLoss := func(W, bias []float64) float64 {
		sum, wsum := 0.0, 0.0
		for i := 0; i < va.n(); i++ {
			y := va.y[i]
			if y < 0 {
				continue
			}
			forward(va, i, W, bias)
			sum += float64(cw[y] * loss(y))
			wsum += cw[y]
		}
		if wsum == 0 {
			return 0
		}
		return sum / wsum
	}

	rng := splitmix{s: cfg.Seed}
	order := make([]int, tr.n())
	for i := range order {
		order[i] = i
	}
	best, bestEpoch, since := math.Inf(1), 0, 0
	var stats []EpochStat
	for epoch := 1; epoch <= cfg.MaxEpochs; epoch++ {
		rng.shuffle(order)
		tl, tw := 0.0, 0.0
		for _, i := range order {
			y := tr.y[i]
			forward(tr, i, W, bias)
			tl += float64(cw[y] * loss(y))
			tw += cw[y]
			for k := range g {
				g[k] = float64(g[k] * cw[y])
			}
			for j := tr.start[i]; j < tr.start[i+1]; j++ {
				base := int(tr.idx[j]) * K
				x := tr.x[j]
				for k := 0; k < K; k++ {
					gr := float64(g[k]*x) + float64(l2*W[base+k])
					G[base+k] += float64(gr * gr)
					W[base+k] -= float64(lr*gr) / (math.Sqrt(G[base+k]) + eps)
				}
			}
			for k := 0; k < K; k++ {
				Gb[k] += float64(g[k] * g[k])
				bias[k] -= float64(lr*g[k]) / (math.Sqrt(Gb[k]) + eps)
			}
		}
		vl := validLoss(W, bias)
		stats = append(stats, EpochStat{Epoch: epoch, TrainLoss: tl / tw, ValidLoss: vl})
		logf("epoch %2d  train loss %.4f  valid loss %.4f\n", epoch, tl/tw, vl)
		if vl < best {
			best, bestEpoch, since = vl, epoch, 0
			copy(bestW, W)
			copy(bestB, bias)
		} else if since++; since >= cfg.Patience {
			break
		}
	}
	logf("best epoch %d (valid loss %.4f)\n", bestEpoch, best)

	m, err := quantize(bestW, bestB, B, K, labels, binary, cfg.Spec)
	if err != nil {
		return nil, err
	}

	// Concordancia float/int16 en validación.
	agree := 0
	raw := make([][]float64, va.n())
	gold := make([]int, va.n())
	for i := 0; i < va.n(); i++ {
		forward(va, i, bestW, bestB)
		fl := argmaxOut(z, binary)
		raw[i] = m.RawLogits(validEx[i].Input(), nil)
		if argmax(raw[i]) == fl {
			agree++
		}
		gold[i] = va.y[i]
	}
	res := &Result{BestEpoch: bestEpoch, Epochs: stats, QuantAgreement: float64(agree) / float64(va.n()),
		floatW: bestW, floatB: bestB, labelIdx: labelIdx}

	m.Temperature = chispa.FitTemperature(raw, gold)
	pred := make([]int, va.n())
	prob := make([]float64, va.n())
	for i := range validEx {
		pr := m.Predict(validEx[i].Input())
		pred[i], prob[i] = pr.Index, pr.Prob
	}
	m.Thresholds = chispa.ChooseThresholds(len(labels), pred, gold, prob,
		chispa.ThresholdParams{TargetPrecision: cfg.TargetPrecision, MinSupport: cfg.MinSupport})
	res.Valid = chispa.Evaluate(m, validEx)

	m.Meta = chispa.Meta{
		CreatedAt:       cfg.CreatedAt,
		OneVsRest:       cfg.OneVsRest,
		DatasetSHA256:   cfg.DatasetSHA256,
		TrainExamples:   len(trainEx),
		ValidExamples:   len(validEx),
		ClassCounts:     counts,
		Optimizer:       "adagrad",
		Epochs:          bestEpoch,
		TargetPrecision: cfg.TargetPrecision,
		QuantAgreement:  res.QuantAgreement,
		Params: map[string]string{
			"learning_rate":    strconv.FormatFloat(cfg.LearningRate, 'g', -1, 64),
			"l2":               strconv.FormatFloat(cfg.L2, 'g', -1, 64),
			"class_weight":     cfg.ClassWeight,
			"max_class_weight": strconv.FormatFloat(cfg.MaxClassWeight, 'g', -1, 64),
			"seed":             strconv.FormatUint(cfg.Seed, 10),
			"max_epochs":       strconv.Itoa(cfg.MaxEpochs),
			"patience":         strconv.Itoa(cfg.Patience),
			"min_support":      strconv.Itoa(cfg.MinSupport),
		},
		ValidMetrics: map[string]float64{
			"accuracy":            res.Valid.Accuracy,
			"macro_f1":            res.Valid.MacroF1,
			"ece":                 res.Valid.ECE,
			"coverage":            res.Valid.Coverage,
			"confident_precision": res.Valid.ConfidentPrecision,
			"temperature":         m.Temperature,
		},
	}
	if err := m.Init(); err != nil {
		return nil, err
	}
	res.Model = m
	return res, nil
}

// quantize pasa los pesos a int16 con una escala por salida: s_k = max|w_k| /
// 32767. Por salida y no global porque las clases tienen magnitudes muy
// distintas y una escala común dejaría a la pequeña con pocos niveles.
func quantize(W, bias []float64, B, K int, labels []string, binary bool, spec chispa.FeatureSpec) (*chispa.Model, error) {
	for _, w := range W {
		if math.IsNaN(w) || math.IsInf(w, 0) {
			return nil, errors.New("training diverged (non-finite weights): lower the learning rate")
		}
	}
	scales := make([]float64, K)
	for k := 0; k < K; k++ {
		mx := 0.0
		for b := 0; b < B; b++ {
			mx = math.Max(mx, math.Abs(W[b*K+k]))
		}
		scales[k] = mx / 32767
	}
	q := make([]int16, B*K)
	for i, w := range W {
		s := scales[i%K]
		if s == 0 {
			continue
		}
		v := math.Round(w / s)
		q[i] = int16(math.Max(-32767, math.Min(32767, v)))
	}
	th := make([]float64, len(labels))
	for i := range th {
		th[i] = chispa.NeverConfident
	}
	m := &chispa.Model{
		Spec: spec, SpecHash: spec.Hash(), Labels: labels, Binary: binary,
		Scales: scales, Bias: append([]float64(nil), bias...), W: q,
		Temperature: 1, Thresholds: th,
	}
	return m, m.Init()
}

func argmax(z []float64) int {
	b := 0
	for k := 1; k < len(z); k++ {
		if z[k] > z[b] {
			b = k
		}
	}
	return b
}

func argmaxOut(z []float64, binary bool) int {
	if binary {
		if z[0] > 0 {
			return 1
		}
		return 0
	}
	return argmax(z)
}

func sigmoid(x float64) float64 {
	if x >= 0 {
		return 1 / (1 + detExpF(-x))
	}
	e := detExpF(x)
	return e / (1 + e)
}

func safeLog(p float64) float64 {
	return chispa.DetLog(math.Max(p, 1e-15))
}

func detExpF(x float64) float64 { return chispa.DetExp(x) }

// relabel copia exs con toda etiqueta distinta de pos cambiada a RestLabel(pos).
func relabel(exs []chispa.Example, pos string) []chispa.Example {
	out := make([]chispa.Example, len(exs))
	for i, ex := range exs {
		if ex.Label != pos {
			ex.Label = chispa.RestLabel(pos)
		}
		out[i] = ex
	}
	return out
}
