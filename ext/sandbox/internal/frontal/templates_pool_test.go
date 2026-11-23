package frontal

// PUT /v1/templates/{name}/pool: fija el fondo precalentado de una plantilla en
// cada host que la tiene. Un daemon mínimo propio (sin KVM): sabe listar
// snapshots y fijar su fondo, y apunta lo que le piden.

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
	"github.com/juan52878911/kindling/pkg/api"
)

type demonioPool struct {
	endpoint string

	mu     sync.Mutex
	snaps  []string
	puts   []string // "snapshot=tamaño"
	listo  int      // ready que dice el daemon
	falla  int      // si != 0, PUT contesta con ese código
	cuerpo string
}

func nuevoDemonioPool(t *testing.T, snaps ...string) *demonioPool {
	t.Helper()
	dir, err := os.MkdirTemp("", "pl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "d.sock"))
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	d := &demonioPool{endpoint: filepath.Join(dir, "d.sock"), snaps: snaps}
	m := http.NewServeMux()
	m.HandleFunc("GET /snapshots", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		out := []*api.Snapshot{}
		for _, n := range d.snaps {
			out = append(out, &api.Snapshot{Name: n})
		}
		writeJSON(w, 200, out)
	})
	m.HandleFunc("PUT /snapshots/{name}/pool", func(w http.ResponseWriter, r *http.Request) {
		var pet api.PoolRequest
		json.NewDecoder(r.Body).Decode(&pet)
		d.mu.Lock()
		defer d.mu.Unlock()
		d.puts = append(d.puts, fmt.Sprintf("%s=%d", r.PathValue("name"), pet.Size))
		if d.falla != 0 {
			failDaemon(w, d.falla, fmt.Errorf("%s", d.cuerpo))
			return
		}
		writeJSON(w, 200, api.PoolInfo{Template: r.PathValue("name"), Size: pet.Size, Ready: min(d.listo, pet.Size)})
	})
	srv := httptest.NewUnstartedServer(m)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	return d
}

func (d *demonioPool) llamadas() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.puts...)
}

// montarPool monta el frontal sobre daemons de pool (montar toma *falso).
func montarPool(t *testing.T, ds map[string]*demonioPool) *httptest.Server {
	t.Helper()
	eps := map[string]string{}
	for n, d := range ds {
		eps[n] = d.endpoint
	}
	s, err := Nuevo(Opciones{Hosts: hosts.Nuevo(eps), Tenants: tenantsConAdmin(), Log: testLog(t)})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

func TestPoolDeAdminFijaElFondoSoloDondeEstaLaPlantilla(t *testing.T) {
	a := nuevoDemonioPool(t, "sbx-python", "sbx-node")
	b := nuevoDemonioPool(t, "sbx-python")
	c := nuevoDemonioPool(t, "sbx-otra")
	a.listo, b.listo = 2, 1
	srv := montarPool(t, map[string]*demonioPool{"a": a, "b": b, "c": c})

	resp := pide(t, srv, tokenAdmin, "PUT", "/v1/templates/python/pool", map[string]int{"size": 3})
	esperaCodigo(t, resp, 200)
	out := decodifica[PoolRespuesta](t, resp)
	if out.Template != "python" || out.Size != 3 || out.Available != 3 || len(out.Hosts) != 2 {
		t.Fatalf("respuesta: %+v", out)
	}
	if got := a.llamadas(); len(got) != 1 || got[0] != "sbx-python=3" {
		t.Errorf("host a: %v", got)
	}
	if got := b.llamadas(); len(got) != 1 || got[0] != "sbx-python=3" {
		t.Errorf("host b: %v", got)
	}
	if got := c.llamadas(); len(got) != 0 {
		t.Errorf("el host sin la plantilla recibió %v", got)
	}

	// size 0 lo apaga.
	resp = pide(t, srv, tokenAdmin, "PUT", "/v1/templates/python/pool", map[string]int{"size": 0})
	esperaCodigo(t, resp, 200)
	if got := a.llamadas(); got[len(got)-1] != "sbx-python=0" {
		t.Errorf("apagar: %v", got)
	}
}

func TestPoolExigeAdmin(t *testing.T) {
	a := nuevoDemonioPool(t, "sbx-python")
	srv := montarPool(t, map[string]*demonioPool{"a": a})
	esperaCodigo(t, pide(t, srv, tokenAlice, "PUT", "/v1/templates/python/pool", map[string]int{"size": 1}), 403)
	esperaCodigo(t, pide(t, srv, "", "PUT", "/v1/templates/python/pool", map[string]int{"size": 1}), 401)
	if got := a.llamadas(); len(got) != 0 {
		t.Fatalf("un no-admin llegó al daemon: %v", got)
	}
}

func TestPoolValidaTamanoYPlantilla(t *testing.T) {
	a := nuevoDemonioPool(t, "sbx-python")
	srv := montarPool(t, map[string]*demonioPool{"a": a})
	esperaCodigo(t, pide(t, srv, tokenAdmin, "PUT", "/v1/templates/python/pool", map[string]int{"size": api.PoolMax + 1}), 400)
	esperaCodigo(t, pide(t, srv, tokenAdmin, "PUT", "/v1/templates/python/pool", map[string]int{"size": -1}), 400)
	esperaCodigo(t, pide(t, srv, tokenAdmin, "PUT", "/v1/templates/python/pool", "no es json"), 400)
	esperaCodigo(t, pide(t, srv, tokenAdmin, "PUT", "/v1/templates/nada/pool", map[string]int{"size": 1}), 404)
	if got := a.llamadas(); len(got) != 0 {
		t.Fatalf("peticiones inválidas llegaron al daemon: %v", got)
	}
}

func TestPoolUnHostQueFallaNoImpideElResto(t *testing.T) {
	a := nuevoDemonioPool(t, "sbx-python")
	b := nuevoDemonioPool(t, "sbx-python")
	a.listo = 4
	b.falla, b.cuerpo = 500, "boom"
	srv := montarPool(t, map[string]*demonioPool{"a": a, "b": b})
	resp := pide(t, srv, tokenAdmin, "PUT", "/v1/templates/python/pool", map[string]int{"size": 4})
	esperaCodigo(t, resp, 200)
	out := decodifica[PoolRespuesta](t, resp)
	var fallos int
	for _, h := range out.Hosts {
		if h.Error != "" {
			fallos++
		}
	}
	if out.Available != 4 || fallos != 1 {
		t.Fatalf("respuesta: %+v", out)
	}

	// Si fallan todos, 502.
	a.mu.Lock()
	a.falla, a.cuerpo = 500, "boom"
	a.mu.Unlock()
	esperaCodigo(t, pide(t, srv, tokenAdmin, "PUT", "/v1/templates/python/pool", map[string]int{"size": 4}), 502)
}
