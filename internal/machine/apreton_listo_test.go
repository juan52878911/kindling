package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/fc"
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

// globosPedidos devuelve los amount_mib de los PATCH /balloon que recibió f.
func globosPedidos(t *testing.T, f *fcFalso) []int {
	t.Helper()
	var r []int
	for _, l := range f.llamadasA(http.MethodPatch, "/balloon") {
		var p struct {
			AmountMiB int `json:"amount_mib"`
		}
		if err := json.Unmarshal(l.Cuerpo, &p); err != nil {
			t.Fatal(err)
		}
		r = append(r, p.AmountMiB)
	}
	return r
}

// El apretón al estar lista infla el globo hasta todo lo disponible (sin el
// margen de 128 MiB de squeeze, que solo se llevaba la memoria libre de un
// arranque en frío) y lo desinfla a la línea base.
func TestApretonListoSinMargen(t *testing.T) {
	if !apretarAlEstarListaActivo() {
		t.Skip("sin apretón al estar lista en esta plataforma")
	}
	m, id, falso := maquinaParaApretonListo(t, true)
	m.bus = events.New()
	falso.mu.Lock()
	falso.globo = &fc.BalloonStats{AvailableMemory: 400 << 20, FreeMemory: 300 << 20, TotalMemory: 512 << 20}
	falso.mu.Unlock()
	m.vigilarListo(id, nil)
	limite := time.Now().Add(3 * time.Second)
	for len(globosPedidos(t, falso)) < 2 && time.Now().Before(limite) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := globosPedidos(t, falso); !slices.Equal(got, []int{400, 0}) {
		t.Fatalf("globo pedido %v; quería [400 0]: inflar a todo lo disponible y volver a la base", got)
	}
}

// squeeze deja su margen; y por pequeño que sea el margen, el globo nunca
// pasa del total menos sueloSqueezeMiB.
func TestSqueezeMargenYSuelo(t *testing.T) {
	for _, c := range []struct {
		nombre       string
		margen       int
		actual, disp int
		quiero       int
	}{
		{"squeeze", balloonSqueezeMarginMiB, 0, 400, 400 - balloonSqueezeMarginMiB},
		{"sin margen", 0, 0, 400, 400},
		{"suelo", 0, 100, 450, 512 - sueloSqueezeMiB},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			m, id, falso := maquinaParaApretonListo(t, true)
			m.bus = events.New()
			falso.globo = &fc.BalloonStats{ActualMiB: c.actual, AvailableMemory: int64(c.disp) << 20, TotalMemory: 512 << 20}
			if _, err := m.squeezeLocked(context.Background(), id, id, false, c.margen); err != nil {
				t.Fatal(err)
			}
			if got := globosPedidos(t, falso); len(got) == 0 || got[0] != c.quiero {
				t.Fatalf("globo pedido %v; quería inflar a %d", got, c.quiero)
			}
		})
	}
}
