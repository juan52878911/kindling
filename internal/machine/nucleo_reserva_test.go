package machine

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

func poblarR6(tb testing.TB, n int) *Manager {
	tb.Setenv("KLING_MEM_OVERCOMMIT", "0")
	m := newTestManager(tb)
	ahora := time.Now()
	m.mu.Lock()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("%016x", 0xabd0000000000000+uint64(i))
		m.byID[id] = &api.Machine{ID: id, Name: fmt.Sprintf("s-%d", i), From: "dorado", State: api.StateRunning,
			VCPUs: 1, MemMiB: 256, PID: 1000 + i, CreatedAt: ahora}
	}
	m.mu.Unlock()
	return m
}

func BenchmarkR6ReservaMemoria(b *testing.B) {
	for _, n := range []int{100, 500, 1000, 2000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			m := poblarR6(b, n)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rel, err := m.reserveMemory(256, "dorado")
				if err != nil {
					b.Fatal(err)
				}
				rel()
			}
		})
	}
}

// Una reserva con 500 vivas no debe costar mucho más que con 5: el coste no
// crece con las máquinas vivas bajo el cerrojo global.
func TestR6ReservaNoHaceUnStatPorMaquina(t *testing.T) {
	m := poblarR6(t, 500)
	var n atomic.Int64
	viejo := statMemFile
	statMemFile = func(p string) (os.FileInfo, error) { n.Add(1); return viejo(p) }
	t.Cleanup(func() { statMemFile = viejo })
	// El mejor de tres rondas, en los dos lados: una ronda sola, con -race y
	// el resto de paquetes corriendo a la vez, pasaba del margen por ruido.
	const vueltas = 200
	mejor := func(m *Manager) time.Duration {
		var menor time.Duration
		for r := 0; r < 3; r++ {
			t0 := time.Now()
			for i := 0; i < vueltas; i++ {
				rel, err := m.reserveMemory(256, "dorado")
				if err != nil {
					t.Fatal(err)
				}
				rel()
			}
			if d := time.Since(t0) / vueltas; r == 0 || d < menor {
				menor = d
			}
		}
		return menor
	}
	porAlta := mejor(m)
	// Lo que importa: tras la primera reserva, ninguna vuelve a mirar el disco
	// por cada máquina viva.
	n.Store(0)
	rel, err := m.reserveMemory(256, "dorado")
	if err != nil {
		t.Fatal(err)
	}
	rel()
	if got := n.Load(); got > 1 { // 1: el dorado, que aquí no existe y no se cachea
		t.Fatalf("una reserva con 500 vivas hizo %d stats (uno por máquina)", got)
	}
	t.Logf("500 vivas: %s por reserva (stats contados: %d)", porAlta, n.Load())
	base := mejor(poblarR6(t, 5))
	if porAlta > base+600*time.Microsecond {
		t.Fatalf("la reserva cuesta %s con 500 máquinas vivas y %s con 5: crece con N bajo el cerrojo global", porAlta, base)
	}
}
