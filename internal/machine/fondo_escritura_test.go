package machine

import (
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/durable"
	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/pkg/api"
)

// SetFondo escribe fondos.json (dos fsync) sin fondoMu: con él tomado, un
// disco lento paraba cada reclamación y cada relleno de todos los fondos.
func TestSetFondoEscribeSinElCandadoDelFondo(t *testing.T) {
	m := newTestManager(t)
	m.bus = events.New()
	escribirSnapshot(t, m, "dorado", api.Snapshot{})
	var n atomic.Int32
	m.altaFondo = altaFalsa(m, &n)
	t.Cleanup(func() { _, _ = m.SetFondo("dorado", 0); m.pararFondos() })

	dentro, seguir := make(chan struct{}), make(chan struct{})
	prev := escribirFondosEn
	escribirFondosEn = func(ruta string, datos []byte, perm os.FileMode) error {
		close(dentro)
		<-seguir
		return durable.Escribir(ruta, datos, perm)
	}
	t.Cleanup(func() { escribirFondosEn = prev })

	hecho := make(chan error, 1)
	go func() { _, err := m.SetFondo("dorado", 1); hecho <- err }()
	select {
	case <-dentro:
	case <-time.After(5 * time.Second):
		t.Fatal("SetFondo no llegó a escribir")
	}
	libre := m.fondoMu.TryLock()
	if libre {
		m.fondoMu.Unlock()
	}
	close(seguir)
	if err := <-hecho; err != nil {
		t.Fatal(err)
	}
	if !libre {
		t.Fatal("fondoMu seguía tomado mientras se escribía fondos.json")
	}
}
