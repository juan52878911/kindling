package pool

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/ext/sandbox/internal/plantilla"
	"github.com/juan52878911/kindling/pkg/api"
)

// El fondo lee lo que quiere cada plantilla de la anotación del snapshot, que es
// donde la dejó quien la construyó: así no hay una segunda lista de plantillas
// que mantener en sincronía.
func TestDeseoDe(t *testing.T) {
	receta, _ := json.Marshal(map[string]any{
		"template": plantilla.Plantilla{Nombre: "ts", Imagen: "toolchain", Pool: 3},
	})

	casos := []struct {
		nombre string
		snap   *api.Snapshot
		quiere int
		cual   string
	}{
		{"con receta y pool", &api.Snapshot{Name: "sbx-ts",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}, 3, "ts"},
		{"snapshot ajeno", &api.Snapshot{Name: "notas",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}, 0, ""},
		{"sin anotación", &api.Snapshot{Name: "sbx-otra"}, 0, ""},
		{"anotación ilegible", &api.Snapshot{Name: "sbx-rota",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: json.RawMessage("{")}}, 0, ""},
	}
	for _, c := range casos {
		cual, quiere := DeseoDe(c.snap)
		if quiere != c.quiere || (c.quiere > 0 && cual != c.cual) {
			t.Errorf("%s: (%q, %d), quería (%q, %d)", c.nombre, cual, quiere, c.cual, c.quiere)
		}
	}
}

// Una plantilla sin pool no fabrica nada: el fondo es opt-in, porque cuesta RAM
// en reposo.
func TestSinPoolNoSeFabricaNada(t *testing.T) {
	receta, _ := json.Marshal(map[string]any{
		"template": plantilla.Plantilla{Nombre: "sin", Imagen: "toolchain"},
	})
	if _, quiere := DeseoDe(&api.Snapshot{Name: "sbx-sin",
		Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}); quiere != 0 {
		t.Fatalf("quiere %d, want 0", quiere)
	}
}

// daemonPool es un daemon mínimo para el fondo: un snapshot de plantilla con
// pool, los sandboxes que se le den, y cuenta lo que se crea y se borra. Lo
// demás (grafos) contesta 404.
type daemonPool struct {
	mu       sync.Mutex
	vivas    []*api.Machine
	creadas  int
	borradas []string
	// snaps, si no es nil, sustituye al snapshot sbx-ts por defecto; falla
	// hace fallar la creación desde esos snapshots; pedidas guarda cada
	// creación aceptada.
	snaps   []*api.Snapshot
	falla   map[string]bool
	pedidas []api.SandboxRequest
}

func nuevoDaemonPool(t *testing.T, pool int, vivas ...*api.Machine) (*daemonPool, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pool")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	d := &daemonPool{vivas: vivas}
	receta, _ := json.Marshal(map[string]any{"template": plantilla.Plantilla{Nombre: "ts", Imagen: "toolchain", Pool: pool}})
	m := http.NewServeMux()
	m.HandleFunc("GET /snapshots", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.snaps != nil {
			json.NewEncoder(w).Encode(d.snaps)
			return
		}
		json.NewEncoder(w).Encode([]*api.Snapshot{{Name: "sbx-ts",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}})
	})
	m.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		json.NewEncoder(w).Encode(d.vivas)
	})
	m.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var req api.SandboxRequest
		json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.falla[req.From] {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(api.Error{Message: "boom"})
			return
		}
		d.creadas++
		d.pedidas = append(d.pedidas, req)
		json.NewEncoder(w).Encode(&api.Machine{ID: "nueva0123456789", State: api.StateRunning})
	})
	m.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.borradas = append(d.borradas, r.PathValue("ref"))
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewUnstartedServer(m)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return d, sock
}

func precalentada(id string, st api.State) *api.Machine {
	return &api.Machine{ID: id, State: st, Labels: map[string]string{
		api.LabelKind: api.KindSandbox, LabelTemplate: "ts"}}
}

// Una precalentada rota (failed, stopped) no la reclama nadie: no cuenta para
// el fondo, que fabrica otra, y se borra para que no ocupe plaza para siempre.
// Las que el frontal sí reclama (running, frozen) cuentan y se dejan.
func TestVueltaCuentaSoloLasReclamables(t *testing.T) {
	d, sock := nuevoDaemonPool(t, 2,
		precalentada("rota0123456789a", api.StateFailed),
		precalentada("parada123456789", api.StateStopped),
		precalentada("viva01234567890", api.StateRunning))
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creadas != 1 {
		t.Errorf("created %d, want 1", d.creadas)
	}
	if len(d.borradas) != 2 || d.borradas[0] != "rota0123456789a" || d.borradas[1] != "parada123456789" {
		t.Errorf("removed %v, want the failed and the stopped one", d.borradas)
	}

	d, sock = nuevoDaemonPool(t, 1, precalentada("dormida12345678", api.StateWarm))
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	if d.creadas != 0 || len(d.borradas) != 0 {
		t.Errorf("a frozen prewarmed one: created %d, removed %v", d.creadas, d.borradas)
	}
}

// snapPool es el snapshot de la plantilla nombre con pool y guardado en hecho.
func snapPool(nombre string, pool int, hecho time.Time) *api.Snapshot {
	receta, _ := json.Marshal(map[string]any{"template": plantilla.Plantilla{Nombre: nombre, Imagen: "toolchain", Pool: pool}})
	return &api.Snapshot{Name: plantilla.SnapshotDe(nombre), CreatedAt: hecho,
		Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}
}

// Tras un rebuild (mismo nombre, otro snapshot) las precalentadas sin dueño de
// antes se borran y se fabrican del snapshot nuevo, con su versión puesta; las
// de una plantilla que ya no existe se borran sin reponer.
func TestVueltaBorraPrecalentadasViejasYHuerfanas(t *testing.T) {
	ahora := time.Now().UTC()
	sn := snapPool("ts", 1, ahora)
	vieja := precalentada("vieja0123456789", api.StateRunning)
	vieja.Labels[plantilla.LabelVersion] = plantilla.VersionDe(&api.Snapshot{CreatedAt: ahora.Add(-time.Hour)})
	huerfana := precalentada("huerfana1234567", api.StateWarm)
	huerfana.Labels[LabelTemplate] = "borrada"
	deTenant := precalentada("tenant012345678", api.StateRunning)
	deTenant.Labels[LabelTenant] = "ana"
	d, sock := nuevoDaemonPool(t, 0, vieja, huerfana, deTenant)
	d.snaps = []*api.Snapshot{sn}
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.borradas) != 2 || d.borradas[0] != "vieja0123456789" || d.borradas[1] != "huerfana1234567" {
		t.Errorf("removed %v, want the stale and the orphaned one (not the tenant's)", d.borradas)
	}
	if d.creadas != 1 || d.pedidas[0].Labels[plantilla.LabelVersion] != plantilla.VersionDe(sn) {
		t.Errorf("created %d (%+v), want one labeled with version %s", d.creadas, d.pedidas, plantilla.VersionDe(sn))
	}
}

// Que una plantilla no se pueda fabricar no deja sin fondo a las demás del host.
func TestVueltaSigueTrasUnFalloDeOtraPlantilla(t *testing.T) {
	ahora := time.Now().UTC()
	d, sock := nuevoDaemonPool(t, 0)
	d.snaps = []*api.Snapshot{snapPool("rota", 1, ahora), snapPool("buena", 1, ahora)}
	d.falla = map[string]bool{"sbx-rota": true}
	Nuevo(hosts.Nuevo(map[string]string{"h1": sock}), time.Hour, log.New(io.Discard, "", 0)).Vuelta(context.Background())
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.creadas != 1 || d.pedidas[0].From != "sbx-buena" {
		t.Errorf("created %d (%+v), want the prewarmed of sbx-buena", d.creadas, d.pedidas)
	}
}
