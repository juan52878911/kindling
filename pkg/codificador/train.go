package codificador

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/juan52878911/kindling/pkg/jev"
)

// TrainConfig es cómo entrenar una cabeza.
type TrainConfig struct {
	Hidden int     // 0 = regresión logística multinomial
	Epochs int     // máximo (parada temprana por macro-F1 de validación)
	LR     float64 // Adam
	L2     float64 // decaimiento de los pesos (no de los sesgos)
	Batch  int
	Seed   uint64
	// ClassWeight: "sqrt" (por defecto: 1/√frecuencia, como JEV) o "none".
	ClassWeight string
	Patience    int
	// TargetPrecision y MinSupport eligen el umbral τ de cada clase en
	// validación (jev.ChooseThresholds).
	TargetPrecision float64
	MinSupport      int
	// ThresholdMask, si no es nil, dice qué filas de validación cuentan para
	// los umbrales: las que de verdad llegan a esta capa (lo que la capa
	// anterior escala), que son más difíciles que la media. Temperatura y
	// parada temprana usan toda la validación.
	ThresholdMask []bool
	Meta          Meta
	Log           io.Writer
}

func (c *TrainConfig) defaults() {
	if c.Epochs == 0 {
		c.Epochs = 60
	}
	if c.LR == 0 {
		c.LR = 2e-3
	}
	if c.Batch == 0 {
		c.Batch = 64
	}
	if c.Seed == 0 {
		c.Seed = 1
	}
	if c.ClassWeight == "" {
		c.ClassWeight = "sqrt"
	}
	if c.Patience == 0 {
		c.Patience = 8
	}
	if c.TargetPrecision == 0 {
		c.TargetPrecision = 0.95
	}
	if c.MinSupport == 0 {
		c.MinSupport = 5
	}
}

// Dataset son vectores del codificador con su etiqueta (índice en Labels).
type Dataset struct {
	X [][]float32
	Y []int
}

// TrainResult es la cabeza y lo que se midió al entrenarla.
type TrainResult struct {
	Head          *Head
	BestEpoch     int
	ValidAccuracy float64 // con la cabeza cuantizada
	ValidMacroF1  float64
	Agreement     float64 // argmax cuantizado = argmax en float, en validación
}

// splitmix64: el generador de JEV. Determinista y el mismo en todas partes.
type splitmix struct{ s uint64 }

func (r *splitmix) next() uint64 {
	r.s += 0x9e3779b97f4a7c15
	z := r.s
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *splitmix) uniform() float64 { return float64(r.next()>>11) / (1 << 53) }

func (r *splitmix) shuffle(p []int) {
	for i := len(p) - 1; i > 0; i-- {
		j := int(r.next() % uint64(i+1))
		p[i], p[j] = p[j], p[i]
	}
}

// params son los pesos en float64 durante el entrenamiento.
type params struct {
	D, H, K   int
	W1, B1    []float64
	W2, B2    []float64
	mW1, vW1  []float64 // momentos de Adam
	mB1, vB1  []float64
	mW2, vW2  []float64
	mB2, vB2  []float64
	gW1, gB1  []float64
	gW2, gB2  []float64
	hid, dhid []float64
	z, p, dz  []float64
	in        int
	adamT     int
	lr, l2    float64
}

func newParams(D, H, K int, rng *splitmix, lr, l2 float64) *params {
	in := D
	if H > 0 {
		in = H
	}
	p := &params{D: D, H: H, K: K, in: in, lr: lr, l2: l2}
	mk := func(n int) []float64 { return make([]float64, n) }
	if H > 0 {
		p.W1, p.B1 = mk(H*D), mk(H)
		p.mW1, p.vW1, p.mB1, p.vB1 = mk(H*D), mk(H*D), mk(H), mk(H)
		p.gW1, p.gB1 = mk(H*D), mk(H)
		p.hid, p.dhid = mk(H), mk(H)
		// Xavier uniforme: sin él, ReLU con pesos a cero no aprende nada.
		a := math.Sqrt(6 / float64(D+H))
		for i := range p.W1 {
			p.W1[i] = float64(float64(2*rng.uniform()-1) * a)
		}
		a2 := math.Sqrt(6 / float64(H+K))
		p.W2 = mk(K * in)
		for i := range p.W2 {
			p.W2[i] = float64(float64(2*rng.uniform()-1) * a2)
		}
	} else {
		p.W2 = mk(K * in) // la logística converge igual desde cero
	}
	p.B2 = mk(K)
	p.mW2, p.vW2, p.mB2, p.vB2 = mk(K*in), mk(K*in), mk(K), mk(K)
	p.gW2, p.gB2 = mk(K*in), mk(K)
	p.z, p.p, p.dz = mk(K), mk(K), mk(K)
	return p
}

// forward deja en p.z las logits de x (y en p.hid la capa oculta).
func (p *params) forward(x []float64) {
	in := x
	if p.H > 0 {
		for j := 0; j < p.H; j++ {
			row := p.W1[j*p.D : (j+1)*p.D]
			s := p.B1[j]
			for d, w := range row {
				s += float64(w * x[d])
			}
			if s < 0 {
				s = 0
			}
			p.hid[j] = s
		}
		in = p.hid
	}
	for k := 0; k < p.K; k++ {
		row := p.W2[k*p.in : (k+1)*p.in]
		s := p.B2[k]
		for d, w := range row {
			s += float64(w * in[d])
		}
		p.z[k] = s
	}
}

// backward acumula el gradiente de la pérdida (entropía cruzada con peso w)
// de un ejemplo ya pasado por forward.
func (p *params) backward(x []float64, y int, w float64) float64 {
	softmaxT(p.z, 1, p.p)
	loss := -w * jev.DetLog(math.Max(p.p[y], 1e-300))
	for k := range p.dz {
		g := p.p[k]
		if k == y {
			g -= 1
		}
		p.dz[k] = float64(g * w)
	}
	in := x
	if p.H > 0 {
		in = p.hid
		for j := range p.dhid {
			p.dhid[j] = 0
		}
	}
	for k := 0; k < p.K; k++ {
		g := p.dz[k]
		p.gB2[k] += g
		row := p.W2[k*p.in : (k+1)*p.in]
		grow := p.gW2[k*p.in : (k+1)*p.in]
		for d := range grow {
			grow[d] += float64(g * in[d])
			if p.H > 0 {
				p.dhid[d] += float64(g * row[d])
			}
		}
	}
	if p.H > 0 {
		for j := 0; j < p.H; j++ {
			if p.hid[j] <= 0 {
				continue // ReLU apagada: no pasa gradiente
			}
			g := p.dhid[j]
			p.gB1[j] += g
			grow := p.gW1[j*p.D : (j+1)*p.D]
			for d := range grow {
				grow[d] += float64(g * x[d])
			}
		}
	}
	return loss
}

// step aplica Adam con el gradiente medio del lote y lo pone a cero.
func (p *params) step(n int) {
	p.adamT++
	const beta1, beta2, eps = 0.9, 0.999, 1e-8
	c1 := 1 - math.Pow(beta1, float64(p.adamT))
	c2 := 1 - math.Pow(beta2, float64(p.adamT))
	inv := 1 / float64(n)
	upd := func(w, g, m, v []float64, decay bool) {
		for i := range w {
			gi := float64(g[i] * inv)
			if decay {
				gi += float64(p.l2 * w[i])
			}
			m[i] = float64(beta1*m[i]) + float64((1-beta1)*gi)
			v[i] = float64(beta2*v[i]) + float64(float64((1-beta2)*gi)*gi)
			w[i] -= float64(p.lr*(m[i]/c1)) / (math.Sqrt(v[i]/c2) + eps)
			g[i] = 0
		}
	}
	if p.H > 0 {
		upd(p.W1, p.gW1, p.mW1, p.vW1, true)
		upd(p.B1, p.gB1, p.mB1, p.vB1, false)
	}
	upd(p.W2, p.gW2, p.mW2, p.vW2, true)
	upd(p.B2, p.gB2, p.mB2, p.vB2, false)
}

func (p *params) clone() *params {
	c := *p
	cp := func(s []float64) []float64 { return append([]float64(nil), s...) }
	c.W1, c.B1, c.W2, c.B2 = cp(p.W1), cp(p.B1), cp(p.W2), cp(p.B2)
	return &c
}

// Train entrena una cabeza sobre vectores ya calculados.
func Train(labels []string, train, valid Dataset, cfg TrainConfig) (*TrainResult, error) {
	cfg.defaults()
	K := len(labels)
	if K < 2 {
		return nil, errors.New("need at least two labels")
	}
	if len(train.X) == 0 || len(train.X) != len(train.Y) || len(valid.X) != len(valid.Y) {
		return nil, errors.New("empty or mismatched training data")
	}
	if len(valid.X) == 0 {
		return nil, errors.New("a validation set is needed (early stopping, temperature and thresholds)")
	}
	if cfg.ThresholdMask != nil && len(cfg.ThresholdMask) != len(valid.X) {
		return nil, errors.New("threshold mask does not match the validation set")
	}
	D := len(train.X[0])
	for _, ds := range []Dataset{train, valid} {
		for i, v := range ds.X {
			if len(v) != D {
				return nil, fmt.Errorf("row %d: vector of %d dimensions, expected %d", i, len(v), D)
			}
			if ds.Y[i] < 0 || ds.Y[i] >= K {
				return nil, fmt.Errorf("row %d: label index out of range", i)
			}
		}
	}
	logf := func(f string, a ...any) {
		if cfg.Log != nil {
			fmt.Fprintf(cfg.Log, f+"\n", a...)
		}
	}

	// Estandarización con las cifras de train: media y desviación de los
	// vectores unitarios. Los codificadores de frases dejan todo en un cono
	// estrecho (cosenos de 0,7–0,9 entre frases sin relación); centrar y
	// escalar le da a la logística un problema bien condicionado.
	mean64 := make([]float64, D)
	unit := func(v []float32, dst []float64) {
		n := 0.0
		for _, a := range v {
			n += float64(float64(a) * float64(a))
		}
		n = math.Sqrt(n)
		if n == 0 {
			n = 1
		}
		for d, a := range v {
			dst[d] = float64(a) / n
		}
	}
	u := make([]float64, D)
	for _, v := range train.X {
		unit(v, u)
		for d := range u {
			mean64[d] += u[d]
		}
	}
	for d := range mean64 {
		mean64[d] /= float64(len(train.X))
	}
	var2 := make([]float64, D)
	for _, v := range train.X {
		unit(v, u)
		for d := range u {
			x := u[d] - mean64[d]
			var2[d] += float64(x * x)
		}
	}
	mean, inv := make([]float32, D), make([]float32, D)
	for d := range var2 {
		sd := math.Sqrt(var2[d] / float64(len(train.X)))
		mean[d] = float32(mean64[d])
		inv[d] = float32(1 / math.Max(sd, 1e-6))
	}
	prep := func(ds Dataset) [][]float64 {
		out := make([][]float64, len(ds.X))
		for i, v := range ds.X {
			out[i] = make([]float64, D)
			standardize(v, mean, inv, out[i])
		}
		return out
	}
	xt, xv := prep(train), prep(valid)

	// Pesos por clase: 1/√frecuencia, normalizados a media 1 por ejemplo.
	cw := make([]float64, K)
	for i := range cw {
		cw[i] = 1
	}
	if cfg.ClassWeight == "sqrt" {
		cnt := make([]float64, K)
		for _, y := range train.Y {
			cnt[y]++
		}
		s := 0.0
		for k := range cw {
			if cnt[k] > 0 {
				cw[k] = 1 / math.Sqrt(cnt[k])
			}
			s += float64(cw[k] * cnt[k])
		}
		for k := range cw {
			cw[k] *= float64(len(train.Y)) / s
		}
	} else if cfg.ClassWeight != "none" {
		return nil, fmt.Errorf("class weight must be sqrt or none, not %q", cfg.ClassWeight)
	}

	rng := &splitmix{s: cfg.Seed}
	p := newParams(D, cfg.Hidden, K, rng, cfg.LR, cfg.L2)
	order := make([]int, len(xt))
	for i := range order {
		order[i] = i
	}
	var best *params
	bestF1, bestEpoch, since := -1.0, 0, 0
	for ep := 1; ep <= cfg.Epochs; ep++ {
		rng.shuffle(order)
		loss := 0.0
		for s := 0; s < len(order); s += cfg.Batch {
			e := min(s+cfg.Batch, len(order))
			for _, i := range order[s:e] {
				p.forward(xt[i])
				loss += p.backward(xt[i], train.Y[i], cw[train.Y[i]])
			}
			p.step(e - s)
		}
		pred := make([]int, len(xv))
		for i, x := range xv {
			p.forward(x)
			pred[i] = argmax(p.z)
		}
		acc, f1 := accF1(pred, valid.Y, K)
		logf("epoch %2d  train loss %.4f  valid acc %.4f  macro-F1 %.4f", ep, loss/float64(len(xt)), acc, f1)
		if f1 > bestF1 {
			bestF1, bestEpoch, best, since = f1, ep, p.clone(), 0
		} else if since++; since >= cfg.Patience {
			break
		}
	}

	h := quantize(best, labels, mean, inv)
	h.Meta = cfg.Meta
	h.Meta.Epochs = bestEpoch

	// Calibración con la cabeza YA cuantizada, que es la que se sirve.
	logits := make([][]float64, len(valid.X))
	agree := 0
	for i, v := range valid.X {
		logits[i] = make([]float64, K)
		if err := h.Logits(v, logits[i]); err != nil {
			return nil, err
		}
		best.forward(xv[i])
		if argmax(logits[i]) == argmax(best.z) {
			agree++
		}
	}
	h.Temperature = jev.FitTemperature(logits, valid.Y)
	pred, prob := make([]int, 0, len(logits)), make([]float64, 0, len(logits))
	gold := make([]int, 0, len(logits))
	predAll := make([]int, len(logits))
	pp := make([]float64, K)
	for i, z := range logits {
		b := softmaxT(z, h.Temperature, pp)
		predAll[i] = b
		if cfg.ThresholdMask != nil && !cfg.ThresholdMask[i] {
			continue
		}
		pred, prob, gold = append(pred, b), append(prob, pp[b]), append(gold, valid.Y[i])
	}
	h.Thresholds = jev.ChooseThresholds(K, pred, gold, prob, jev.ThresholdParams{
		TargetPrecision: cfg.TargetPrecision, MinSupport: cfg.MinSupport})
	h.Meta.TargetPrecision = cfg.TargetPrecision
	acc, f1 := accF1(predAll, valid.Y, K)
	h.Meta.ValidAccuracy, h.Meta.ValidMacroF1 = round4(acc), round4(f1)
	if err := h.Validate(); err != nil {
		return nil, err
	}
	logf("best epoch %d; quantized valid acc %.4f macro-F1 %.4f; temperature %.3f", bestEpoch, acc, f1, h.Temperature)
	return &TrainResult{Head: h, BestEpoch: bestEpoch, ValidAccuracy: acc, ValidMacroF1: f1,
		Agreement: float64(agree) / float64(len(valid.X))}, nil
}

// quantize pasa los pesos a int16 con una escala por capa (el mayor valor
// absoluto ocupa 32767). Los sesgos se quedan en float64: son pocos.
func quantize(p *params, labels []string, mean, inv []float32) *Head {
	q := func(w []float64) ([]int16, float64) {
		m := 0.0
		for _, x := range w {
			m = math.Max(m, math.Abs(x))
		}
		if m == 0 {
			return make([]int16, len(w)), 0
		}
		s := m / 32767
		out := make([]int16, len(w))
		for i, x := range w {
			out[i] = int16(math.Round(x / s))
		}
		return out, s
	}
	h := &Head{Labels: labels, Dim: p.D, Hidden: p.H, Mean: mean, InvStd: inv, Temperature: 1,
		B2: append([]float64(nil), p.B2...)}
	if p.H > 0 {
		h.W1, h.S1 = q(p.W1)
		h.B1 = append([]float64(nil), p.B1...)
	}
	h.W2, h.S2 = q(p.W2)
	h.Thresholds = make([]float64, len(labels))
	return h
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

// accF1 son el acierto y el macro-F1 (clases presentes en el oro o en la
// predicción).
func accF1(pred, gold []int, K int) (float64, float64) {
	tp, fp, fn := make([]int, K), make([]int, K), make([]int, K)
	ok := 0
	for i := range pred {
		if pred[i] == gold[i] {
			ok++
			tp[gold[i]]++
		} else {
			fp[pred[i]]++
			fn[gold[i]]++
		}
	}
	sum, n := 0.0, 0
	for k := 0; k < K; k++ {
		if tp[k]+fp[k]+fn[k] == 0 {
			continue
		}
		n++
		if tp[k] > 0 {
			pr := float64(tp[k]) / float64(tp[k]+fp[k])
			rc := float64(tp[k]) / float64(tp[k]+fn[k])
			sum += 2 * pr * rc / (pr + rc)
		}
	}
	if len(pred) == 0 || n == 0 {
		return 0, 0
	}
	return float64(ok) / float64(len(pred)), sum / float64(n)
}

func round4(x float64) float64 { return math.Round(x*10000) / 10000 }
