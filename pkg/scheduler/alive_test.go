package scheduler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// daemonAlive imita lo justo del daemon para distinguir GET /machines (List,
// recorre el disco de TODAS las máquinas) de GET /machines/<id> (Get, una
// sola). Registra si List() se llegó a llamar.
type daemonAlive struct {
	maquinas    map[string]*api.Machine
	listLlamado atomic.Bool
}

func (d *daemonAlive) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if r.URL.Path == "/machines" {
		d.listLlamado.Store(true)
		l := []*api.Machine{}
		for _, mc := range d.maquinas {
			l = append(l, mc)
		}
		_ = json.NewEncoder(w).Encode(l)
		return
	}
	if mc := d.maquinas[filepath.Base(r.URL.Path)]; mc != nil {
		_ = json.NewEncoder(w).Encode(mc)
		return
	}
	http.NotFound(w, r)
}

// gwConDaemonAlive levanta daemonAlive en un socket unix y un planificador que
// habla con él.
func gwConDaemonAlive(t *testing.T, d *daemonAlive) *Scheduler {
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

// G-02: alive debe usar Get(id), no List(): List recorre el disco de TODAS
// las máquinas del daemon y ensure() llamaba a alive en cada thaw de una
// instancia con la caché de vida caducada, bajo el candado del servicio. Con
// 200+ máquinas era el tráfico dominante del socket del daemon.
func TestAliveUsaGetNoList(t *testing.T) {
	d := &daemonAlive{maquinas: map[string]*api.Machine{
		"m1": {ID: "m1", State: api.StateRunning},
	}}
	g := gwConDaemonAlive(t, d)

	if !g.alive(context.Background(), "m1") {
		t.Fatal("alive(m1) = false; la máquina está running")
	}
	if d.listLlamado.Load() {
		t.Error("alive llamó a List(); debe usar Get(id)")
	}
}

// Una máquina que ya no está running (o que Get no encuentra) no está viva.
func TestAliveFalseSiNoEstaRunning(t *testing.T) {
	d := &daemonAlive{maquinas: map[string]*api.Machine{
		"m1": {ID: "m1", State: api.StateWarm},
	}}
	g := gwConDaemonAlive(t, d)

	if g.alive(context.Background(), "m1") {
		t.Error("alive(m1) = true; la máquina está congelada, no corriendo")
	}
	if g.alive(context.Background(), "no-existe") {
		t.Error("alive(no-existe) = true; Get debía fallar con 404")
	}
	if d.listLlamado.Load() {
		t.Error("alive llamó a List() en algún caso; debe usar Get(id) siempre")
	}
}

// gwConManejador levanta h en un socket unix y un planificador que habla con él.
func gwConManejador(t *testing.T, h http.Handler) *Scheduler {
	t.Helper()
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
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return New(api.NewClient("unix://"+sock), time.Minute, false, 0)
}

// B-01: alive daba por muerta la instancia ante CUALQUIER error de Get, también
// un fallo pasajero del daemon (reiniciándose, ocupado). ensure la quitaba del
// mapa y la readoptaba con una entrada nueva a inflight 0: el segador veía
// ociosa una instancia con trabajo en curso. Un error que no dice «no existe»
// no es una muerte: la entrada se queda como estaba.
func TestEnsureNoDescartaLaInstanciaPorUnFalloDelDaemon(t *testing.T) {
	g := gwConManejador(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"restarting"}`, http.StatusServiceUnavailable)
	}))
	e := &entry{machineID: "m1", ip: "10.0.0.9", lastUse: time.Now(), inflight: 1}
	g.services["svc"] = e

	got, err := g.ensure(context.Background(), "svc")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if got != e {
		t.Fatal("ensure no devolvió la entrada que ya había")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.services["svc"] != e {
		t.Fatal("la entrada salió del mapa por un fallo pasajero del daemon")
	}
	if e.inflight != 1 {
		t.Errorf("inflight = %d; quería 1", e.inflight)
	}
}

// B-01: lo mismo con el contexto del cliente cancelado: que el cliente se vaya
// no dice nada de la máquina.
func TestEnsureNoDescartaLaInstanciaConElContextoCancelado(t *testing.T) {
	d := &daemonAlive{maquinas: map[string]*api.Machine{
		"m1": {ID: "m1", State: api.StateRunning},
	}}
	g := gwConDaemonAlive(t, d)
	e := &entry{machineID: "m1", ip: "10.0.0.9", lastUse: time.Now(), inflight: 1}
	g.services["svc"] = e

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = g.ensure(ctx, "svc")

	g.mu.Lock()
	defer g.mu.Unlock()
	if g.services["svc"] != e {
		t.Fatal("la entrada salió del mapa porque el cliente canceló")
	}
}

// B-01: si la primaria no estaba running (el daemon la congeló) y ensure
// readopta la MISMA máquina, las peticiones que seguían en vuelo sobre la
// entrada vieja deben contar en la nueva; si no, el segador la ve ociosa y la
// congela debajo de ellas. Y su End debe descontar donde contaron.
func TestEnsureConservaElTrabajoEnVueloSiVuelveLaMismaMaquina(t *testing.T) {
	puerto := agenteFalso(t)
	mc := instanciaVieja("m1")
	mc.IP = "127.0.0.1"
	d := &daemonFalso{maquinas: map[string]*api.Machine{"m1": mc}}
	g := conDaemonFalso(t, d)
	g.Port = puerto
	vieja := &entry{machineID: "m1", ip: mc.IP, lastUse: time.Now(), renewedAt: time.Now(), inflight: 1}
	g.services["svc"] = vieja

	e, err := g.ensure(context.Background(), "svc")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	g.mu.Lock()
	n := e.inflight
	g.mu.Unlock()
	if n != 1 {
		t.Fatalf("inflight de la entrada readoptada = %d; quería 1 (la petición en vuelo)", n)
	}
	g.end(vieja)
	g.mu.Lock()
	n = g.services["svc"].inflight
	g.mu.Unlock()
	if n != 0 {
		t.Errorf("tras el End de la petición vieja, inflight = %d; quería 0", n)
	}
}
