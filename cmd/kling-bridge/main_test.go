package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

// nuevaSesionPrueba crea una sesión con un proceso real (sleep) para que el
// session.close() pueda llamar a cmd.Kill() y cmd.Wait() sin panicar. El
// proceso se mata al final del test.
func nuevaSesionPrueba(t *testing.T, lastUse time.Time) *session {
	t.Helper()
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("no pude lanzar sleep: %v", err)
	}
	// stdin tiene que ser no-nil porque close() lo cierra.
	r, w, err := os.Pipe()
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("no pude crear pipe: %v", err)
	}
	_ = r.Close()
	t.Cleanup(func() {
		// Si la sesión sigue viva al final del test, la matamos.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return &session{
		id:      "test-" + itoa(int(cmd.Process.Pid)),
		cmd:     cmd,
		stdin:   w,
		lastUse: lastUse,
		pending: map[string]chan json.RawMessage{},
		notes:   make(chan json.RawMessage, 32),
		done:    make(chan struct{}),
	}
}

// itoa es un conversor mínimo para no depender de strconv en un test que solo
// construye IDs. fmt.Sprintf sería más limpio pero introduce strings en una
// función que no quiere ese ruido.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// TestReapIdleSaleEnCtxCancel: la goroutine de reapIdle responde al ctx y
// termina. Sin un shutdown limpio del bridge, esta goroutine se quedaba viva
// hasta que el proceso moría (el ctx nunca se cancelaba). Aquí demostramos que
// la cancel path interna funciona.
func TestReapIdleSaleEnCtxCancel(t *testing.T) {
	b := &bridge{
		argv:     []string{"sleep", "60"},
		idle:     50 * time.Millisecond,
		sessions: map[string]*session{},
	}

	before := runtime.NumGoroutine()

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		b.reapIdle(ctx)
		close(done)
	}()

	// La goroutine está corriendo.
	time.Sleep(20 * time.Millisecond)
	if runtime.NumGoroutine() <= before {
		t.Fatalf("reapIdle no lanzó goroutine: antes=%d ahora=%d",
			before, runtime.NumGoroutine())
	}

	cancel()

	select {
	case <-done:
		// reapIdle salió limpiamente.
	case <-time.After(2 * time.Second):
		t.Fatal("reapIdle no salió tras cancel del ctx")
	}

	// Tras salir, la goroutine tiene que haber desaparecido del runtime.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutine de reapIdle no salió: antes=%d ahora=%d",
		before, runtime.NumGoroutine())
}

// TestReapIdleCierraSesionesViejas: las sesiones con lastUse más allá de idle
// se cierran en el siguiente tick. Antes del shutdown limpio del bridge este
// era el ÚNICO camino para cerrar sesiones huérfanas; sigue siendo el camino
// principal durante la vida del proceso.
func TestReapIdleCierraSesionesViejas(t *testing.T) {
	b := &bridge{
		argv:     []string{"sleep", "60"},
		idle:     30 * time.Millisecond,
		sessions: map[string]*session{},
	}

	vieja := nuevaSesionPrueba(t, time.Now().Add(-time.Hour))
	fresca := nuevaSesionPrueba(t, time.Now())

	b.mu.Lock()
	b.sessions[vieja.id] = vieja
	b.sessions[fresca.id] = fresca
	b.mu.Unlock()

	var wg sync.WaitGroup
	wg.Add(1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer wg.Done()
		b.reapIdle(ctx)
	}()
	// defers en orden LIFO: cancel() corre antes que wg.Wait() para que el
	// reapIdle vea el ctx.Done() y salga en vez de quedarse en el ticker.
	defer wg.Wait()
	defer cancel()

	// El ticker dispara cada idle/4 = ~7ms. Esperamos al menos un tick.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		vieja.mu.Lock()
		viejaclosed := vieja.closed
		vieja.mu.Unlock()
		if viejaclosed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	vieja.mu.Lock()
	viejaclosed := vieja.closed
	vieja.mu.Unlock()
	if !viejaclosed {
		t.Fatal("la sesión vieja debería estar cerrada tras el reap")
	}

	fresca.mu.Lock()
	frescaclosed := fresca.closed
	fresca.mu.Unlock()
	if frescaclosed {
		t.Fatal("la sesión fresca NO debería estar cerrada aún")
	}
}

// TestCloseAllCierraTodasLasSesiones: closeAll cierra TODAS las sesiones, no
// solo las que reapIdle considera inactivas. Esto es lo que permite al bridge
// limpiar sus procesos hijo cuando recibe SIGTERM y no dejar zombis en la
// microVM.
func TestCloseAllCierraTodasLasSesiones(t *testing.T) {
	b := &bridge{
		argv:     []string{"sleep", "60"},
		idle:     30 * time.Millisecond,
		sessions: map[string]*session{},
	}

	s1 := nuevaSesionPrueba(t, time.Now())
	s2 := nuevaSesionPrueba(t, time.Now())
	s3 := nuevaSesionPrueba(t, time.Now())

	b.mu.Lock()
	b.sessions[s1.id] = s1
	b.sessions[s2.id] = s2
	b.sessions[s3.id] = s3
	b.mu.Unlock()

	b.closeAll()

	b.mu.Lock()
	n := len(b.sessions)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("closeAll dejó %d sesiones en el mapa", n)
	}

	for name, s := range map[string]*session{"s1": s1, "s2": s2, "s3": s3} {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if !closed {
			t.Fatalf("%s no quedó cerrada tras closeAll", name)
		}
	}

	// Idempotencia: llamar closeAll con el mapa vacío no debe panicar.
	b.closeAll()
}
