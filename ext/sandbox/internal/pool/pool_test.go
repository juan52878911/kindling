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
		json.NewEncoder(w).Encode([]*api.Snapshot{{Name: "sbx-ts",
			Annotations: map[string]json.RawMessage{plantilla.AnotacionReceta: receta}}})
	})
	m.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		json.NewEncoder(w).Encode(d.vivas)
	})
	m.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.creadas++
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
