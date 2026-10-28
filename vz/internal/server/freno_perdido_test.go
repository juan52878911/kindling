package server

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// frenoMuerto es el Freeze de un proceso freno que ya no está.
func frenoMuerto(bool) error {
	return fmt.Errorf("%w: EOF", ErrFreezeLost)
}

// Si el proceso freno muere con el auxiliar parado, nadie puede soltarlo: el
// servidor termina con un error que dice por qué (kling-vz sale sin Stop y el
// sistema se lleva el auxiliar), en vez de seguir con una VM que figura
// running y no corre.
func TestFrenoPerdidoConElAuxiliarParado(t *testing.T) {
	var mu sync.Mutex
	var avisos []string
	r := newRig(t)
	r.srv.d.Logf = func(f string, a ...any) {
		mu.Lock()
		avisos = append(avisos, fmt.Sprintf(f, a...))
		mu.Unlock()
	}
	r.srv.d.Freeze = frenoMuerto
	congelar(r.srv)
	r.srv.soltarFreno()
	if sigueCongelado(r.srv) {
		t.Fatal("sigue marcado como congelado tras perder el freno")
	}
	select {
	case err := <-r.srv.Done():
		if !errors.Is(err, ErrFreezeLost) {
			t.Fatalf("Done = %v, quería ErrFreezeLost", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("el servidor no terminó con el auxiliar parado sin freno")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(avisos) != 1 || !strings.Contains(avisos[0], "CPU brake process died") {
		t.Fatalf("avisos = %q", avisos)
	}
}

// Si el freno murió después de soltar el auxiliar (o lo soltó al recibir un
// SIGTERM), el auxiliar corre: no hay que terminar, basta con seguir sin
// freno.
func TestFrenoPerdidoConElAuxiliarYaSuelto(t *testing.T) {
	r := newRig(t)
	r.srv.d.Freeze = frenoMuerto
	r.srv.d.HelperStopped = func() (bool, error) { return false, nil }
	congelar(r.srv)
	r.srv.soltarFreno()
	if sigueCongelado(r.srv) {
		t.Fatal("sigue marcado como congelado")
	}
	select {
	case err := <-r.srv.Done():
		t.Fatalf("terminó con el auxiliar corriendo: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	// Y con el auxiliar de verdad parado, sí termina.
	r2 := newRig(t)
	r2.srv.d.Freeze = frenoMuerto
	r2.srv.d.HelperStopped = func() (bool, error) { return true, nil }
	congelar(r2.srv)
	r2.srv.soltarFreno()
	select {
	case err := <-r2.srv.Done():
		if !errors.Is(err, ErrFreezeLost) {
			t.Fatalf("Done = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no terminó con el auxiliar parado")
	}
}

// La medida de CPU va fuera de s.mu: con proc_pid_rusage lento (un Mac
// cargado), el API no espera al regulador.
func TestMedidaLentaNoBloqueaElAPI(t *testing.T) {
	m := nuevoSimulado(1)
	r := rigSimulado(t, 1, m, true, nil)
	dentro, soltar := make(chan struct{}, 1), make(chan struct{})
	r.srv.d.CPUTime = func() (time.Duration, error) {
		select {
		case dentro <- struct{}{}:
		default:
		}
		<-soltar
		return m.CPUTime()
	}
	defer close(soltar)
	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	select {
	case <-dentro:
	case <-time.After(5 * time.Second):
		t.Fatal("el regulador no llegó a medir")
	}
	hecho := make(chan int, 1)
	go func() {
		code, _ := r.call("GET", "/", "")
		hecho <- code
	}()
	select {
	case code := <-hecho:
		if code != 200 {
			t.Fatalf("GET / = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GET / esperó a que terminara la medida de CPU")
	}
}

// Si muere sin nada parado, el techo sigue con pausas del framework: se avisa
// una vez y no se le vuelve a pedir nada al freno.
func TestFrenoPerdidoSinNadaParadoPausaLaVM(t *testing.T) {
	m := nuevoSimulado(1)
	var mu sync.Mutex
	var avisos []string
	r := rigSimulado(t, 1, m, true, func(f string, a ...any) {
		mu.Lock()
		avisos = append(avisos, f)
		mu.Unlock()
	})
	var pedidas int
	r.srv.d.Freeze = func(stop bool) error {
		mu.Lock()
		pedidas++
		mu.Unlock()
		return frenoMuerto(stop)
	}
	vm, _, _ := r.f.last()
	r.must("PUT", "/kling/cpu", `{"pct":50}`)
	limite := time.Now().Add(5 * time.Second)
	for {
		pausas := 0
		for _, op := range vm.ops() {
			if op == "pause" {
				pausas++
			}
		}
		if pausas >= 3 {
			break
		}
		if time.Now().After(limite) {
			t.Fatalf("no pausó por el framework: %v", vm.ops())
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if pedidas != 1 {
		t.Fatalf("se le pidió %d veces al freno muerto, quería 1", pedidas)
	}
	n := 0
	for _, a := range avisos {
		if strings.Contains(a, "could not stop the VM's helper") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d avisos, quería 1: %q", n, avisos)
	}
	select {
	case err := <-r.srv.Done():
		t.Fatalf("terminó sin tener nada parado: %v", err)
	default:
	}
}
