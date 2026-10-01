package machine

import (
	"context"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Lo que no cabe por reservas EN VUELO (otros arranques que aún no terminan)
// espera a que se liberen en vez de rechazar: 64 `kling db up` a la vez
// dejaban a los últimos en "doesn't fit" con el host casi vacío.
func TestReservaEsperaALasEnVuelo(t *testing.T) {
	avail := availableMiB()
	if avail <= 0 {
		t.Skip("sin /proc/meminfo")
	}
	t.Setenv("KLING_MIN_FREE_MIB", "0")
	m := newTestManager(t)
	// Lo pendiente llena el host; lo pedido cabe solo.
	want := 64
	m.mu.Lock()
	m.pendingMiB = avail
	m.mu.Unlock()
	go func() {
		time.Sleep(150 * time.Millisecond)
		m.mu.Lock()
		m.pendingMiB = 0
		m.mu.Unlock()
	}()
	t0 := time.Now()
	release, err := m.reserveMemoryMakingRoom(context.Background(), want, "", "")
	if err != nil {
		t.Fatalf("debía esperar a las reservas en vuelo: %v", err)
	}
	release()
	if d := time.Since(t0); d < 100*time.Millisecond {
		t.Errorf("no esperó (%v)", d)
	}

	// Sin reservas en vuelo, lo que no cabe se rechaza al momento.
	old := esperaReservasMax
	esperaReservasMax = 5 * time.Second
	t.Cleanup(func() { esperaReservasMax = old })
	t0 = time.Now()
	_, err = m.reserveMemoryMakingRoom(context.Background(), avail*4, "", "")
	if !api.IsInsufficientMemory(err) || time.Since(t0) > time.Second {
		t.Fatalf("falta de memoria de verdad: %v en %v", err, time.Since(t0))
	}
}
