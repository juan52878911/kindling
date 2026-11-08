package machine

import (
	"context"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// El tope de máquinas cuenta también las que están naciendo: antes se miraba
// solo len(byID) y N peticiones a la vez pasaban todas con un hueco libre.
func TestTopeDeMaquinasCuentaLasQueNacen(t *testing.T) {
	m := newTestManager(t)
	t.Setenv("KLING_MAX_MACHINES", "3")
	m.addForTest("ac00000000000001")

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		huecos   []*hueco
		rechazos int
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := m.reservarHueco()
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !api.IsMachineLimit(err) {
					t.Errorf("error inesperado: %v", err)
				}
				rechazos++
				return
			}
			huecos = append(huecos, h)
		}()
	}
	wg.Wait()
	if len(huecos) != 2 || rechazos != 6 {
		t.Fatalf("pasaron %d y se rechazaron %d; con 1 de 3 ocupada quería 2 y 6", len(huecos), rechazos)
	}

	// Publicar gasta el hueco: la máquina pasa a contar en byID, no dos veces.
	mc := &api.Machine{ID: "ac00000000000002", Name: "ac00000000000002", State: api.StateCreated}
	if err := m.publicarCon(conHueco(context.Background(), huecos[0]), mc); err != nil {
		t.Fatal(err)
	}
	m.soltarHueco(huecos[0]) // ya gastado: no descuenta otra vez
	m.mu.RLock()
	n, naciendo := len(m.byID), m.naciendo
	m.mu.RUnlock()
	if n != 2 || naciendo != 1 {
		t.Fatalf("byID=%d naciendo=%d; quería 2 y 1", n, naciendo)
	}
	if _, err := m.reservarHueco(); !api.IsMachineLimit(err) {
		t.Fatalf("con 2 en byID y 1 naciendo cabía otra (%v)", err)
	}
	// La que no llega a publicarse devuelve su hueco.
	m.soltarHueco(huecos[1])
	if h, err := m.reservarHueco(); err != nil {
		t.Fatalf("el hueco devuelto no se puede volver a usar: %v", err)
	} else {
		m.soltarHueco(h)
	}
}
