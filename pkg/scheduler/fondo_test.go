package scheduler

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// daemonFondo es el daemon mínimo que necesita PrewarmAll: el catálogo de
// snapshots y la foto de memoria (/procstats). procStats nil = el daemon
// falla al darla.
type daemonFondo struct {
	snaps     []*api.Snapshot
	procStats *api.ProcStats
}

func (d *daemonFondo) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/snapshots":
		_ = json.NewEncoder(w).Encode(d.snaps)
	case "/procstats":
		if d.procStats == nil {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(d.procStats)
	default:
		http.NotFound(w, r)
	}
}

// gwFondo levanta d en un socket unix y un planificador efímero con fondo de
// tamaño size, popularidad solo en memoria (nada de tocar el fichero real) y
// un warmer de mentira que cuenta lo que precalienta por servicio.
func gwFondo(t *testing.T, d *daemonFondo, size int) (*Scheduler, *contadorWarm) {
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
	srv := &http.Server{Handler: d}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	g := New(api.NewClient("unix://"+sock), time.Minute, true, size)
	g.SetPopularityFile("")
	c := &contadorWarm{por: map[string]int{}}
	g.pool.warmFn = c.warm
	g.pool.removeFn = func(context.Context, string) error { return nil }
	return g, c
}

type contadorWarm struct {
	mu  sync.Mutex
	por map[string]int
}

func (c *contadorWarm) warm(_ context.Context, service, _ string) (*warmVM, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.por[service]++
	return &warmVM{id: service, born: time.Now()}, nil
}

func (c *contadorWarm) servicios() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var s []string
	for k := range c.por {
		s = append(s, k)
	}
	sort.Strings(s)
	return s
}

func (c *contadorWarm) de(svc string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.por[svc]
}

func snapDe(svc string, mem int) *api.Snapshot {
	return &api.Snapshot{Name: svc + "-gold", MemMiB: mem, Labels: map[string]string{api.LabelService: svc}}
}

// prewarmar corre una pasada y espera a que acaben las reposiciones.
func prewarmar(g *Scheduler) {
	g.PrewarmAll(context.Background())
	g.pool.wg.Wait()
}

// Z1: con historial, un servicio que nadie llama (puntuación 0) no recibe
// fondo. Antes sí, y en efímero su copia caducaba por edad cada 2 × idle y
// la pasada siguiente la recreaba: restore + initialize para siempre.
func TestPrewarmNoRellenaLoQueNadieLlama(t *testing.T) {
	d := &daemonFondo{
		snaps:     []*api.Snapshot{snapDe("usado", 256), snapDe("ocioso", 256)},
		procStats: &api.ProcStats{AvailableMiB: 64 * 1024},
	}
	g, c := gwFondo(t, d, 1)
	g.Observe("usado")
	prewarmar(g)
	if got := c.servicios(); len(got) != 1 || got[0] != "usado" {
		t.Fatalf("precalentó %v, quería solo [usado]", got)
	}

	// Aunque la EMA decaiga y se pode sin tráfico, el historial ya existe: el
	// ocioso sigue sin fondo (si volviera al reparto a ciegas, el churn volvería
	// a los pocos minutos de silencio).
	for range 20 {
		g.pop.fold()
	}
	g.pool.ready = map[string][]*warmVM{}
	prewarmar(g)
	if n := c.de("ocioso"); n != 0 {
		t.Fatalf("tras decaer la EMA precalentó %d del ocioso, quería 0", n)
	}
}

// Sin historial (gateway recién nacido) se rellena todo, como siempre: aún no
// se sabe qué es popular.
func TestPrewarmSinHistorialRellenaTodo(t *testing.T) {
	d := &daemonFondo{
		snaps:     []*api.Snapshot{snapDe("a", 256), snapDe("b", 256)},
		procStats: &api.ProcStats{AvailableMiB: 64 * 1024},
	}
	g, c := gwFondo(t, d, 1)
	prewarmar(g)
	if got := c.servicios(); len(got) != 2 {
		t.Fatalf("precalentó %v, quería [a b]", got)
	}
}
