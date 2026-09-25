package codificador

import (
	"errors"
	"fmt"
	"math"

	"github.com/juan52878911/kindling/pkg/chispa"
)

// Head es la cabeza de clasificación sobre los embeddings congelados:
//
//	x = (v/‖v‖ − media) · invStd        (estandarizado con las cifras de train)
//	h = relu(S1 · W1·x + B1)            (solo si Hidden > 0)
//	z = S2 · W2·(h o x) + B2
//	p = softmax(z / Temperature)
//
// Los pesos son int16 con una escala por capa (como Chispa): 384×28 pesos son
// 21 KB y el producto es exacto en cualquier máquina. Las sumas van en orden
// fijo y cada producto se redondea antes de sumarlo (float64(a*b)): Go puede
// fundir a*b+c en una FMA en arm64 y no en amd64, y entonces la misma frase
// daría otra probabilidad en el último bit según el host.
type Head struct {
	Labels []string
	Meta   Meta
	Dim    int // dimensión del codificador
	Hidden int // 0 = regresión logística

	Mean, InvStd []float32

	S1 float64
	W1 []int16 // Hidden × Dim
	B1 []float64

	S2 float64
	W2 []int16 // K × (Hidden o Dim)
	B2 []float64

	Temperature float64
	Thresholds  []float64 // τ por etiqueta (chispa.NeverConfident = escala siempre)
}

// Meta es lo que acompaña a la cabeza: con qué codificador se entrenó (otro
// daría vectores en otro espacio), el prefijo que ese codificador espera, y
// cómo se entrenó y midió.
type Meta struct {
	Encoder       string `json:"encoder"`                  // multilingual-e5-small:q8_0
	EncoderSHA256 string `json:"encoder_sha256,omitempty"` // del GGUF
	// Prefix se antepone a cada texto antes de pedir su vector («query: » en
	// e5, que se entrenó así; vacío en MiniLM).
	Prefix          string  `json:"prefix,omitempty"`
	TargetPrecision float64 `json:"target_precision,omitempty"`
	CreatedAt       string  `json:"created_at,omitempty"`
	DatasetSHA256   string  `json:"dataset_sha256,omitempty"`
	Epochs          int     `json:"epochs,omitempty"`
	ValidAccuracy   float64 `json:"valid_accuracy,omitempty"`
	ValidMacroF1    float64 `json:"valid_macro_f1,omitempty"`
	Notes           string  `json:"notes,omitempty"`
}

// Prediction es lo que dice la cabeza de un vector.
type Prediction struct {
	Label     string
	Index     int
	Prob      float64 // calibrada
	Threshold float64
	Confident bool
	// Probs es la distribución completa (en el orden de Labels).
	Probs []float64
}

// Validate comprueba que las piezas cuadran: la usan Unmarshal y el
// entrenador antes de devolver nada.
func (h *Head) Validate() error {
	K, D, H := len(h.Labels), h.Dim, h.Hidden
	switch {
	case K < 2 || K > chispa.MaxLabels:
		return fmt.Errorf("need between 2 and %d labels, got %d", chispa.MaxLabels, K)
	case D < 1 || D > MaxDim:
		return fmt.Errorf("bad dimension %d", D)
	case H < 0 || H > maxHidden:
		return fmt.Errorf("bad hidden size %d", H)
	case len(h.Mean) != D || len(h.InvStd) != D:
		return errors.New("standardization vectors do not match the dimension")
	case len(h.Thresholds) != K || len(h.B2) != K:
		return errors.New("per-label vectors do not match the labels")
	case !(h.Temperature > 0) || math.IsInf(h.Temperature, 0):
		return errors.New("bad temperature")
	case !finite(h.S2) || h.S2 < 0:
		return errors.New("bad output scale")
	}
	in := D
	if H > 0 {
		in = H
		if len(h.W1) != H*D || len(h.B1) != H || !finite(h.S1) || h.S1 < 0 {
			return errors.New("hidden layer does not match its sizes")
		}
	} else if len(h.W1) != 0 || len(h.B1) != 0 {
		return errors.New("hidden weights without a hidden layer")
	}
	if len(h.W2) != K*in {
		return errors.New("output weights do not match their sizes")
	}
	for _, v := range [][]float64{h.B1, h.B2, h.Thresholds} {
		for _, x := range v {
			if !finite(x) {
				return errors.New("non-finite parameter")
			}
		}
	}
	for _, v := range [][]float32{h.Mean, h.InvStd} {
		for _, x := range v {
			if !finite(float64(x)) {
				return errors.New("non-finite standardization")
			}
		}
	}
	seen := map[string]bool{}
	for _, l := range h.Labels {
		if l == "" || len(l) > chispa.MaxLabelBytes || seen[l] {
			return fmt.Errorf("invalid or repeated label %q", l)
		}
		seen[l] = true
	}
	return nil
}

func finite(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }

// Logits escribe en z (len(Labels)) las logits SIN calibrar de v. Es lo que
// usa la calibración; Predict divide por la temperatura.
func (h *Head) Logits(v []float32, z []float64) error {
	if len(v) != h.Dim {
		return fmt.Errorf("vector of %d dimensions for a head of %d", len(v), h.Dim)
	}
	x := make([]float64, h.Dim)
	standardize(v, h.Mean, h.InvStd, x)
	in := x
	if h.Hidden > 0 {
		hid := make([]float64, h.Hidden)
		for j := range hid {
			row := h.W1[j*h.Dim : (j+1)*h.Dim]
			s := 0.0
			for d, w := range row {
				s += float64(float64(w) * x[d])
			}
			a := float64(h.S1*s) + h.B1[j]
			if a < 0 {
				a = 0
			}
			hid[j] = a
		}
		in = hid
	}
	n := len(in)
	for k := range z {
		row := h.W2[k*n : (k+1)*n]
		s := 0.0
		for d, w := range row {
			s += float64(float64(w) * in[d])
		}
		z[k] = float64(h.S2*s) + h.B2[k]
	}
	return nil
}

// Predict clasifica un vector del codificador.
func (h *Head) Predict(v []float32) (Prediction, error) {
	K := len(h.Labels)
	z := make([]float64, K)
	if err := h.Logits(v, z); err != nil {
		return Prediction{}, err
	}
	p := make([]float64, K)
	best := softmaxT(z, h.Temperature, p)
	pr := Prediction{Label: h.Labels[best], Index: best, Prob: p[best], Threshold: h.Thresholds[best], Probs: p}
	pr.Confident = pr.Prob >= pr.Threshold
	return pr, nil
}

// standardize: x = (v/‖v‖ − media)·invStd. Normalizar primero hace que la
// cabeza no dependa de si el servidor ya devuelve vectores unitarios.
func standardize(v, mean, inv []float32, x []float64) {
	n := 0.0
	for _, a := range v {
		n += float64(float64(a) * float64(a))
	}
	n = math.Sqrt(n)
	if n == 0 {
		n = 1
	}
	for d, a := range v {
		x[d] = float64(float64(float64(a)/n)-float64(mean[d])) * float64(inv[d])
	}
}

// softmaxT es la softmax de z/T con la exponencial reproducible de Chispa. En
// empate gana el índice menor.
func softmaxT(z []float64, T float64, p []float64) int {
	best := 0
	for k := 1; k < len(z); k++ {
		if z[k] > z[best] {
			best = k
		}
	}
	m := z[best]
	sum := 0.0
	for k, v := range z {
		p[k] = chispa.DetExp(float64(v-m) / T)
		sum += p[k]
	}
	for k := range p {
		p[k] /= sum
	}
	return best
}
