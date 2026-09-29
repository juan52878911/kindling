package main

import (
	"math"
	"sort"
)

// Dist resume una serie de latencias en milisegundos.
//
// Percentiles por rango más cercano sobre la serie ORDENADA y completa, sin
// histogramas ni interpolación (igual que kling-mcpbench): cada percentil es
// una latencia que ocurrió de verdad.
type Dist struct {
	N    int     `json:"n"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// percentile devuelve el percentil p (0-100) de sorted, que DEBE venir
// ordenada: el ceil(p/100 × n)-ésimo contando desde 1. Serie vacía: 0.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	return sorted[rank-1]
}

// summarize ordena una COPIA de xs y calcula su resumen.
func summarize(xs []float64) Dist {
	if len(xs) == 0 {
		return Dist{}
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	var sum float64
	for _, x := range s {
		sum += x
	}
	return Dist{
		N:    len(s),
		P50:  round2(percentile(s, 50)),
		P95:  round2(percentile(s, 95)),
		P99:  round2(percentile(s, 99)),
		Max:  round2(s[len(s)-1]),
		Mean: round2(sum / float64(len(s))),
	}
}

func round2(x float64) float64 { return math.Round(x*100) / 100 }

// degradedPct: por encima de este porcentaje de copias fallidas una ronda (o
// una celda) es DEGRADED y sus latencias no se citan como las de la celda.
const degradedPct = 1.0

// failStatus decide el estado a partir de fallos y total.
func failStatus(fails, total int) string {
	if total > 0 && float64(fails)*100/float64(total) > degradedPct {
		return "DEGRADED"
	}
	return "ok"
}
