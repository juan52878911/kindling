package machine

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// La puerta deja pasar como mucho a `cap` a la vez; el que sobra espera a que
// alguien suelte su hueco.
func TestLaunchGateAcotaConcurrencia(t *testing.T) {
	m := &Manager{launchGate: make(chan struct{}, 2)}
	ctx := context.Background()

	r1, err := m.enterLaunch(ctx)
	if err != nil {
		t.Fatalf("primer enterLaunch: %v", err)
	}
	r2, err := m.enterLaunch(ctx)
	if err != nil {
		t.Fatalf("segundo enterLaunch: %v", err)
	}

	// El tercero no debe pasar mientras haya dos dentro.
	var entro atomic.Bool
	done := make(chan struct{})
	go func() {
		r3, err := m.enterLaunch(ctx)
		if err == nil {
			entro.Store(true)
			r3()
		}
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	if entro.Load() {
		t.Fatal("el tercero entró con la puerta llena")
	}

	// Al soltar uno, el tercero pasa.
	r1()
	select {
	case <-done:
		if !entro.Load() {
			t.Fatal("el tercero terminó sin entrar")
		}
	case <-time.After(time.Second):
		t.Fatal("el tercero no pasó tras liberar un hueco")
	}
	r2()
}

// Un contexto cancelado mientras se espera turno devuelve error y no deja el
// hueco tomado.
func TestLaunchGateRespetaContexto(t *testing.T) {
	m := &Manager{launchGate: make(chan struct{}, 1)}
	r1, err := m.enterLaunch(context.Background())
	if err != nil {
		t.Fatalf("enterLaunch: %v", err)
	}
	defer r1()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()

	if _, err := m.enterLaunch(ctx); err == nil {
		t.Fatal("se esperaba error de contexto con la puerta llena")
	}
}

// Soltar dos veces el mismo hueco no libera de más: el sync.Once lo impide.
func TestLaunchGateReleaseIdempotente(t *testing.T) {
	m := &Manager{launchGate: make(chan struct{}, 1)}
	r, _ := m.enterLaunch(context.Background())
	r()
	r() // segundo release: no debe robar el hueco del siguiente

	// Con el hueco de vuelta, otro entra; y el doble release no lo dejó en -1.
	if _, err := m.enterLaunch(context.Background()); err != nil {
		t.Fatalf("tras doble release la puerta quedó rota: %v", err)
	}
}

// Un Manager sin puerta (construido a mano en un test) no bloquea nunca.
func TestLaunchGateNilSinLimite(t *testing.T) {
	m := &Manager{}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.enterLaunch(context.Background())
			if err != nil {
				t.Errorf("puerta nil devolvió error: %v", err)
			}
			r()
		}()
	}
	wg.Wait()
}
