package scheduler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// daemonFalso imita lo justo del daemon para ver qué le hace el planificador al
// TTL de sus instancias: listar, despertar, congelar, renovar y el vigilante que
// congela lo que venció. Thaw NO toca el reloj del TTL, igual que el de verdad
// (ver TestElTTLNoSeReiniciaAlDespertar en internal/machine).
type daemonFalso struct {
	mu       sync.Mutex
	maquinas map[string]*api.Machine
	llamadas []string
	// sinRenew hace de daemon anterior a la capacidad "renew".
	sinRenew bool
}

func (d *daemonFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.llamadas = append(d.llamadas, r.Method+" "+r.URL.Path)
	responder := func(v any) { _ = json.NewEncoder(w).Encode(v) }

	if r.Method == http.MethodGet && r.URL.Path == "/info" {
		caps := []string{"exec", "sandboxes"}
		if !d.sinRenew {
			caps = append(caps, "renew")
		}
		responder(api.Info{Version: "test", Capabilities: caps})
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/machines" {
		l := []*api.Machine{}
		for _, mc := range d.maquinas {
			c := *mc
			l = append(l, &c)
		}
		responder(l)
		return
	}
	partes := strings.Split(strings.TrimPrefix(r.URL.Path, "/machines/"), "/")
	if len(partes) != 2 || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	mc := d.maquinas[partes[0]]
	if mc == nil {
		http.NotFound(w, r)
		return
	}
	ahora := time.Now()
	switch partes[1] {
	case "thaw":
		mc.State = api.StateRunning
		mc.StartedAt = &ahora
	case "freeze":
		mc.State = api.StateWarm
	case "renew":
		if d.sinRenew {
			http.NotFound(w, r)
			return
		}
		var req api.RenewRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.TTLSeconds > 0 {
			mc.TTLSeconds = req.TTLSeconds
		}
		mc.TTLAt = &ahora
	default:
		http.NotFound(w, r)
		return
	}
	c := *mc
	responder(&c)
}

// vigilar es expireTTL del daemon: congela las que llevan corriendo más de su
// TTL según su reloj (TTLAt).
func (d *daemonFalso) vigilar() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, mc := range d.maquinas {
		if mc.State != api.StateRunning || mc.TTLSeconds <= 0 || mc.TTLAt == nil {
			continue
		}
		if time.Since(*mc.TTLAt) >= time.Duration(mc.TTLSeconds)*time.Second {
			mc.State = api.StateWarm
		}
	}
}

func (d *daemonFalso) estado(id string) api.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.maquinas[id].State
}

func (d *daemonFalso) visto(llamada string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, l := range d.llamadas {
		if l == llamada {
			n++
		}
	}
	return n
}

// conDaemonFalso levanta el daemon falso en un socket unix y un planificador
// con idle de un minuto (TTL de sus instancias: dos).
func conDaemonFalso(t *testing.T, d *daemonFalso) *Scheduler {
	t.Helper()
	// /tmp y no t.TempDir(): la ruta de un socket unix no puede pasar de ~104
	// bytes, y la de TempDir en macOS se acerca.
	dir, err := os.MkdirTemp("/tmp", "sch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: d}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return New(api.NewClient("unix://"+sock), time.Minute, false, 0)
}

// instanciaVieja es una instancia del gateway creada hace mucho más de su TTL
// (2×idle) y congelada por el segador: su reloj del TTL ya venció.
func instanciaVieja(id string) *api.Machine {
	hace1h := time.Now().Add(-time.Hour)
	return &api.Machine{
		ID: id, Name: id, State: api.StateWarm, IP: "10.0.0.2",
		Labels:     map[string]string{api.LabelService: "svc"},
		TTLSeconds: 120, TTLAt: &hace1h, StartedAt: &hace1h,
	}
}

// El fallo: el planificador despertaba una instancia creada hace más de 2×idle,
// y como despertar no reinicia el reloj del TTL (a propósito, por los
// sandboxes), el vigilante del daemon la volvía a congelar en la siguiente
// vuelta, con la petición que la había despertado todavía en curso.
func TestDespertarUnaInstanciaViejaRenuevaSuTTL(t *testing.T) {
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": instanciaVieja("m1")}}
	g := conDaemonFalso(t, d)

	mc, err := g.acquire(context.Background(), "svc", false)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if mc.ID != "m1" || mc.State != api.StateRunning {
		t.Fatalf("acquire = %s %s; quería m1 despierta", mc.ID, mc.State)
	}

	d.vigilar()
	if s := d.estado("m1"); s != api.StateRunning {
		t.Fatalf("el vigilante del daemon congeló la instancia recién despertada (%s): su TTL no se renovó", s)
	}

	// Y se renueva ANTES de despertarla: si fuera después, una vuelta del
	// vigilante entre las dos llamadas la congelaría igual.
	d.mu.Lock()
	var orden []string
	for _, l := range d.llamadas {
		if strings.HasPrefix(l, "POST /machines/m1/") {
			orden = append(orden, strings.TrimPrefix(l, "POST /machines/m1/"))
		}
	}
	d.mu.Unlock()
	if strings.Join(orden, ",") != "renew,thaw" {
		t.Fatalf("llamadas sobre m1 = %v; quería renew y luego thaw", orden)
	}
}

// Una que ya estaba corriendo y se adopta también puede traer el reloj vencido.
func TestAdoptarUnaInstanciaEnMarchaRenuevaSuTTL(t *testing.T) {
	vieja := instanciaVieja("m1")
	vieja.State = api.StateRunning
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": vieja}}
	g := conDaemonFalso(t, d)

	if _, err := g.acquire(context.Background(), "svc", false); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	d.vigilar()
	if s := d.estado("m1"); s != api.StateRunning {
		t.Fatalf("la instancia adoptada se congeló (%s): su TTL no se renovó", s)
	}
}

// El tráfico HTTP no toca el TTL en el daemon: una instancia que atiende sin
// parar durante más de 2×idle se congelaría debajo de sus peticiones. El
// segador renueva el TTL de las que siguen despiertas, como un latido: si el
// gateway muere, el latido para y la red de seguridad vuelve a funcionar.
func TestElSegadorRenuevaElTTLDeLasDespiertas(t *testing.T) {
	ocupada := instanciaVieja("m1")
	ocupada.State = api.StateRunning
	ociosa := instanciaVieja("m2")
	ociosa.State = api.StateRunning
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": ocupada, "m2": ociosa}}
	g := conDaemonFalso(t, d)

	hace := time.Now().Add(-time.Hour)
	g.mu.Lock()
	// m1 lleva una hora atendiendo peticiones (en vuelo ahora mismo).
	g.services["svc"] = &entry{machineID: "m1", lastUse: time.Now(), inflight: 1, renewedAt: hace}
	// m2 es una réplica ociosa: la congela el segador, no hace falta renovarla.
	g.extra["svc"] = []*entry{{machineID: "m2", lastUse: hace, renewedAt: hace}}
	g.mu.Unlock()

	g.reapOnce(context.Background())
	d.vigilar()

	if s := d.estado("m1"); s != api.StateRunning {
		t.Fatalf("la instancia ocupada se congeló (%s): el segador no renovó su TTL", s)
	}
	if n := d.visto("POST /machines/m2/renew"); n != 0 {
		t.Errorf("renovó %d veces la réplica ociosa que iba a congelar", n)
	}
	if s := d.estado("m2"); s != api.StateWarm {
		t.Errorf("la réplica ociosa está %s; el segador debía congelarla", s)
	}

	// Recién renovada, la siguiente vuelta no vuelve a llamar al daemon.
	g.reapOnce(context.Background())
	if n := d.visto("POST /machines/m1/renew"); n != 1 {
		t.Errorf("renovaciones de m1 = %d, want 1: renovar en cada vuelta sobra", n)
	}
}

// Contra un daemon sin la capacidad, el planificador sigue funcionando como
// antes: despierta sin renovar y no insiste.
func TestSinCapacidadRenewSeDespiertaIgual(t *testing.T) {
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": instanciaVieja("m1")}, sinRenew: true}
	g := conDaemonFalso(t, d)

	mc, err := g.acquire(context.Background(), "svc", false)
	if err != nil || mc.State != api.StateRunning {
		t.Fatalf("acquire = %v, %v; quería despertarla aunque el daemon no sepa renovar", mc, err)
	}
	if n := d.visto("POST /machines/m1/renew"); n != 0 {
		t.Errorf("pidió %d renovaciones a un daemon que no las anuncia", n)
	}
	g.acquire(context.Background(), "svc", false)
	if n := d.visto("GET /info"); n != 1 {
		t.Errorf("preguntó %d veces por las capacidades; basta una", n)
	}
}
