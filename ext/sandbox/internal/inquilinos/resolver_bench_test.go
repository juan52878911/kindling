package inquilinos

import (
	"sort"
	"sync"
	"testing"
	"time"
)

// D-06: el volcado del último uso se escribe sin el cerrojo, así que puede
// llegar a disco después de un cambio posterior. Ese volcado atrasado no puede
// pisar el cambio: una clave revocada no vuelve a valer al reabrir.
func TestVolcadoAtrasadoNoPisaUnCambio(t *testing.T) {
	dir, rl := t.TempDir(), &reloj{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	a := abrir(t, dir, rl)
	if _, err := a.Crear("ana", false, Topes{}); err != nil {
		t.Fatal(err)
	}
	c, k, _ := a.NuevaClave("ana", nil)
	rl.avanza(time.Second)
	a.Resolver(k)
	// La copia que un Resolver habría tomado para volcar, antes de revocar.
	a.mu.Lock()
	atrasada, g := a.est.clonar(), a.gen
	a.mu.Unlock()
	if err := a.Revocar("ana", c.ID); err != nil {
		t.Fatal(err)
	}
	if err := a.escribir(atrasada, g); err != nil {
		t.Fatal(err)
	}
	a.Cerrar()
	b := abrir(t, dir, rl)
	if _, ok := b.Resolver(k); ok {
		t.Fatal("la clave revocada vuelve a valer: el volcado atrasado pisó la revocación")
	}
}

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
