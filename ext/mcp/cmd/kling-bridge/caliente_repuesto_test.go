package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/guest"
)

// LO QUE PAGAN LAS SESIONES QUE LLEGAN DESPUÉS DE LA PRIMERA.
//
// Tras restaurar el dorado hay UN hijo caliente: la primera sesión lo adopta y
// no paga el arranque del runtime. Las siguientes de la misma instancia sí.

const arranqueFalsoMs = 300

// sinMMDS hace que el almacén MMDS conteste vacío, sin salir a la red.
func sinMMDS(tb testing.TB) {
	antes := fetchMMDS
	fetchMMDS = func() (*guest.MMDSStore, error) { return nil, nil }
	tb.Cleanup(func() { fetchMMDS = antes })
}

// initializeSesion abre una sesión y mide hasta la respuesta del initialize.
func initializeSesion(tb testing.TB, b *bridge) (time.Duration, *session) {
	tb.Helper()
	t0 := time.Now()
	s, _, err := b.resolve("", true)
	if err != nil {
		tb.Fatalf("resolve: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.request(ctx, `"i"`, []byte(`{"jsonrpc":"2.0","id":"i","method":"initialize","params":{}}`)); err != nil {
		tb.Fatalf("initialize: %v", err)
	}
	b.release(s)
	return time.Since(t0), s
}

// BenchmarkSesionesTrasRestaurar: un puente con su caliente (como tras
// restaurar) recibe tres sesiones separadas por más de lo que tarda el
// runtime en arrancar; ms_sesionN es lo que tardó el initialize de la N.
func BenchmarkSesionesTrasRestaurar(b *testing.B) {
	b.Setenv(envArranqueFalso, fmt.Sprint(arranqueFalsoMs))
	sinMMDS(b)
	var suma [3]time.Duration
	for range b.N {
		b.StopTimer()
		br := testBridge()
		if err := br.replenishWarm(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		for i := range 3 {
			d, _ := initializeSesion(b, br)
			suma[i] += d
			b.StopTimer()
			time.Sleep(2 * arranqueFalsoMs * time.Millisecond)
			b.StartTimer()
		}
		b.StopTimer()
		br.closeAll()
		b.StartTimer()
	}
	for i, d := range suma {
		b.ReportMetric(float64(d.Milliseconds())/float64(b.N), fmt.Sprintf("ms_sesion%d", i+1))
	}
}
