package codificador

import (
	"math"
	"sort"
)

// KNN es la comparación de referencia: los K ejemplos etiquetados más
// cercanos por coseno votan, cada uno con su similitud. Solo para evaluar:
// guardar decenas de miles de vectores por modelo no es lo que se sirve, y la
// cabeza lineal ya condensa lo mismo en 21 KB.
type KNN struct {
	K      int
	Labels []string
	vecs   [][]float32 // unitarios
	y      []int
}

// NewKNN guarda los vectores de train normalizados.
func NewKNN(k int, labels []string, ds Dataset) *KNN {
	n := &KNN{K: k, Labels: labels, y: ds.Y}
	for _, v := range ds.X {
		n.vecs = append(n.vecs, unitVec(v))
	}
	return n
}

func unitVec(v []float32) []float32 {
	s := 0.0
	for _, a := range v {
		s += float64(a) * float64(a)
	}
	s = math.Sqrt(s)
	if s == 0 {
		s = 1
	}
	out := make([]float32, len(v))
	for i, a := range v {
		out[i] = float32(float64(a) / s)
	}
	return out
}

// Predict devuelve la etiqueta ganadora y su parte del voto (0..1).
func (n *KNN) Predict(v []float32) (int, float64) {
	q := unitVec(v)
	type nb struct {
		sim float64
		i   int
	}
	best := make([]nb, 0, n.K+1)
	for i, u := range n.vecs {
		s := 0.0
		for d, a := range u {
			s += float64(a) * float64(q[d])
		}
		if len(best) < n.K || s > best[len(best)-1].sim {
			best = append(best, nb{s, i})
			sort.Slice(best, func(a, b int) bool { return best[a].sim > best[b].sim })
			if len(best) > n.K {
				best = best[:n.K]
			}
		}
	}
	votes := make([]float64, len(n.Labels))
	tot := 0.0
	for _, b := range best {
		w := math.Max(b.sim, 0)
		votes[n.y[b.i]] += w
		tot += w
	}
	k := argmax(votes)
	if tot == 0 {
		return k, 0
	}
	return k, votes[k] / tot
}
