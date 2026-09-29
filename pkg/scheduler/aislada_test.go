package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// daemonAislado imita lo que el planificador le pide al daemon con sesiones
// aisladas: crear del dorado, listar, consultar, congelar, descongelar y
// borrar. Las máquinas se alcanzan por un reenvío (Forwards), así que esperar
// a que escuchen es una pregunta al daemon y no un dial de verdad.
type daemonAislado struct {
	mu       sync.Mutex
	n        int
	maquinas map[string]*api.Machine
	borradas []string
}

func (d *daemonAislado) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	responder := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/info":
		responder(api.Info{Version: "test"})
		return
	case r.URL.Path == "/snapshots":
		responder([]*api.Snapshot{{Name: "svc", Labels: map[string]string{api.LabelService: "svc"}}})
		return
	case r.URL.Path == "/machines" && r.Method == http.MethodGet:
		l := []*api.Machine{}
		for _, mc := range d.maquinas {
			c := *mc
			l = append(l, &c)
		}
		responder(l)
		return
	case r.URL.Path == "/machines" && r.Method == http.MethodPost:
		var req api.RunRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.n++
		mc := &api.Machine{
			ID: fmt.Sprintf("m%d", d.n), Name: fmt.Sprintf("svc-%d", d.n), From: req.From,
			State: api.StateRunning, IP: "172.16.0.2", Labels: req.Labels, CreatedAt: time.Now(),
			Forwards: map[string]string{fmt.Sprint(GuestPort): "127.0.0.1:1"},
		}
		d.maquinas[mc.ID] = mc
		c := *mc
		responder(&c)
		return
	}
	partes := strings.Split(strings.TrimPrefix(r.URL.Path, "/machines/"), "/")
	mc := d.maquinas[partes[0]]
	if mc == nil {
		http.Error(w, `{"error":"machine does not exist"}`, http.StatusNotFound)
		return
	}
	switch {
	case len(partes) == 1 && r.Method == http.MethodGet:
	case len(partes) == 1 && r.Method == http.MethodDelete:
		delete(d.maquinas, mc.ID)
		d.borradas = append(d.borradas, mc.ID)
		w.WriteHeader(http.StatusNoContent)
		return
	case len(partes) == 2 && partes[1] == "guest":
		responder(api.GuestResponse{Status: 200})
		return
	case len(partes) == 2 && partes[1] == "thaw":
		mc.State = api.StateRunning
	case len(partes) == 2 && partes[1] == "freeze":
		mc.State = api.StateWarm
	default:
		http.NotFound(w, r)
		return
	}
	c := *mc
	responder(&c)
}

func (d *daemonAislado) estado(id string) api.State {
	d.mu.Lock()
	defer d.mu.Unlock()
	if mc := d.maquinas[id]; mc != nil {
		return mc.State
	}
	return ""
}

func (d *daemonAislado) existe(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.maquinas[id] != nil
}

func conDaemonAislado(t *testing.T) (*Scheduler, *daemonAislado) {
	t.Helper()
	d := &daemonAislado{maquinas: map[string]*api.Machine{}}
	dir, err := os.MkdirTemp("/tmp", "sch-ais")
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
	return New(api.NewClient("unix://"+sock), time.Minute, false, 0), d
}

var sinTenant = &tenant{name: defaultTenant}

// Lo que el modo existe para garantizar: dos sesiones, dos máquinas, y ninguna
// de las dos puede recibir una sesión ajena.
func TestDosSesionesAisladasTienenMaquinasDistintas(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()

	a, err := g.isolatedSession(ctx, "svc", "A", sinTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	b, err := g.isolatedSession(ctx, "svc", "B", sinTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	if a.machineID == b.machineID {
		t.Fatalf("las dos sesiones comparten la máquina %s", a.machineID)
	}
	for _, e := range []*entry{a, b} {
		if d.maquinas[e.machineID].Labels[LabelIsolated] != "true" {
			t.Errorf("la máquina %s no lleva la marca de aislada", e.machineID)
		}
	}
	// La misma clave vuelve a SU máquina, sin crear otra.
	if a2, err := g.isolatedSession(ctx, "svc", "A", sinTenant, true); err != nil || a2 != a {
		t.Fatalf("la sesión A volvió a %v (%v), quería su instancia", a2, err)
	}
	// Ninguna aislada es candidata a una sesión nueva normal.
	g.mu.Lock()
	n := len(g.entriesLocked("svc"))
	g.mu.Unlock()
	if n != 0 {
		t.Fatalf("entriesLocked ofrece %d instancia(s) aislada(s) a sesiones ajenas", n)
	}
	// Una clave de otro servicio no se cruza.
	if _, err := g.isolatedSession(ctx, "otro", "A", sinTenant, true); !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("clave de svc usada en otro servicio: %v", err)
	}
}

// La primaria de un servicio no puede adoptar ni descongelar la máquina de una
// sesión aislada: le entregaría el disco y la memoria de esa sesión.
func TestLaPrimariaNoAdoptaUnaMaquinaAislada(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	a, err := g.isolatedSession(ctx, "svc", "A", sinTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	// Como la deja el segador: fuera de las instancias despiertas, viva en el
	// daemon. Registrada, acquire ya la saltaba por otro motivo.
	g.olvidarInstancia("svc", a.machineID)
	for _, estado := range []api.State{api.StateRunning, api.StateWarm} {
		d.mu.Lock()
		d.maquinas[a.machineID].State = estado
		d.mu.Unlock()
		mc, how, err := g.acquire(ctx, "svc", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if mc.ID == a.machineID {
			t.Fatalf("con la aislada %s, acquire la tomó (%s)", estado, how)
		}
		if how != "restore" {
			t.Fatalf("acquire = %s, quería una restauración nueva", how)
		}
		g.mu.Lock()
		delete(g.adquiriendo, mc.ID)
		g.mu.Unlock()
		// La nueva es una primaria normal: fuera, para la siguiente vuelta.
		d.mu.Lock()
		delete(d.maquinas, mc.ID)
		d.mu.Unlock()
	}
}

// Congelada por inactividad, la sesión vuelve a SU máquina: el estado del
// proceso y del disco sigue ahí. La ruta tampoco caduca con idle.
func TestSesionAisladaSobreviveAlCongelado(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	a, err := g.isolatedSession(ctx, "svc", "A", sinTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	g.Bind("A", "svc", a)
	id := a.machineID

	// Más vieja que idle: el segador la congela.
	g.mu.Lock()
	a.lastUse = time.Now().Add(-2 * time.Minute)
	g.routes["A"].lastUse = a.lastUse
	g.mu.Unlock()
	g.reapOnce(ctx)
	if s := d.estado(id); s != api.StateWarm {
		t.Fatalf("tras el segador la máquina está %q, quería congelada", s)
	}
	if g.Instance("svc", id) != nil {
		t.Fatal("la instancia congelada sigue registrada como despierta")
	}
	if g.Route("A") == nil {
		t.Fatal("la ruta de la sesión aislada caducó con idle; tiene que durar SessionTTL")
	}

	// Sin create: despertar la suya, nunca crear otra.
	e, err := g.isolatedSession(ctx, "svc", "A", sinTenant, false)
	if err != nil {
		t.Fatal(err)
	}
	if e.machineID != id {
		t.Fatalf("la sesión despertó en %s, quería su máquina %s", e.machineID, id)
	}
	if s := d.estado(id); s != api.StateRunning {
		t.Fatalf("la máquina está %q tras despertarla", s)
	}
	if _, err := g.isolatedSession(ctx, "svc", "nadie", sinTenant, false); !errors.Is(err, ErrNoSuchSession) {
		t.Fatalf("sin create, una clave desconocida dio %v", err)
	}
}

// Cerrar la sesión destruye su máquina, que es lo que libera su overlay en el
// host; caducar por SessionTTL, lo mismo.
func TestCerrarOCaducarDestruyeLaMaquina(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	a, _ := g.isolatedSession(ctx, "svc", "A", sinTenant, true)
	b, _ := g.isolatedSession(ctx, "svc", "B", sinTenant, true)
	g.Bind("A", "svc", a)

	if !g.releaseIsolated(ctx, "A") {
		t.Fatal("releaseIsolated no encontró la sesión A")
	}
	if d.existe(a.machineID) || g.Route("A") != nil || g.IsIsolated("A") {
		t.Fatal("tras cerrar A quedan su máquina, su ruta o su registro")
	}
	if g.releaseIsolated(ctx, "A") {
		t.Fatal("cerrar dos veces no debe encontrar nada")
	}

	// B, congelada y abandonada más de SessionTTL.
	g.SessionTTL = time.Hour
	g.mu.Lock()
	b.lastUse = time.Now().Add(-2 * time.Minute)
	g.mu.Unlock()
	g.reapOnce(ctx)
	if !d.existe(b.machineID) || !g.IsIsolated("B") {
		t.Fatal("B se destruyó antes de su SessionTTL")
	}
	g.mu.Lock()
	g.aisladas["B"].lastUse = time.Now().Add(-2 * time.Hour)
	g.mu.Unlock()
	g.reapOnce(ctx)
	if d.existe(b.machineID) || g.IsIsolated("B") {
		t.Fatal("B pasó su SessionTTL y su máquina sigue")
	}
}

// Con trabajo en vuelo una sesión no caduca, por vieja que sea su última
// llegada.
func TestSesionAisladaConTrabajoNoCaduca(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	g.SessionTTL = time.Minute
	a, _ := g.isolatedSession(ctx, "svc", "A", sinTenant, true)
	g.begin(a)
	g.mu.Lock()
	g.aisladas["A"].lastUse = time.Now().Add(-time.Hour)
	g.mu.Unlock()
	g.reapOnce(ctx)
	if !d.existe(a.machineID) {
		t.Fatal("se destruyó una sesión con una llamada en curso")
	}
}

// En el tope se recicla la sesión más ociosa si ya pasó la gracia; si todas
// están vivas, ErrMaxReplicas y nada se destruye.
func TestTopeDeSesionesAisladas(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	g.MaxReplicas = 1
	a, err := g.isolatedSession(ctx, "svc", "A", sinTenant, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.isolatedSession(ctx, "svc", "B", sinTenant, true); !errors.Is(err, ErrMaxReplicas) {
		t.Fatalf("en el tope con A activa: %v, quería ErrMaxReplicas", err)
	}
	if !d.existe(a.machineID) {
		t.Fatal("rechazar B se llevó por delante a A")
	}
	g.mu.Lock()
	g.aisladas["A"].lastUse = time.Now().Add(-time.Minute)
	g.mu.Unlock()
	b, err := g.isolatedSession(ctx, "svc", "B", sinTenant, true)
	if err != nil {
		t.Fatalf("con A ociosa, B tenía que entrar reciclándola: %v", err)
	}
	if d.existe(a.machineID) || g.IsIsolated("A") {
		t.Fatal("A no se recicló")
	}
	if b.machineID == a.machineID {
		t.Fatal("B heredó la máquina de A")
	}
}

// Si la máquina de la sesión desapareció, la sesión se da por perdida: nada de
// darle otra máquina en silencio.
func TestSesionAisladaSinMaquinaSePierde(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	a, _ := g.isolatedSession(ctx, "svc", "A", sinTenant, true)
	g.olvidarInstancia("svc", a.machineID)
	d.mu.Lock()
	delete(d.maquinas, a.machineID)
	d.mu.Unlock()
	if _, err := g.isolatedSession(ctx, "svc", "A", sinTenant, false); !errors.Is(err, ErrSessionLost) {
		t.Fatalf("despertar una sesión sin máquina: %v, quería ErrSessionLost", err)
	}
	if g.IsIsolated("A") {
		t.Fatal("la sesión perdida sigue registrada")
	}
}

// El barrido recoge las aisladas que no son de ninguna sesión (un gateway
// anterior), pero no las recién nacidas ni las de otro producto.
func TestBarridoDeAisladasHuerfanas(t *testing.T) {
	g, d := conDaemonAislado(t)
	ctx := context.Background()
	g.MachineLabels = map[string]string{"owner": "mcp"}
	viva, _ := g.isolatedSession(ctx, "svc", "A", sinTenant, true)
	vieja := time.Now().Add(-time.Hour)
	d.mu.Lock()
	d.maquinas["huerfana"] = &api.Machine{ID: "huerfana", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{LabelIsolated: "true", "owner": "mcp", api.LabelService: "svc"}}
	d.maquinas["recien"] = &api.Machine{ID: "recien", State: api.StateRunning, CreatedAt: time.Now(),
		Labels: map[string]string{LabelIsolated: "true", "owner": "mcp"}}
	d.maquinas["ajena"] = &api.Machine{ID: "ajena", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{LabelIsolated: "true", "owner": "otro"}}
	d.maquinas["normal"] = &api.Machine{ID: "normal", State: api.StateWarm, CreatedAt: vieja,
		Labels: map[string]string{"owner": "mcp"}}
	d.mu.Unlock()
	g.mu.Lock()
	if !g.tocaBarrerLocked() {
		g.mu.Unlock()
		t.Fatal("la primera vuelta tiene que barrer")
	}
	if g.tocaBarrerLocked() {
		g.mu.Unlock()
		t.Fatal("barrió dos veces seguidas")
	}
	g.mu.Unlock()
	g.barrerAisladas(ctx)
	if d.existe("huerfana") {
		t.Error("la huérfana sigue")
	}
	for _, id := range []string{viva.machineID, "recien", "ajena", "normal"} {
		if !d.existe(id) {
			t.Errorf("el barrido se llevó %s", id)
		}
	}
}

// Una aislada congelada para hacer sitio no vuelve nunca de primaria.
func TestReponerAisladaNoLaHacePrimaria(t *testing.T) {
	g := &Scheduler{services: map[string]*entry{}, extra: map[string][]*entry{}}
	e := &entry{machineID: "m1", aislada: "A"}
	g.reponerEntryLocked("svc", e)
	if g.services["svc"] != nil {
		t.Fatal("una aislada acabó de primaria")
	}
	if len(g.extra["svc"]) != 1 {
		t.Fatal("la aislada no se repuso en extra")
	}
}
