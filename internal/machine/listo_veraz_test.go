package machine

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
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
	m.trasRestaurar(context.Background(), id, api.ResyncInstance, resultadoResync{fallo: true})
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

// Una tanda pendiente que no se pudo lanzar era de esa restauración: tras
// stop + start (en frío) WaitReady no la arrastra ni lanza un /hooks de copia
// a un invitado recién arrancado. Antes solo la quitaban rm, kling machine
// hooks u otra restauración.
func TestGanchosPendientesNoSobrevivenAParar(t *testing.T) {
	a := &agenteListo{ganchos: true, listoEn: 1}
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	m := newTestManager(t)
	m.bus = events.New()
	m.priv = &Privileges{}
	id := "abcdef0123456789"
	m.addForTest(id)
	enMarcha := func() {
		m.mu.Lock()
		m.byID[id].State = api.StateRunning
		m.byID[id].Forwards = map[string]string{"8080": strings.TrimPrefix(srv.URL, "http://")}
		m.mu.Unlock()
	}
	enMarcha()
	var lanzadas atomic.Int32
	m.pruebasGanchos = func(ctx context.Context, id, kind string) (api.GuestReady, error) {
		lanzadas.Add(1)
		return api.GuestReady{}, errors.New("guest answered 500 to /hooks")
	}
	m.ganchosPendientes.Store(id, &ganchosPendientes{kind: api.ResyncInstance})
	if _, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: 5 * time.Second}); err == nil ||
		!strings.Contains(err.Error(), "resync failed") {
		t.Fatalf("antes de parar: %v", err)
	}
	if _, err := m.Stop(id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.reclamarParada(id); err != nil {
		t.Fatal(err)
	}
	enMarcha()
	antes := lanzadas.Load()
	res, err := m.WaitReady(context.Background(), id, OpcionesListo{Plazo: 5 * time.Second})
	if err != nil || res.Ready != api.ReadyYes || (res.Guest != nil && res.Guest.Hooks == api.HooksFailed) {
		t.Fatalf("tras stop + start: %+v, %v", res, err)
	}
	if n := lanzadas.Load() - antes; n != 0 {
		t.Fatalf("tras stop + start se lanzaron %d tandas de ganchos de copia", n)
	}

	// Una que se paró sola (sin Stop, p. ej. el VMM murió): la quita el
	// arranque.
	m.ganchosPendientes.Store(id, &ganchosPendientes{kind: api.ResyncInstance})
	m.mu.Lock()
	m.byID[id].State, m.byID[id].PID = api.StateStopped, 0
	m.mu.Unlock()
	if _, err := m.reclamarParada(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.ganchosPendientes.Load(id); ok {
		t.Fatal("el arranque en frío heredó la tanda pendiente")
	}
}

// agenteTardio sirve a en una dirección que no escucha hasta pasado tarde: un
// invitado cuyo agente tarda en contestar la primera vez.
func agenteTardio(t *testing.T, h http.Handler, tarde time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	srv := &http.Server{Handler: h}
	t.Cleanup(func() { srv.Close() })
	go func() {
		time.Sleep(tarde)
		if ln, err := net.Listen("tcp", addr); err == nil {
			_ = srv.Serve(ln)
		}
	}()
	return addr
}

// Sin poder mirar la imagen, la gracia para la primera respuesta del agente
// sale del plazo del commit (tres cuartos), no de un fijo: un agente que
// contesta pasada la gracia mínima, pero dentro del plazo, no se toma por una
// imagen sin agente y se espera a su sonda.
func TestListoParaCongelarAgenteTardio(t *testing.T) {
	antes := graciaAgenteMin
	graciaAgenteMin = 100 * time.Millisecond
	t.Cleanup(func() { graciaAgenteMin = antes })
	for _, c := range []struct {
		nombre string
		a      *agenteListo
		listo  bool
	}{
		{"listo", &agenteListo{listoEn: 1}, true},
		{"aún arrancando", &agenteListo{listoEn: 1 << 30}, false},
	} {
		t.Run(c.nombre, func(t *testing.T) {
			m := &Manager{byID: map[string]*api.Machine{}}
			id := "abcdef0123456789"
			m.byID[id] = &api.Machine{ID: id, Name: "x", State: api.StateRunning, Image: "img",
				Forwards: map[string]string{"8080": agenteTardio(t, c.a, 500*time.Millisecond)}}
			err := m.listoParaCongelar(context.Background(), id, 2*time.Second)
			c.a.mu.Lock()
			llamadas := c.a.llamadas
			c.a.mu.Unlock()
			if llamadas == 0 {
				t.Fatalf("congeló sin esperar al agente (%v)", err)
			}
			if c.listo != (err == nil) || (!c.listo && !errors.Is(err, ErrNotReady)) {
				t.Fatalf("listoParaCongelar = %v; listo %v", err, c.listo)
			}
		})
	}
	if g := graciaAgente(0); g != DefaultReadyWait*3/4 {
		t.Errorf("gracia con el plazo por defecto = %s", g)
	}
	graciaAgenteMin = vigiaAgenteMax
	if g := graciaAgente(5 * time.Second); g != vigiaAgenteMax {
		t.Errorf("gracia con un plazo corto = %s; nunca menos de %s", g, vigiaAgenteMax)
	}
}

// Solo un /resync que falla con un agente que debía contestar deja ganchos
// pendientes. Sin dirección, un agente que no sabe hacerlo (Lacks o 404),
// nadie escuchando o una imagen que se sabe sin agente no tienen ganchos que
// lanzar: antes cada una de esas restauraciones apuntaba una tanda y lanzaba
// una vigía de hasta 4 consultas de 5 s.
func TestResyncFalloSoloConAgenteQueDebiaContestar(t *testing.T) {
	capturarLog(t)
	con := func(code int) (*Manager, string) {
		return managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "x", code)
		}))
	}
	m, id := con(http.StatusInternalServerError)
	if r := m.resyncGuest(context.Background(), id, "", api.ResyncInstance); r.ok || !r.fallo {
		t.Errorf("un 500: %+v, quería fallo", r)
	}
	m, id = con(http.StatusNotFound)
	if r := m.resyncGuest(context.Background(), id, "", api.ResyncInstance); r.fallo {
		t.Errorf("un agente sin /resync (404): %+v, no es un fallo", r)
	}
	m, id = con(http.StatusInternalServerError)
	m.byID[id].Agent = &api.GuestAgent{Agent: "kling-guest", Version: "v9.0.0", Caps: []string{api.GuestCapMCP}}
	if r := m.resyncGuest(context.Background(), id, "", api.ResyncInstance); r.fallo {
		t.Errorf("un agente que no anuncia resync: %+v, no es un fallo", r)
	}
	// Nadie escucha: RST.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cerrado := l.Addr().String()
	l.Close()
	m.byID[id].Agent, m.byID[id].Forwards = nil, map[string]string{"8080": cerrado}
	clave := claveSnapshot(&api.Snapshot{Name: "dorado", CreatedAt: time.Now()})
	if r := m.resyncGuest(context.Background(), id, clave, api.ResyncInstance); r.fallo {
		t.Errorf("nadie escucha: %+v, no es un fallo", r)
	}
	// La siguiente copia del mismo dorado ni lo intenta.
	if r := m.resyncGuest(context.Background(), id, clave, api.ResyncInstance); r.fallo || r.took != 0 {
		t.Errorf("dorado que se sabe sin agente: %+v", r)
	}
	m.byID[id].Forwards = nil
	if r := m.resyncGuest(context.Background(), id, "", api.ResyncInstance); r.fallo {
		t.Errorf("sin dirección: %+v, no es un fallo", r)
	}
}

// Y sin fallo, trasRestaurar no apunta nada ni lanza la vigía.
func TestTrasRestaurarSinFalloNoDejaPendientes(t *testing.T) {
	var consultas atomic.Int32
	m, id := managerConInvitado(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == api.GuestReadyPath || r.URL.Path == api.GuestHooksPath {
			consultas.Add(1)
		}
		http.NotFound(w, r)
	}))
	m.byID[id].State = api.StateRunning
	m.quit = make(chan struct{})
	defer close(m.quit)
	m.trasRestaurar(context.Background(), id, api.ResyncInstance, resultadoResync{})
	if _, ok := m.ganchosPendientes.Load(id); ok {
		t.Fatal("una restauración sin agente al día dejó ganchos pendientes")
	}
	if mc, _ := m.Get(id); mc.HooksPending != "" {
		t.Errorf("HooksPending = %q", mc.HooksPending)
	}
	time.Sleep(3 * pasoListo)
	if n := consultas.Load(); n != 0 {
		t.Errorf("la vigía consultó %d veces a un invitado sin nada pendiente", n)
	}
}

// La tanda pendiente sobrevive a un reinicio del daemon: va en state.json
// (HooksPending) y load la vuelve a apuntar; al lanzarse se borra de ambos.
func TestGanchosPendientesSobrevivenAlReinicio(t *testing.T) {
	m := newTestManager(t)
	id := "abcdef0123456789"
	m.addForTest(id)
	m.apuntarGanchosPendientes(id, &ganchosPendientes{kind: api.ResyncInstance})
	m.Close()
	var enDisco string
	for _, f := range readState(t, m) {
		if f.ID == id {
			enDisco = f.HooksPending
		}
	}
	if enDisco != api.ResyncInstance {
		t.Fatalf("HooksPending en state.json = %q, quería %q", enDisco, api.ResyncInstance)
	}

	m2 := newTestManager(t)
	m2.root = m.root
	m2.load()
	v, ok := m2.ganchosPendientes.Load(id)
	if !ok || v.(*ganchosPendientes).kind != api.ResyncInstance {
		t.Fatalf("tras reiniciar, la tanda pendiente se perdió (%v, %v)", v, ok)
	}
	if !m2.olvidarGanchosPendientes(id, nil) {
		t.Fatal("no había nada que olvidar")
	}
	if mc, _ := m2.Get(id); mc.HooksPending != "" {
		t.Errorf("HooksPending tras lanzarla = %q", mc.HooksPending)
	}
}

// GET /machines/{ref}/ready sin poder mirar la imagen (sin debugfs): durante
// la gracia, un agente que no contesta es "waiting"; pasada la gracia sin
// que haya contestado nunca, es una imagen sin agente (como el commit con
// GraciaAgente). Si alguna vez contestó, sigue siendo "waiting".
func TestWaitReadyUnaVezSinDebugfsTrasLaGracia(t *testing.T) {
	antes := graciaAgenteMin
	graciaAgenteMin = 200 * time.Millisecond
	t.Cleanup(func() { graciaAgenteMin = antes })
	capturarLog(t)
	m := &Manager{byID: map[string]*api.Machine{}}
	id := "abcdef0123456789"
	ahora := time.Now()
	m.byID[id] = &api.Machine{ID: id, Name: "x", State: api.StateRunning, Image: "img", StartedAt: &ahora,
		Forwards: map[string]string{"8080": "127.0.0.1:1"}}
	if res, err := m.WaitReady(context.Background(), id, OpcionesListo{UnaVez: true}); err != nil || res.OK() {
		t.Fatalf("recién arrancada, sin agente aún: %+v, %v", res, err)
	}
	hace := time.Now().Add(-time.Second)
	m.byID[id].StartedAt = &hace
	if res, err := m.WaitReady(context.Background(), id, OpcionesListo{UnaVez: true}); err != nil || !res.OK() {
		t.Fatalf("pasada la gracia sin agente: %+v, %v", res, err)
	}
	m.byID[id].Agent = &api.GuestAgent{Agent: "kling-guest", Version: "v9.0.0"}
	if res, _ := m.WaitReady(context.Background(), id, OpcionesListo{UnaVez: true}); res.OK() {
		t.Fatalf("un agente que contestó y ya no: %+v", res)
	}
}

// Una copia cuyo /resync falló y que se congela antes de que nadie lance su
// tanda (instance) la corre en el thaw siguiente, como instance: si el thaw
// la olvidara, la copia no correría nunca sus ganchos de identidad.
func TestThawLanzaLaTandaInstancePendiente(t *testing.T) {
	a := &agenteListo{ganchos: true, listoEn: 1}
	m, id := conAgenteListo(t, a)
	var kinds []string
	var mu sync.Mutex
	m.pruebasGanchos = func(ctx context.Context, id, kind string) (api.GuestReady, error) {
		mu.Lock()
		kinds = append(kinds, kind)
		mu.Unlock()
		return api.GuestReady{Ready: true, HasHooks: true, Hooks: api.HooksDone}, nil
	}
	m.ganchosPendientes.Store(id, &ganchosPendientes{kind: api.ResyncInstance})
	m.trasRestaurar(context.Background(), id, api.ResyncThaw,
		resultadoResync{listo: &api.GuestReady{Ready: true, HasHooks: true, Hooks: api.HooksDone}})
	mu.Lock()
	defer mu.Unlock()
	if len(kinds) != 1 || kinds[0] != api.ResyncInstance {
		t.Fatalf("ganchos lanzados = %v; quería uno, instance", kinds)
	}
	if _, ok := m.ganchosPendientes.Load(id); ok {
		t.Error("lanzados, ya no están pendientes")
	}
}
