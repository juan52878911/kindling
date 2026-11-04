package machine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Un intento colgado (lanzado antes de que el invitado tuviera su vsock) no
// se espera entero: el siguiente, que sale al escalón, contesta "abierto".
func TestSondearEscalonadoNoEsperaAlColgado(t *testing.T) {
	var n atomic.Int32
	intento := func(ctx context.Context) (bool, error) {
		if n.Add(1) == 1 {
			<-ctx.Done() // el primero se pierde
			return false, ctx.Err()
		}
		return true, nil
	}
	t0 := time.Now()
	open, err := sondearEscalonado(context.Background(), 2*time.Second, 10*time.Millisecond, 0, intento)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v, want true, nil", open, err)
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("took %s: it waited for the hung attempt", d)
	}
}

// Sin pasoCerrado, "cerrado" es la respuesta y no sale ningún intento más.
func TestSondearEscalonadoCerradoEsRespuesta(t *testing.T) {
	var n atomic.Int32
	intento := func(ctx context.Context) (bool, error) { n.Add(1); return false, nil }
	open, err := sondearEscalonado(context.Background(), time.Second, 10*time.Millisecond, 0, intento)
	if err != nil || open {
		t.Fatalf("open=%v err=%v, want false, nil", open, err)
	}
	time.Sleep(30 * time.Millisecond)
	if got := n.Load(); got != 1 {
		t.Fatalf("%d attempts, want 1", got)
	}
}

// Con pasoCerrado, un "cerrado" se repite hasta que algo escucha (el agente
// arrancando), dentro del plazo.
func TestSondearEscalonadoEsperaTrasCerrado(t *testing.T) {
	escucha := time.Now().Add(40 * time.Millisecond)
	var n atomic.Int32
	intento := func(ctx context.Context) (bool, error) {
		n.Add(1)
		return time.Now().After(escucha), nil
	}
	open, err := sondearEscalonado(context.Background(), time.Second, 20*time.Millisecond, 2*time.Millisecond, intento)
	if err != nil || !open {
		t.Fatalf("open=%v err=%v, want true, nil", open, err)
	}
	if d := time.Since(escucha); d > 100*time.Millisecond {
		t.Fatalf("answered %s after it started listening", d)
	}
	if n.Load() < 2 {
		t.Fatalf("%d attempts: it did not ask again", n.Load())
	}
}

// Con pasoCerrado y nadie que llegue a escuchar: "cerrado" al acabar el
// plazo, sin error y sin pasarse de él.
func TestSondearEscalonadoCerradoHastaElPlazo(t *testing.T) {
	intento := func(ctx context.Context) (bool, error) { return false, nil }
	t0 := time.Now()
	open, err := sondearEscalonado(context.Background(), 60*time.Millisecond, 20*time.Millisecond, 5*time.Millisecond, intento)
	if err != nil || open {
		t.Fatalf("open=%v err=%v, want false, nil", open, err)
	}
	if d := time.Since(t0); d < 50*time.Millisecond || d > time.Second {
		t.Fatalf("took %s, want about the deadline (60 ms)", d)
	}
}

// Todos colgados: "todavía no" (false, nil) al plazo, no un error.
func TestSondearEscalonadoTodosColgados(t *testing.T) {
	intento := func(ctx context.Context) (bool, error) { <-ctx.Done(); return false, ctx.Err() }
	open, err := sondearEscalonado(context.Background(), 50*time.Millisecond, 10*time.Millisecond, 0, intento)
	if err != nil || open {
		t.Fatalf("open=%v err=%v, want false, nil", open, err)
	}
}

// Un error del VMM (uno sin /kling/probe) se devuelve tal cual, y el ctx
// cancelado de quien pregunta también.
func TestSondearEscalonadoErrores(t *testing.T) {
	errVMM := errors.New("404")
	_, err := sondearEscalonado(context.Background(), time.Second, 10*time.Millisecond, 5*time.Millisecond,
		func(ctx context.Context) (bool, error) { return false, errVMM })
	if !errors.Is(err, errVMM) {
		t.Fatalf("err=%v, want the VMM's", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err = sondearEscalonado(ctx, 5*time.Second, 10*time.Millisecond, 0,
		func(ctx context.Context) (bool, error) { <-ctx.Done(); return false, ctx.Err() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}
