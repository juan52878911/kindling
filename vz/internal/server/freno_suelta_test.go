package server

import (
	"sync"
	"testing"
	"time"
)

// frenoFalso es un Deps.Freeze que solo apunta lo que le piden.
type frenoFalso struct {
	mu       sync.Mutex
	soltadas int
}

func (f *frenoFalso) Freeze(stop bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !stop {
		f.soltadas++
	}
	return nil
}

func (f *frenoFalso) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.soltadas
}

func congelar(s *Server) {
	s.frenoMu.Lock()
	s.congelado = true
	s.frenoMu.Unlock()
}

func sigueCongelado(s *Server) bool {
	s.frenoMu.Lock()
	defer s.frenoMu.Unlock()
	return s.congelado
}

// Si el SIGCONT del regulador falla, el auxiliar se queda parado. Parado no
// gasta CPU, el cubo se llena y el regulador no vuelve a frenar, que era lo
// único que llamaba a soltarlo otra vez: la VM quedaba parada para siempre,
// figurando running. Tiene que reintentarlo.
func TestReguladorReintentaSoltarElFreno(t *testing.T) {
	m := nuevoSimulado(1)
	m.fallarSoltar = 1
	r := rigSimulado(t, 1, m, true, nil)
	r.must("PUT", "/kling/cpu", `{"pct":10}`)

	limite := time.Now().Add(5 * time.Second)
	for {
		m.mu.Lock()
		consumido := m.fallarSoltar == 0
		m.mu.Unlock()
		if consumido {
			break
		}
		if time.Now().After(limite) {
			t.Fatal("el regulador no llegó a frenar y soltar")
		}
		time.Sleep(50 * time.Microsecond)
	}
	t0, _, _, _ := m.foto()
	m.hasta(t, t0.Add(2*time.Second))
	// El regulador frena y suelta continuamente con pct 10; lo que no puede
	// pasar es que el auxiliar se quede parado de forma permanente. Se mira
	// varias veces: parado un periodo es normal, parado siempre no.
	for i := 0; i < 200; i++ {
		m.mu.Lock()
		parado, desde, ahora := m.parado, m.desde, m.t
		m.mu.Unlock()
		if !parado || ahora.Sub(desde) < cpuPausaMax+cpuPeriodoDef {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("tras un SIGCONT fallido el auxiliar sigue parado: nadie lo reintentó")
}

// reaplicarGlobo le pedía al framework el globo con el auxiliar parado por el
// regulador, sin soltarlo antes como putBalloon.
func TestReaplicarGloboSueltaElFreno(t *testing.T) {
	r := newRig(t)
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	t.Cleanup(r.srv.Shutdown)
	f := &frenoFalso{}
	r.srv.d.Freeze = f.Freeze
	congelar(r.srv)
	r.srv.mu.Lock()
	vm := r.srv.vm
	r.srv.mu.Unlock()

	r.srv.reaplicarGlobo(vm, []time.Duration{0})
	if sigueCongelado(r.srv) || f.n() == 0 {
		t.Fatal("reaplicarGlobo movió el globo con el auxiliar parado")
	}
}

// vmConPantalla es una VM de prueba con captura de pantalla.
type vmConPantalla struct{ VM }

func (vmConPantalla) Screenshot() ([]byte, error) { return []byte("\x89PNG"), nil }

// getScreenshot, igual: con el auxiliar parado la ventana no pinta.
func TestScreenshotSueltaElFreno(t *testing.T) {
	r := newRig(t)
	r.configure(t.TempDir())
	r.must("PUT", "/actions", `{"action_type":"InstanceStart"}`)
	t.Cleanup(r.srv.Shutdown)
	f := &frenoFalso{}
	r.srv.d.Freeze = f.Freeze
	r.srv.mu.Lock()
	r.srv.vm = vmConPantalla{r.srv.vm}
	r.srv.mu.Unlock()
	congelar(r.srv)

	r.must("GET", "/kling/screenshot", "")
	if sigueCongelado(r.srv) || f.n() == 0 {
		t.Fatal("getScreenshot pidió la pantalla con el auxiliar parado")
	}
}
