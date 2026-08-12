package gateway

import (
	"context"
	"sync"
	"testing"
	"time"
)

// El pool real llama a warm(), que habla con el daemon. Para los tests se
// sustituye por un doble que cuenta lo que crea y lo que se retira, que es lo
// único que hace falta para saber si queda una microVM huérfana.
type fakeWarmer struct {
	mu      sync.Mutex
	created []string
	removed []string
	delay   time.Duration
	fail    bool
}

func (f *fakeWarmer) warm(ctx context.Context, service, snapshot string) (*warmVM, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.fail {
		return nil, context.DeadlineExceeded
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := service + "-" + time.Now().Format("150405.000000000")
	f.created = append(f.created, id)
	return &warmVM{id: id, ip: "10.0.0.1", session: "s"}, nil
}

func (f *fakeWarmer) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed = append(f.removed, id)
}

func (f *fakeWarmer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.removed)
}

// newTestPool arma un pool con el doble inyectado, sin gateway ni daemon.
func newTestPool(t *testing.T, size int, f *fakeWarmer) *pool {
	t.Helper()
	p := &pool{
		size:    size,
		ready:   map[string][]*warmVM{},
		filling: map[string]bool{},
	}
	p.warmFn = f.warm
	p.removeFn = func(ctx context.Context, id string) error { f.remove(id); return nil }
	return p
}

// El caso que el test de la rama no cubría: el fill empieza DESPUÉS de que
// drain haya mirado el contador. Con wg.Add fuera del mutex, drain veía cero,
// vaciaba `ready`, y esta microVM quedaba viva sin que nadie pudiera retirarla.
func TestDrainNoDejaHuerfanaLaQueLlegaTarde(t *testing.T) {
	f := &fakeWarmer{delay: 50 * time.Millisecond}
	p := newTestPool(t, 1, f)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Justo mientras drain está esperando.
		time.Sleep(10 * time.Millisecond)
		p.fill(context.WithoutCancel(context.Background()), "eco", "eco")
	}()

	p.drain(context.Background())
	wg.Wait()

	// Puede que el fill llegara antes o después del cierre; en cualquiera de los
	// dos casos, todo lo creado tiene que acabar retirado.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		created, removed := f.counts()
		if created == removed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	created, removed := f.counts()
	t.Errorf("quedaron microVMs huérfanas: creadas %d, retiradas %d", created, removed)
}

// Tras drain no debe pre-calentarse nada nuevo: el gateway se está apagando.
func TestFillNoArrancaTrasDrain(t *testing.T) {
	f := &fakeWarmer{}
	p := newTestPool(t, 2, f)
	p.drain(context.Background())

	p.fill(context.Background(), "eco", "eco")
	time.Sleep(100 * time.Millisecond)

	if created, _ := f.counts(); created != 0 {
		t.Errorf("pre-calentó %d instancias con el pool ya cerrado", created)
	}
}

// drain espera a las reposiciones en vuelo, para que se retiren antes de salir.
func TestDrainEsperaLoQueEstaEnVuelo(t *testing.T) {
	f := &fakeWarmer{delay: 80 * time.Millisecond}
	p := newTestPool(t, 1, f)
	p.fill(context.WithoutCancel(context.Background()), "eco", "eco")
	time.Sleep(10 * time.Millisecond) // que entre en warm

	start := time.Now()
	p.drain(context.Background())
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("drain volvió en %s: no esperó al fill en vuelo", elapsed)
	}
	if created, removed := f.counts(); created != removed {
		t.Errorf("creadas %d, retiradas %d", created, removed)
	}
}

// Y no espera para siempre: un warm colgado no puede secuestrar el apagado.
func TestDrainNoSeCuelgaConUnWarmEterno(t *testing.T) {
	f := &fakeWarmer{delay: time.Hour}
	p := newTestPool(t, 1, f)
	p.fill(context.WithoutCancel(context.Background()), "eco", "eco")
	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() { p.drain(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("drain se colgó esperando a un warm que no termina")
	}
}

// Un servicio no debe tener dos reposiciones a la vez: es lo que evita levantar
// veinte microVMs del mismo servicio ante una ráfaga.
func TestFillNoDuplicaReposicion(t *testing.T) {
	f := &fakeWarmer{delay: 40 * time.Millisecond}
	p := newTestPool(t, 3, f)
	for range 5 {
		p.fill(context.Background(), "eco", "eco")
	}
	time.Sleep(400 * time.Millisecond)

	if created, _ := f.counts(); created > 3 {
		t.Errorf("pre-calentó %d, más que el tamaño del fondo (3)", created)
	}
}
