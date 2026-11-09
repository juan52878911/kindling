package machine

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Close espera a los vigilantes: antes los lanzaba Watch sin WaitGroup y uno
// que acababa tarde (un freeze del TTL, la presión de memoria) escribía
// state.json después de que Close volviera, encima del estado del daemon
// siguiente.
func TestCloseEsperaALosVigilantes(t *testing.T) {
	m := newTestManager(t)
	var acabo atomic.Bool
	m.lanzarVigilante(context.Background(), func(ctx context.Context) {
		<-ctx.Done() // Close lo cancela
		time.Sleep(100 * time.Millisecond)
		acabo.Store(true)
	})
	m.Close()
	if !acabo.Load() {
		t.Fatal("Close volvió con un vigilante aún en marcha")
	}
}

// Una operación en segundo plano que llega tras Close no empieza: la
// escribiría su trasCierre después del cierre.
func TestRemoveSiTrasCloseNoHaceNada(t *testing.T) {
	m := newTestManager(t)
	id := newID()
	m.mu.Lock()
	m.byID[id] = &api.Machine{ID: id, Name: "r", State: api.StateStopped, CreatedAt: time.Now()}
	m.mu.Unlock()
	m.Close()
	err := m.removeSi(id, func(*api.Machine) bool { return true })
	if err == nil {
		t.Fatal("removeSi borró una máquina tras Close")
	}
	if _, ok := m.get(id); !ok {
		t.Fatal("la máquina ya no está")
	}
	if _, err := m.freezeSi(context.Background(), id, func(*api.Machine) bool { return true }); err == nil {
		t.Fatal("freezeSi siguió tras Close")
	}
}

// La última escritura del estado, la del cierre, reintenta un rato: antes era
// un solo intento y un fallo pasajero perdía la última transición.
func TestUltimaEscrituraDelCierreReintenta(t *testing.T) {
	m := newTestManager(t)
	// Falla todo hasta el cierre (la foto sigue pendiente al llegar quit) y
	// las dos primeras del cierre: solo llega a disco si el cierre reintenta.
	var fallos atomic.Int32
	prev := escribirEstado
	escribirEstado = func(ruta string, datos []byte, perm os.FileMode) error {
		if !m.cerrado() || fallos.Add(1) <= 2 {
			return errors.New("disco lleno")
		}
		return prev(ruta, datos, perm)
	}
	t.Cleanup(func() { escribirEstado = prev })
	m.mu.Lock()
	id := newID()
	m.byID[id] = &api.Machine{ID: id, Name: "e", State: api.StateStopped, CreatedAt: time.Now()}
	gen := m.persist()
	m.mu.Unlock()
	m.Close()
	if got := escritoGen(m); got < gen {
		t.Fatalf("tras Close se escribió la foto %d, quería %d", got, gen)
	}
}
