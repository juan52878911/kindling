package inquilinos

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// BenchmarkResolverConVolcado mide Resolver desde muchas goroutines mientras
// el "último uso" se vuelca a disco a menudo (D-06): el volcado no debería
// parar a las demás autenticaciones. Da el p50, el p99 y el máximo.
func BenchmarkResolverConVolcado(b *testing.B) {
	a, err := Abrir(b.TempDir(), Opciones{CadaUso: time.Millisecond})
	if err != nil {
		b.Fatal(err)
	}
	defer a.Cerrar()
	var k string
	for i := 0; i < 50; i++ {
		n := "t" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, err := a.Crear(n, false, Topes{}); err != nil {
			b.Fatal(err)
		}
		if _, k, err = a.NuevaClave(n, nil); err != nil {
			b.Fatal(err)
		}
	}
	var mu sync.Mutex
	var todas []time.Duration
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		var ds []time.Duration
		for pb.Next() {
			ini := time.Now()
			if _, ok := a.Resolver(k); !ok {
				b.Error("clave rechazada")
				return
			}
			ds = append(ds, time.Since(ini))
		}
		mu.Lock()
		todas = append(todas, ds...)
		mu.Unlock()
	})
	b.StopTimer()
	sort.Slice(todas, func(i, j int) bool { return todas[i] < todas[j] })
	if len(todas) == 0 {
		return
	}
	b.ReportMetric(float64(todas[len(todas)/2].Nanoseconds()), "p50-ns")
	b.ReportMetric(float64(todas[len(todas)*99/100].Nanoseconds()), "p99-ns")
	b.ReportMetric(float64(todas[len(todas)-1].Microseconds()), "max-µs")
}
