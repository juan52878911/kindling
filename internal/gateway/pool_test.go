package gateway

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// nuevoPoolPrueba construye un pool mínimo para tests: sin Gateway ni cliente
// real. El caller debe asignar p.warmer (y solo p.warmer, no se llama a Remove
// en estos tests porque mantenemos ready vacío).
func nuevoPoolPrueba(size int) *pool {
	return &pool{
		size:    size,
		ready:   map[string][]*warmVM{},
		filling: map[string]bool{},
	}
}

// warmerBloqueante devuelve un warmer que espera hasta que el ctx se cancele.
// Avisa por enEntry en cuanto entra (para que el test pueda sincronizarse con
// la goroutine de fill) y devuelve retErr al desbloquearse.
func warmerBloqueante(enEntry chan<- struct{}, retErr error) func(context.Context, string, string) (*warmVM, error) {
	return func(ctx context.Context, _, _ string) (*warmVM, error) {
		select {
		case enEntry <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, retErr
	}
}

// warmerExitoso devuelve un warmer que añade VMs a ready sin bloquear. Útil
// para verificar que fill() repone hasta `size` instancias.
func warmerExitoso(contador *atomic.Int32) func(context.Context, string, string) (*warmVM, error) {
	return func(_ context.Context, service, _ string) (*warmVM, error) {
		id := contador.Add(1)
		return &warmVM{
			id:   "vm-" + service + "-" + itoa(int(id)),
			ip:   "10.0.0." + itoa(int(id)),
			born: time.Now(),
		}, nil
	}
}

// TestPoolFillSaleEnCtxCancel verifica que la goroutine lanzada por fill()
// termina cuando el ctx se cancela. Antes del WaitGroup no había manera de
// ESPERAR a esa salida; ahora podemos comprobar con runtime.NumGoroutine que
// el contador vuelve al baseline.
func TestPoolFillSaleEnCtxCancel(t *testing.T) {
	p := nuevoPoolPrueba(2)

	exit := make(chan struct{}, 1)
	p.warmer = warmerBloqueante(exit, context.Canceled)

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	p.fill(ctx, "svc", "snap")

	// Esperar a que el warmer sea invocado (la goroutine arrancó).
	select {
	case <-exit:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("fill no llegó a invocar el warmer")
	}

	// La goroutine de fill está viva y bloqueada en el warmer.
	if runtime.NumGoroutine() <= before {
		t.Fatalf("esperaba goroutine extra, antes=%d ahora=%d",
			before, runtime.NumGoroutine())
	}

	// Cancelamos: el warmer debe retornar con error y la goroutine debe
	// terminar por el check de ctx.Err() al principio del bucle.
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("la goroutine de fill no salió tras ctx cancel: antes=%d ahora=%d",
		before, runtime.NumGoroutine())
}

// TestPoolFillNoLanzaDuplicado verifica que el flag `filling` impide lanzar
// dos goroutines para el mismo servicio. Con size=1 cada fill() solo pide UNA
// warm; las llamadas concurrentes durante el warm son no-op.
func TestPoolFillNoLanzaDuplicado(t *testing.T) {
	p := nuevoPoolPrueba(1)

	var llamadas atomic.Int32
	p.warmer = func(_ context.Context, _, _ string) (*warmVM, error) {
		llamadas.Add(1)
		// Bloquear para que las llamadas siguientes a fill() lleguen mientras
		// la primera goroutine está todavía en el warmer.
		time.Sleep(200 * time.Millisecond)
		return &warmVM{id: "vm", ip: "10.0.0.1", born: time.Now()}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.fill(ctx, "svc", "snap")
	p.fill(ctx, "svc", "snap") // no-op: filling=true o ready ya lleno
	p.fill(ctx, "svc", "snap") // idem

	// Esperamos a que la goroutine de la primera fill termine su trabajo.
	time.Sleep(400 * time.Millisecond)

	if got := llamadas.Load(); got != 1 {
		t.Fatalf("warmer llamado %d veces, esperaba 1 (filling evita duplicados)", got)
	}

	p.mu.Lock()
	ready := len(p.ready["svc"])
	p.mu.Unlock()
	if ready != 1 {
		t.Fatalf("ready tiene %d VMs, esperaba 1", ready)
	}
}

// TestPoolFillReponeHastaSize verifica que fill() rellena hasta `size` VMs.
func TestPoolFillReponeHastaSize(t *testing.T) {
	p := nuevoPoolPrueba(3)

	var contador atomic.Int32
	p.warmer = warmerExitoso(&contador)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.fill(ctx, "svc", "snap")

	// Esperamos a que las 3 goroutines de warm se completen. Usamos el WaitGroup
	// interno: si no sale en 2 segundos, hay un bug en el wiring de wg.
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("las goroutines de fill no terminaron")
	}

	p.mu.Lock()
	ready := len(p.ready["svc"])
	p.mu.Unlock()
	if ready != 3 {
		t.Fatalf("ready tiene %d VMs, esperaba 3", ready)
	}
	if got := contador.Load(); got != 3 {
		t.Fatalf("warmer llamado %d veces, esperaba 3", got)
	}
}

// TestPoolDrainEsperaFills verifica que drain() NO avanza mientras haya
// goroutines de fill en vuelo: espera a que terminen (vía wg.Wait) antes de
// vaciar ready. Esto cierra la ventana en la que un warm a medio crear podía
// dejar la VM huérfana.
func TestPoolDrainEsperaFills(t *testing.T) {
	p := nuevoPoolPrueba(2)

	// El primer warm se queda bloqueado hasta que cancelemos. El segundo warm
	// NUNCA llega porque la goroutine sale al primer error.
	enWarm := make(chan struct{}, 1)
	p.warmer = func(ctx context.Context, _, _ string) (*warmVM, error) {
		select {
		case enWarm <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, context.Canceled
	}

	ctx, cancel := context.WithCancel(context.Background())

	p.fill(ctx, "svc", "snap")

	// Esperar a que el primer warm esté en curso.
	select {
	case <-enWarm:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("fill no invocó el warmer")
	}

	// Lanzamos drain en segundo plano. SIN el wg.Wait() del fix, drain
	// pasaría inmediatamente a iterar ready (vacío) y volvería sin esperar.
	// CON el fix, drain se queda bloqueada hasta que la goroutine de fill
	// termine.
	drainDone := make(chan struct{})
	go func() {
		p.drain(context.Background())
		close(drainDone)
	}()

	// Dren no debe haber completado: fill sigue bloqueada en el warmer.
	select {
	case <-drainDone:
		cancel()
		t.Fatal("drain retornó antes de que las goroutines de fill salieran")
	case <-time.After(100 * time.Millisecond):
		// Bien: drain está bloqueada en wg.Wait().
	}

	// Cancelamos: el warmer retorna con error, la goroutine de fill sale, el
	// wg.Wait() de drain se libera y drain termina.
	cancel()

	select {
	case <-drainDone:
		// Éxito: drain solo retorna cuando wg.Wait() se libera, que solo pasa
		// cuando la goroutine de fill sale. Si antes del fix drain retornaba
		// "temprano" (sin esperar a fill), este select habría disparado
		// timeoutFailed más arriba.
	case <-time.After(2 * time.Second):
		t.Fatal("drain no completó tras cancel del ctx")
	}
}

// TestPoolFillConWarmerConErrorTermina rapido verifica que un warm que falla
// termina la goroutine de fill (en vez de quedarse atascada en un bucle
// infinito). Esto es el camino de fallo por VM, no por ctx.
func TestPoolFillConWarmerConErrorTerminaRapido(t *testing.T) {
	p := nuevoPoolPrueba(5)

	p.warmer = func(_ context.Context, _, _ string) (*warmVM, error) {
		return nil, errors.New("simulado: daemon caído")
	}

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p.fill(ctx, "svc", "snap")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("la goroutine de fill no salió tras error del warmer")
}
