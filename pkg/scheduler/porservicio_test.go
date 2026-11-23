package scheduler

import (
	"context"
	"encoding/json"
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
		d.n++
		id := "r" + strconv.Itoa(d.n)
		mc := &api.Machine{ID: id, Name: id, State: api.StateRunning, IP: "127.0.0.1",
			From: req.From, Labels: req.Labels, MemMiB: 128}
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
