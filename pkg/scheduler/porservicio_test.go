package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// daemonServicio imita lo justo del daemon para los ganchos por servicio:
// listar, restaurar del dorado (POST /machines), despertar, congelar, pausar,
// renovar y borrar. Las máquinas restauradas escuchan en 127.0.0.1 (el puerto
// lo pone PortFor apuntando a un servidor del test), así que esperarListo
// marca de verdad.
type daemonServicio struct {
	mu       sync.Mutex
	maquinas map[string]*api.Machine
	orden    []string
	llamadas []string
	n        int
	// pedidas son los cuerpos de POST /machines, en orden.
	pedidas []api.RunRequest
}

func nuevoDaemonServicio() *daemonServicio {
	return &daemonServicio{maquinas: map[string]*api.Machine{}}
}

// poner registra una máquina ya existente (para adoptar o descongelar).
func (d *daemonServicio) poner(mc *api.Machine) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.maquinas[mc.ID] = mc
	d.orden = append(d.orden, mc.ID)
}

func (d *daemonServicio) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.llamadas = append(d.llamadas, r.Method+" "+r.URL.Path)
	responder := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/info":
		responder(api.Info{Version: "test", Capabilities: []string{"exec", "renew", "pause"}})
		return
	case r.Method == http.MethodGet && r.URL.Path == "/snapshots":
		responder([]*api.Snapshot{{Name: "svc-gold", Labels: map[string]string{api.LabelService: "svc"}}})
		return
	case r.Method == http.MethodGet && r.URL.Path == "/machines":
		l := []*api.Machine{}
		for _, id := range d.orden {
			if mc, ok := d.maquinas[id]; ok {
				c := *mc
				l = append(l, &c)
			}
		}
		responder(l)
		return
	case r.Method == http.MethodPost && r.URL.Path == "/machines":
		var req api.RunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.pedidas = append(d.pedidas, req)
		d.n++
		id := "r" + strconv.Itoa(d.n)
		mc := &api.Machine{ID: id, Name: id, State: api.StateRunning, IP: "127.0.0.1",
			From: req.From, Labels: req.Labels, MemMiB: 128, OnTTL: req.OnTTL}
		d.maquinas[id] = mc
		d.orden = append(d.orden, id)
		c := *mc
		responder(&c)
		return
	}
	partes := strings.Split(strings.TrimPrefix(r.URL.Path, "/machines/"), "/")
	mc := d.maquinas[partes[0]]
	if mc == nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case r.Method == http.MethodGet && len(partes) == 1:
	case r.Method == http.MethodDelete && len(partes) == 1:
		delete(d.maquinas, partes[0])
		w.WriteHeader(http.StatusNoContent)
		return
	case r.Method == http.MethodPost && len(partes) == 2:
		switch partes[1] {
		case "thaw":
			mc.State = api.StateRunning
		case "freeze":
			mc.State = api.StateWarm
		case "pause":
			mc.State = api.StatePaused
		case "renew":
		default:
			http.NotFound(w, r)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}
	c := *mc
	responder(&c)
}

// contar dice cuántas llamadas con ese prefijo ("DELETE /machines/") vio.
func (d *daemonServicio) contar(prefijo string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, l := range d.llamadas {
		if strings.HasPrefix(l, prefijo) {
			n++
		}
	}
	return n
}

// pedidasCopia devuelve los cuerpos de POST /machines vistos hasta ahora.
func (d *daemonServicio) pedidasCopia() []api.RunRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]api.RunRequest(nil), d.pedidas...)
}

// existe dice si el daemon sigue teniendo la máquina id.
func (d *daemonServicio) existe(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.maquinas[id]
	return ok
}

// puertoDe da el puerto TCP en que escucha srv.
func puertoDe(t *testing.T, srv *httptest.Server) int {
	t.Helper()
	_, p, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return n
}

// puertoCerrado es un puerto de loopback en que no escucha nadie: el defecto
// (Port) apunta ahí para que solo PortFor lleve a un invitado que conteste.
const puertoCerrado = 1

// gwServicio arma un planificador contra d con el invitado de mentira srv
// detrás de PortFor.
func gwServicio(t *testing.T, d *daemonServicio, srv *httptest.Server) *Scheduler {
	t.Helper()
	g := gwConManejador(t, d)
	g.Port = puertoCerrado
	p := puertoDe(t, srv)
	g.PortFor = func(string) int { return p }
	return g
}

func invitadoOK(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(srv.Close)
	return srv
}

// PortFor: el puerto del servicio es el que se sondea al restaurar y al que
// apunta la instancia. Sin el gancho se esperaba en Port (cerrado aquí) hasta
// vencer el plazo.
func TestPortForSonda(t *testing.T) {
	d := nuevoDaemonServicio()
	srv := invitadoOK(t)
	g := gwServicio(t, d, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	e, err := g.ensure(ctx, "svc")
	if err != nil {
		t.Fatalf("ensure: %v; el invitado escucha en el puerto de PortFor", err)
	}
	want := "http://" + srv.Listener.Addr().String()
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	e.proxy.Director(r)
	if got := "http://" + r.URL.Host; got != want {
		t.Errorf("el proxy de la instancia apunta a %s; quería %s", got, want)
	}
}

// ReadyFor: un servidor que abre el puerto antes de cargar contesta 503 a la
// sonda; la instancia no se entrega hasta que contesta el código esperado.
func TestReadyForHTTP503LuegoListo(t *testing.T) {
	var sondas atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if sondas.Add(1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, srv)
	g.ReadyFor = func(string) *Probe {
		return &Probe{Path: "/healthz", Expect: http.StatusNoContent, Timeout: 5 * time.Second}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := g.ensure(ctx, "svc"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if n := sondas.Load(); n < 4 {
		t.Fatalf("la sonda se hizo %d veces; debía repetirse hasta pasar los 503", n)
	}
}

// Una sonda que nunca pasa agota SU plazo (no los 20 s de readyTimeout) y la
// restaurada se borra, como la que no llega a escuchar.
func TestReadyForPlazoAgotadoBorra(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	d := nuevoDaemonServicio()
	g := gwServicio(t, d, srv)
	g.ReadyFor = func(string) *Probe { return &Probe{Path: "/healthz", Timeout: 300 * time.Millisecond} }
	t0 := time.Now()
	if _, err := g.ensure(context.Background(), "svc"); err == nil {
		t.Fatal("ensure no falló con una sonda que contesta siempre 503")
	}
	if d := time.Since(t0); d > 3*time.Second {
		t.Errorf("tardó %v en rendirse; el plazo de la sonda era 300 ms", d)
	}
	if n := d.contar("DELETE /machines/r1"); n != 1 {
		t.Errorf("DELETE de la restaurada = %d; quería 1", n)
	}
}

// R8-08: al ADOPTAR una máquina que ya corría, la espera se hace con el
// candado del servicio tomado: con ReadyFor, el sondeo es corto (no el plazo
// entero) para no dejar en fila a todas las peticiones del modelo.
func TestReadyForAdoptSondeoCorto(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	d := nuevoDaemonServicio()
	d.poner(&api.Machine{ID: "vieja", Name: "vieja", State: api.StateRunning, IP: "127.0.0.1",
		Labels: map[string]string{api.LabelService: "svc"}, MemMiB: 128})
	g := gwServicio(t, d, srv)
	g.ReadyFor = func(string) *Probe { return &Probe{Path: "/healthz", Timeout: 30 * time.Second} }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	_, err := g.ensure(ctx, "svc")
	if err == nil {
		t.Fatal("ensure no falló con la adoptada sin pasar la sonda")
	}
	if el := time.Since(t0); el > 5*time.Second {
		t.Errorf("la adopción esperó %v con el candado del servicio; debía ser un sondeo corto", el)
	}
}

// gwTrabajo arma un planificador sin daemon con dos instancias calientes de
// svc (primaria y réplica) de maxSessions sesiones cada una. Un scale-out
// falla y se cuenta: los tests de abajo no deben necesitarlo.
func gwTrabajo(t *testing.T, maxSessions int) (*Scheduler, *entry, *entry, *atomic.Int32) {
	t.Helper()
	ahora := time.Now()
	e1 := &entry{machineID: "m1", lastUse: ahora, checkedAt: ahora, maxSessions: maxSessions}
	e2 := &entry{machineID: "m2", lastUse: ahora, checkedAt: ahora, maxSessions: maxSessions}
	var escalados atomic.Int32
	g := &Scheduler{
		idle:     time.Minute,
		services: map[string]*entry{"svc": e1},
		extra:    map[string][]*entry{"svc": {e2}},
		routes:   map[string]*sessionRoute{},
		buildFn: func(context.Context, string, *tenant, bool) (*entry, error) {
			escalados.Add(1)
			return nil, fmt.Errorf("no scale-out in this test")
		},
	}
	return g, e1, e2, &escalados
}

// R8-07: entre elegir la réplica y que la petición la marque en vuelo (Begin)
// hay un hueco. Con max_inflight 1, dos peticiones que llegan a la vez leían
// las dos inflight 0 y elegían la misma réplica: la segunda esperaba en cola
// teniendo otra libre. Las reservas cuentan como trabajo al elegir.
func TestDosPeticionesSimultaneasNoMismaReplica(t *testing.T) {
	g, _, _, escalados := gwTrabajo(t, 32)
	g.MaxInflightFor = func(string) int { return 1 }
	ctx := context.Background()
	a, err := g.pickInstance(ctx, "svc", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.pickInstance(ctx, "svc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("las dos peticiones fueron a %s con max_inflight 1 y otra réplica libre", a.machineID)
	}
	if n := escalados.Load(); n != 0 {
		t.Errorf("hubo %d scale-out con una réplica libre", n)
	}
}

// R8-07: con MaxInflightFor el tope de sesiones (la fórmula de memoria del
// puente MCP, 1 en una réplica de 64 MiB) no limita: el producto no usa
// sesiones y lo que cuenta es el trabajo. Y una petición que pasa de reserva a
// en vuelo (Begin) no cuenta dos veces.
func TestMaxInflightForSinTopeDeSesiones(t *testing.T) {
	g, e1, e2, escalados := gwTrabajo(t, 1)
	g.MaxInflightFor = func(string) int { return 2 }
	ctx := context.Background()
	var elegidas []*entry
	for i := 0; i < 4; i++ {
		e, err := g.pickInstance(ctx, "svc", nil)
		if err != nil {
			t.Fatalf("petición %d: %v", i, err)
		}
		g.begin(e)
		elegidas = append(elegidas, e)
	}
	if n := escalados.Load(); n != 0 {
		t.Fatalf("hubo %d scale-out con hueco de trabajo libre (tope de sesiones aplicado)", n)
	}
	if elegidas[0] != e1 || elegidas[1] != e1 || elegidas[2] != e2 || elegidas[3] != e2 {
		t.Errorf("reparto = %s %s %s %s; quería m1 m1 m2 m2", elegidas[0].machineID,
			elegidas[1].machineID, elegidas[2].machineID, elegidas[3].machineID)
	}
	for _, e := range elegidas {
		g.end(e)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if e1.inflight != 0 || e2.inflight != 0 || e1.reservasLocked() != 0 || e2.reservasLocked() != 0 {
		t.Errorf("tras End quedan inflight %d/%d reservas %d/%d", e1.inflight, e2.inflight,
			e1.reservasLocked(), e2.reservasLocked())
	}
}

// olvidosVistos apunta las llamadas a OnForget y si llegaron con g.mu tomado.
type olvidosVistos struct {
	mu         sync.Mutex
	vistos     []string
	conCerrojo int
}

func (o *olvidosVistos) gancho(g *Scheduler) func(service, addr string) {
	return func(service, addr string) {
		if g.mu.TryLock() {
			g.mu.Unlock()
		} else {
			o.mu.Lock()
			o.conCerrojo++
			o.mu.Unlock()
		}
		o.mu.Lock()
		o.vistos = append(o.vistos, service+" "+addr)
		o.mu.Unlock()
	}
}

func (o *olvidosVistos) lista() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.vistos...)
}

// R8-09: una instancia cuyo TTL ya no se puede renovar (el daemon la congeló o
// la borró por debajo) salía del mapa sin avisar a nadie: el gateway de IA
// guardaba para siempre el transporte hacia su dirección. OnForget se llama
// una vez, con g.mu suelto.
func TestOnForgetEnTTLVencido(t *testing.T) {
	d := &daemonFalso{maquinas: map[string]*api.Machine{}, fallarRenew: map[string][]int{}}
	g := conDaemonFalso(t, d)
	o := &olvidosVistos{}
	g.OnForget = o.gancho(g)
	g.mu.Lock()
	g.services["svc"] = &entry{machineID: "m1", ip: "10.0.0.9", port: 8000,
		lastUse: time.Now(), renewedAt: time.Now().Add(-time.Hour)}
	g.mu.Unlock()
	g.reapOnce(context.Background())
	if got := o.lista(); len(got) != 1 || got[0] != "svc 10.0.0.9:8000" {
		t.Fatalf("OnForget = %v; quería una vez svc 10.0.0.9:8000", got)
	}
	if o.conCerrojo != 0 {
		t.Errorf("OnForget se llamó %d veces con g.mu tomado", o.conCerrojo)
	}
}

// R8-09: toda retirada avisa, no solo dormir: la instancia que el proxy da por
// desaparecida (DropInstance) y la que el segador congela.
func TestOnForgetEnDropYSegador(t *testing.T) {
	g, _, _ := gwPausas(0)
	o := &olvidosVistos{}
	g.OnForget = o.gancho(g)
	g.services["a"] = &entry{machineID: "m-a", ip: "10.0.0.1", lastUse: time.Now()}
	g.services["b"] = &entry{machineID: "m-b", ip: "10.0.0.2", lastUse: time.Now().Add(-time.Hour)}
	g.Port = 9000
	g.DropInstance("a", "m-a")
	g.reapOnce(context.Background())
	got := o.lista()
	if len(got) != 2 || got[0] != "a 10.0.0.1:9000" || got[1] != "b 10.0.0.2:9000" {
		t.Fatalf("OnForget = %v; quería a (drop) y b (segador)", got)
	}
	if o.conCerrojo != 0 {
		t.Errorf("OnForget se llamó %d veces con g.mu tomado", o.conCerrojo)
	}
}

// Sin MaxInflightFor (el gateway MCP) se reparte como siempre: por sesiones y
// MaxInflight global, sin contar reservas como trabajo.
func TestMaxInflightForNilComoSiempre(t *testing.T) {
	g, e1, _, _ := gwTrabajo(t, 32)
	g.MaxInflight = 1
	ctx := context.Background()
	a, _ := g.pickInstance(ctx, "svc", nil)
	b, _ := g.pickInstance(ctx, "svc", nil)
	if a != e1 || b != e1 {
		t.Errorf("sin gancho las dos reservas iban a la primaria (inflight 0); fueron a %s y %s", a.machineID, b.machineID)
	}
}

// Una instancia del fondo de un servicio con PortFor se prepara en el puerto
// de ese servicio, no en el del planificador.
func TestFondoUsaPortFor(t *testing.T) {
	g, _ := conDaemonAislado(t)
	g.PortFor = func(string) int { return 9000 }
	var addr string
	g.PrepareAddr = func(_ context.Context, a string) (string, error) { addr = a; return "sid", nil }
	if _, err := g.pool.warm(context.Background(), "svc", "svc"); err != nil {
		t.Fatal(err)
	}
	if addr != "172.16.0.2:9000" {
		t.Fatalf("PrepareAddr(%q), quería el puerto 9000 del servicio", addr)
	}
}

// Status da la dirección en el puerto de la instancia (el de PortFor al
// crearla), no en el del planificador.
func TestStatusUsaPuertoDeLaInstancia(t *testing.T) {
	g, _ := conDaemonAislado(t)
	g.mu.Lock()
	g.services["svc"] = &entry{machineID: "m1", ip: "10.0.0.9", port: 9000, lastUse: time.Now()}
	g.mu.Unlock()
	if st := g.Status("svc"); st.Addr != "10.0.0.9:9000" {
		t.Fatalf("Status.Addr = %q, quería 10.0.0.9:9000", st.Addr)
	}
}
