package machine

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// GET /machines/{ref}/ready sin ?wait (UnaVez): un agente que contesta un 5xx
// o que no contesta no es un invitado listo. Antes devolvía Ready "" y nil, y
// `kling machine ready` decía "ready (no probe)" y salía con 0.
func TestWaitReadyUnaVezErrorNoEsListo(t *testing.T) {
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	m.byID[id].State = api.StateRunning
	res, err := m.WaitReady(context.Background(), id, OpcionesListo{UnaVez: true})
	if err != nil || res.OK() || res.Ready != api.ReadyWaiting || !strings.Contains(res.Detail, "500") {
		t.Fatalf("un 500 del agente: %+v, %v", res, err)
	}
	if mc, _ := m.Get(id); mc.Ready != api.ReadyUnknown {
		t.Errorf("no se sabe nada de la imagen: Machine.Ready no cambia (%q)", mc.Ready)
	}
}

func TestWaitReadyUnaVezSinAgente(t *testing.T) {
	m := &Manager{byID: map[string]*api.Machine{}}
	id := "abcdef0123456789"
	// Nadie escucha en 127.0.0.1:1. La imagen "img" no existe: no se sabe si
	// declara algo.
	m.byID[id] = &api.Machine{ID: id, Name: "x", State: api.StateRunning, Image: "img",
		Forwards: map[string]string{"8080": "127.0.0.1:1"}}
	res, err := m.WaitReady(context.Background(), id, OpcionesListo{UnaVez: true})
	if err != nil || res.OK() || res.Detail == "" {
		t.Fatalf("sin agente y sin saber qué declara la imagen: %+v, %v", res, err)
	}
	// Si ya contestó "waiting" antes, tampoco.
	m.byID[id].Ready = api.ReadyWaiting
	if res, _ := m.WaitReady(context.Background(), id, OpcionesListo{UnaVez: true}); res.OK() {
		t.Fatalf("imagen que declara sonda, sin agente: %+v", res)
	}
	// Una máquina sin imagen no declara nada: sin agente no hay qué esperar.
	m.byID[id].Ready, m.byID[id].Image = api.ReadyUnknown, ""
	if res, err := m.WaitReady(context.Background(), id, OpcionesListo{UnaVez: true}); err != nil || !res.OK() {
		t.Fatalf("sin nada declarado: %+v, %v", res, err)
	}
}

// Sin poder mirar la imagen (sin debugfs), commit no da por listo a un
// invitado cuyo agente aún no contesta: antes, ErrNoDebugfs era "no declara
// nada" y se congelaba al momento. Con la gracia agotada sin que conteste
// nadie, es una imagen sin agente y se congela.
func TestListoParaCongelarAnteLaDuda(t *testing.T) {
	buf := capturarLog(t)
	m := &Manager{byID: map[string]*api.Machine{}}
	id := "abcdef0123456789"
	m.byID[id] = &api.Machine{ID: id, Name: "x", State: api.StateRunning, Image: "img",
		Forwards: map[string]string{"8080": "127.0.0.1:1"}}
	if err := m.listoParaCongelar(context.Background(), id, 300*time.Millisecond); !errors.Is(err, ErrNotReady) {
		t.Fatalf("sin saber qué declara la imagen y sin agente: %v", err)
	}
	if err := m.listoParaCongelar(context.Background(), id, 300*time.Millisecond); !errors.Is(err, ErrNotReady) {
		t.Fatalf("la segunda vez: %v", err)
	}
	if n := strings.Count(buf.String(), "cannot tell whether image img"); n != 1 {
		t.Errorf("avisos de la duda = %d, quiero 1 por imagen:\n%s", n, buf)
	}
	t0 := time.Now()
	res, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: time.Minute, GraciaAgente: 200 * time.Millisecond})
	if err != nil || !res.OK() || time.Since(t0) > 5*time.Second {
		t.Fatalf("gracia agotada sin agente: %+v, %v en %s", res, err, time.Since(t0))
	}
	// Con un agente que contesta, la gracia no cuenta: manda su /ready.
	ma, ida := conAgenteListo(t, &agenteListo{listoEn: 1 << 30})
	if _, err := ma.WaitReady(context.Background(), ida, OpcionesListo{Plazo: 600 * time.Millisecond, GraciaAgente: time.Millisecond}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("con agente que dice waiting: %v", err)
	}
}

// Si el /resync de una restauración falla, la copia trae en la memoria del
// dorado "hooks: done" y su /ready dice listo; trasRestaurar no lanzaba sus
// ganchos y run -from -wait-ready daba por buena una copia sin identidad
// propia.
func TestTrasRestaurarResyncFallidoLanzaGanchos(t *testing.T) {
	a := &agenteListo{ganchos: true, listoEn: 1}
	m, id := conAgenteListo(t, a)
	m.quit = make(chan struct{})
	defer close(m.quit)
	m.trasRestaurar(context.Background(), id, api.ResyncInstance, nil)
	if _, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	plazo := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		lanzados := append([]string(nil), a.lanzados...)
		a.mu.Unlock()
		if len(lanzados) == 1 && lanzados[0] == api.ResyncInstance {
			break
		}
		if len(lanzados) > 1 || time.Now().After(plazo) {
			t.Fatalf("ganchos lanzados = %v; quiero uno, de la instancia", lanzados)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, ok := m.ganchosPendientes.Load(id); ok {
		t.Error("lanzados, ya no están pendientes")
	}
}

// Y si no se pueden lanzar, no está lista, aunque el /ready del invitado diga
// lo del dorado, hasta que se lancen a mano.
func TestGanchosPendientesQueNoSeLanzan(t *testing.T) {
	a := &agenteListo{ganchos: true, listoEn: 1}
	m, id := conAgenteListo(t, a)
	var falla atomic.Bool
	falla.Store(true)
	m.pruebasGanchos = func(ctx context.Context, id, kind string) (api.GuestReady, error) {
		if falla.Load() {
			return api.GuestReady{}, errors.New("guest answered 500 to /hooks")
		}
		return api.GuestReady{Ready: true, HasHooks: true, Hooks: api.HooksDone}, nil
	}
	// Sin la vigía de fondo: la tanda pendiente la encuentra WaitReady.
	m.ganchosPendientes.Store(id, &ganchosPendientes{kind: api.ResyncInstance})
	for i := 0; i < 2; i++ {
		res, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: 5 * time.Second})
		if !errors.Is(err, ErrNotReady) || res.Ready != api.ReadyFailed || !strings.Contains(err.Error(), "resync failed") {
			t.Fatalf("vuelta %d: %+v, %v", i, res, err)
		}
	}
	falla.Store(false)
	if _, err := m.RunHooks(context.Background(), id, 5*time.Second); err != nil {
		t.Fatalf("lanzados a mano: %v", err)
	}
	if res, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: 5 * time.Second}); err != nil || res.Ready != api.ReadyYes {
		t.Fatalf("tras kling machine hooks: %+v, %v", res, err)
	}
}

func TestInterpretarGanchosCuerpoRaro(t *testing.T) {
	buf := capturarLog(t)
	st, err := interpretarGanchos(http.StatusAccepted, []byte("<html>"))
	if err != nil || !st.HasHooks || st.Hooks != api.HooksRunning || st.Ready {
		t.Fatalf("202 sin JSON = %+v, %v", st, err)
	}
	if !strings.Contains(buf.String(), "not JSON") {
		t.Errorf("sin aviso: %q", buf)
	}
}
