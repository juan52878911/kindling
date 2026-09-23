package jev

import (
	"math"
	"sort"
)

// Calibración: dos piezas independientes.
//
//  1. Temperatura (Guo et al., 2017): un solo escalar T que divide las logits y
//     minimiza la log-verosimilitud negativa en validación. No cambia el
//     argmax, así que no toca la exactitud; solo hace que «0,9» signifique
//     acertar ~9 de cada 10 veces. Se ajusta sobre el modelo YA cuantizado,
//     que es el que se sirve.
//  2. Umbral por clase τ_c: el menor corte cuya precisión en validación, entre
//     las predicciones de esa clase con p >= τ_c, llega al objetivo. Es lo que
//     decide confident/escalate, y por clase porque cada una tiene su dificultad:
//     un único umbral global o dejaría escapar la clase difícil o haría escalar
//     de más a la fácil.

// FitTemperature busca T en [0.05, 20] (búsqueda de sección áurea sobre log T,
// la NLL es unimodal en T) para las logits sin calibrar de validación y sus
// etiquetas correctas. gold[i] < 0 (etiqueta desconocida) se ignora.
func FitTemperature(logits [][]float64, gold []int) float64 {
	nll := func(logT float64) float64 {
		T := detExp(logT)
		sum, n := 0.0, 0
		for i, z := range logits {
			if gold[i] < 0 {
				continue
			}
			m := z[0]
			for _, v := range z[1:] {
				m = math.Max(m, v)
			}
			s := 0.0
			for _, v := range z {
				s += detExp(float64(v-m) / T)
			}
			sum += detLog(s) - float64(z[gold[i]]-m)/T
			n++
		}
		if n == 0 {
			return 0
		}
		return sum / float64(n)
	}
	const phi = 0.6180339887498949
	a, b := detLog(0.05), detLog(20)
	c := b - float64(phi*(b-a))
	d := a + float64(phi*(b-a))
	fc, fd := nll(c), nll(d)
	for i := 0; i < 60; i++ {
		if fc <= fd {
			b, d, fd = d, c, fc
			c = b - float64(phi*(b-a))
			fc = nll(c)
		} else {
			a, c, fc = c, d, fd
			d = a + float64(phi*(b-a))
			fd = nll(d)
		}
	}
	return detExp((a + b) / 2)
}

// ThresholdParams controla la elección de τ.
type ThresholdParams struct {
	// TargetPrecision: precisión mínima del subconjunto confiado de cada clase.
	TargetPrecision float64
	// MinSupport: predicciones confiadas de validación necesarias para fiarse
	// del corte. Con menos, la clase escala siempre.
	MinSupport int
}

// ChooseThresholds elige τ por clase. prob[i] es la probabilidad calibrada de
// la predicción pred[i], gold[i] la correcta. La precisión se estima de forma
// conservadora como aciertos/(n+1): con 19 de 19 no se promete el 100 %, y
// con pocos datos el corte sube en vez de fiarse de la suerte.
func ChooseThresholds(nLabels int, pred, gold []int, prob []float64, p ThresholdParams) []float64 {
	if p.MinSupport < 1 {
		p.MinSupport = 1
	}
	out := make([]float64, nLabels)
	for c := 0; c < nLabels; c++ {
		type row struct {
			p  float64
			ok bool
		}
		var rows []row
		for i := range pred {
			if pred[i] == c {
				rows = append(rows, row{prob[i], gold[i] == c})
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].p > rows[j].p })
		out[c] = NeverConfident
		tp := 0
		for i, r := range rows {
			if r.ok {
				tp++
			}
			n := i + 1
			if n < len(rows) && rows[n].p == r.p {
				continue // los empates entran o salen juntos
			}
			if n >= p.MinSupport && float64(tp)/float64(n+1) >= p.TargetPrecision {
				out[c] = r.p
			}
		}
	}
	return out
}
