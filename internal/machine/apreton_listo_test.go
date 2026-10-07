package machine

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// maquinaParaApretonListo deja en un Manager mínimo una máquina corriendo con
// un VMM falso (para ver si se le pide el globo) y un agente cuyo /ready
// contesta listo a la segunda pregunta, declarando sonda o no.
func maquinaParaApretonListo(t *testing.T, sonda bool) (*Manager, string, *fcFalso) {
	t.Helper()
	m := &Manager{byID: map[string]*api.Machine{}, socket: map[string]string{}, quit: make(chan struct{})}
	t.Cleanup(func() { close(m.quit) })
	id := "abcdef0123456789"
	m.byID[id] = &api.Machine{ID: id, Name: "pg", State: api.StateRunning, PID: 1}
	falso := nuevoFcFalso(t)
	m.socket[id] = falso.Sock
	var preguntas atomic.Int32
	m.pruebasListo = func(context.Context, string) (api.GuestReady, error) {
		if preguntas.Add(1) < 2 {
			return api.GuestReady{Probe: sonda}, nil
		}
		return api.GuestReady{Probe: sonda, Ready: true}, nil
	}
	return m, id, falso
}

// esperarLlamadas espera a que el VMM falso reciba alguna llamada a ruta, o a
// que pase plazo; devuelve cuántas recibió.
func esperarLlamadas(f *fcFalso, ruta string, plazo time.Duration) int {
	limite := time.Now().Add(plazo)
	for {
		n := len(f.llamadasA(http.MethodGet, ruta))
		if n > 0 || time.Now().After(limite) {
			return n
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Al pasar la sonda de un arranque en frío se aprieta el globo una vez
// (Firecracker); KLING_SQUEEZE_ON_READY=0 lo apaga.
func TestApretonAlPasarLaSonda(t *testing.T) {
	for _, c := range []struct {
		nombre  string
		apagado bool
		sonda   bool
		quiero  bool
	}{
		{"con sonda", false, true, !globoSinEstadisticas},
		{"apagado", true, true, false},
		{"sin sonda", false, false, false},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			if c.apagado {
				t.Setenv("KLING_SQUEEZE_ON_READY", "0")
			}
			m, id, falso := maquinaParaApretonListo(t, c.sonda)
			m.vigilarListo(id, nil)
			plazo := 3 * time.Second
			if !c.quiero {
				plazo = 2*4*pasoListo + 200*time.Millisecond
			}
			n := esperarLlamadas(falso, "/balloon/statistics", plazo)
			if c.quiero && n != 1 {
				t.Fatalf("el globo se miró %d veces al pasar la sonda; quería un apretón", n)
			}
			if !c.quiero && n != 0 {
				t.Fatalf("se apretó %d veces y no tocaba", n)
			}
		})
	}
}

// Tras un thaw (vigía con lo que contestó el /resync) no se aprieta: la
// memoria se comparte con el dorado.
func TestApretonListoNoTrasRestaurar(t *testing.T) {
	m, id, falso := maquinaParaApretonListo(t, true)
	m.vigilarListo(id, &api.GuestReady{Probe: true})
	if n := esperarLlamadas(falso, "/balloon/statistics", 2*4*pasoListo+200*time.Millisecond); n != 0 {
		t.Fatalf("se apretó %d veces tras restaurar", n)
	}
	if mc, _ := m.Get(id); mc == nil || mc.Ready != api.ReadyYes {
		got := ""
		if mc != nil {
			got = mc.Ready
		}
		t.Fatalf("la vigía no siguió la sonda: %q", got)
	}
}
