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

func TestDecidirLaunch(t *testing.T) {
	if lanzamientoPlataforma() > 0 {
		t.Skip("la plataforma fija su propio tope")
	}
	ant, cpu := hostAnidado, numCPU
	defer func() { hostAnidado, numCPU = ant, cpu }()
	for _, c := range []struct {
		nombre  string
		anidado bool
		cpus    int
		env     string
		quiero  int
	}{
		{"anidado 4", true, 4, "", 2},
		{"anidado 7", true, 7, "", 2},
		{"anidado 8", true, 8, "", 4},
		{"anidado 16", true, 16, "", 8},
		{"anidado 64", true, 64, "", 8},
		{"hierro 4", false, 4, "", 2},
		{"hierro 64", false, 64, "", 8},
		{"env manda", true, 64, "3", 3},
		{"env inválida", true, 4, "x", 2},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			hostAnidado = func() bool { return c.anidado }
			numCPU = func() int { return c.cpus }
			t.Setenv("KLING_MAX_PARALLEL_BOOT", c.env)
			if n, _ := decidirLaunch(); n != c.quiero {
				t.Fatalf("got %d, quiero %d", n, c.quiero)
			}
		})
	}
}

// El retroceso baja el tope a la mitad, no pasa de 2 y se recupera de uno en
// uno con el reloj falso.
func TestRetrocesoPuerta(t *testing.T) {
	ahora := time.Unix(1000, 0)
	r := &retrocesoPuerta{base: 8, reloj: func() time.Time { return ahora }}
	if r.tope() != 8 {
		t.Fatalf("tope inicial %d", r.tope())
	}
	for _, quiero := range []int{4, 2, 2} {
		r.retroceder()
		if got := r.tope(); got != quiero {
			t.Fatalf("tras retroceder: tope %d, quiero %d", got, quiero)
		}
	}
	if r.retrocesos() != 3 {
		t.Fatalf("retrocesos %d", r.retrocesos())
	}
	ahora = ahora.Add(plazoRetroceso - time.Second)
	if r.tope() != 2 {
		t.Fatalf("antes del plazo: %d", r.tope())
	}
	ahora = ahora.Add(time.Second)
	if r.tope() != 3 {
		t.Fatalf("un plazo: %d", r.tope())
	}
	ahora = ahora.Add(10 * plazoRetroceso)
	if r.tope() != 8 {
		t.Fatalf("recuperado: %d, no debe pasar de la base", r.tope())
	}
	// Un tope base de 1 (KLING_MAX_PARALLEL_BOOT=1) no sube a 2.
	u := &retrocesoPuerta{base: 1, reloj: func() time.Time { return ahora }}
	u.retroceder()
	if u.tope() != 1 {
		t.Fatalf("base 1: %d", u.tope())
	}
}

// La puerta real respeta el tope bajado: con 4 de capacidad y un retroceso a 2,
// solo dos entran a la vez.
func TestPuertaRetrocedeLimitaEntradas(t *testing.T) {
	m := &Manager{launchGate: make(chan struct{}, 4)}
	ahora := time.Unix(1000, 0)
	m.retrocesoDe().reloj = func() time.Time { return ahora }
	m.puertaRetroceder("test")
	var sueltas []func()
	for i := 0; i < 2; i++ {
		r, err := m.enterLaunch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sueltas = append(sueltas, r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if r, err := m.enterLaunch(ctx); err == nil {
		r()
		t.Fatal("entró un tercero con el tope en 2")
	}
	// Pasado el plazo, el tope vuelve a 3 y entra uno más.
	ahora = ahora.Add(plazoRetroceso)
	r3, err := m.enterLaunch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r3()
	for _, s := range sueltas {
		s()
	}
	ahora = ahora.Add(10 * plazoRetroceso)
	var todas []func()
	for i := 0; i < 4; i++ {
		r, err := m.enterLaunch(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		todas = append(todas, r)
	}
	for _, s := range todas {
		s()
	}
	if len(m.launchGate) != 0 {
		t.Fatalf("huecos sin soltar: %d", len(m.launchGate))
	}
}
