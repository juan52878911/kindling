package main

import "testing"

// Rango más cercano: el percentil es siempre una muestra que ocurrió.
func TestPercentileRangoMasCercano(t *testing.T) {
	xs := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	casos := []struct {
		p, want float64
	}{
		{0, 1}, {10, 1}, {11, 2}, {50, 5}, {51, 6}, {90, 9}, {95, 10}, {99, 10}, {100, 10},
	}
	for _, c := range casos {
		if got := percentile(xs, c.p); got != c.want {
			t.Errorf("p%.0f = %v, quería %v", c.p, got, c.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("serie vacía: %v, quería 0", got)
	}
	if got := percentile([]float64{42}, 99); got != 42 {
		t.Errorf("una muestra: %v, quería 42", got)
	}
}

// Con 100 muestras 1..100 el p99 es 99 y el máximo 100: un p99 que diera el
// máximo escondería la cola que se quiere ver.
func TestSummarizeCien(t *testing.T) {
	xs := make([]float64, 100)
	for i := range xs {
		xs[len(xs)-1-i] = float64(i + 1) // desordenada a propósito
	}
	d := summarize(xs)
	if d.N != 100 || d.P50 != 50 || d.P95 != 95 || d.P99 != 99 || d.Max != 100 || d.Mean != 50.5 {
		t.Fatalf("summarize = %+v", d)
	}
	if xs[0] != 100 {
		t.Error("summarize ordenó la serie del llamador; debe trabajar sobre una copia")
	}
}

func TestSummarizeVacia(t *testing.T) {
	if d := summarize(nil); d != (Dist{}) {
		t.Fatalf("summarize(nil) = %+v, quería ceros", d)
	}
}
