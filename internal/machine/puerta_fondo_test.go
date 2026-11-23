package machine

import (
	"context"
	"sync"
	"testing"
	"time"
)

// medirPuertaFondo cuenta cuántas altas del fondo entran por la puerta de
// arranque antes de que haya entrado la última petición real (G-15). Con la
// puerta llena, llegan primero fondo altas del fondo y después reales
// peticiones; al soltarse el hueco, cada una entra, lo retiene un momento y
// lo suelta.
func medirPuertaFondo(tb testing.TB, fondo, reales int) int {
	tb.Helper()
	m := &Manager{launchGate: make(chan struct{}, 1)}
	ocupado, err := m.enterLaunch(context.Background())
	if err != nil {
		tb.Fatal(err)
	}
	var mu sync.Mutex
	var orden []bool // true = fondo
	var wg sync.WaitGroup
	lanzar := func(esFondo bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			if esFondo {
				ctx = context.WithValue(ctx, ctxFondo{}, true)
			}
			soltar, err := m.enterLaunch(ctx)
			if err != nil {
				tb.Error(err)
				return
			}
			mu.Lock()
			orden = append(orden, esFondo)
			mu.Unlock()
			time.Sleep(time.Millisecond)
			soltar()
		}()
	}
	for range fondo {
		lanzar(true)
	}
	time.Sleep(20 * time.Millisecond) // las del fondo, ya en la cola
	for range reales {
		lanzar(false)
	}
	time.Sleep(20 * time.Millisecond) // y las reales detrás
	ocupado()
	wg.Wait()

	ultimaReal := -1
	for i, f := range orden {
		if !f {
			ultimaReal = i
		}
	}
	n := 0
	for i := 0; i < ultimaReal; i++ {
		if orden[i] {
			n++
		}
	}
	return n
}

// BenchmarkPuertaFondo da cuántos huecos de la puerta se llevan las altas del
// fondo mientras hay peticiones reales esperando (métrica fondo_antes/op).
func BenchmarkPuertaFondo(b *testing.B) {
	total := 0
	for b.Loop() {
		total += medirPuertaFondo(b, 2, 8)
	}
	b.ReportMetric(float64(total)/float64(b.N), "fondo_antes/op")
}
