package frontal

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// conPausa pone delante del daemon falso un daemon que sabe pausar, reanudar y
// cambiar el egress (el falso no tiene esas rutas) y deja pasar el resto. Cuenta
// las llamadas que recibe.
type conPausa struct {
	mu     sync.Mutex
	llamas []string
}

func (p *conPausa) vistas() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.llamas...)
}

func ponerPausa(t *testing.T, f *falso) *conPausa {
	t.Helper()
	dir, err := os.MkdirTemp("", "ps")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no unix sockets here: %v", err)
	}
	p := &conPausa{}
	detras := f.endpoint
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) { r.URL.Scheme, r.URL.Host = "http", "daemon" },
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", detras)
		}},
	}
	estado := func(st api.State) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p.mu.Lock()
			p.llamas = append(p.llamas, r.Method+" "+r.URL.Path)
			p.mu.Unlock()
			mc := f.maquina(r.PathValue("ref"))
			mc.State = st
			json.NewEncoder(w).Encode(mc)
		}
	}
	m := http.NewServeMux()
	m.HandleFunc("POST /machines/{ref}/pause", estado(api.StatePaused))
	m.HandleFunc("POST /machines/{ref}/thaw", estado(api.StateRunning))
	m.HandleFunc("PATCH /machines/{ref}/egress", func(w http.ResponseWriter, r *http.Request) {
		var req api.EgressRequest
		json.NewDecoder(r.Body).Decode(&req)
		p.mu.Lock()
		p.llamas = append(p.llamas, r.Method+" "+r.URL.Path+" "+req.Egress)
		p.mu.Unlock()
		json.NewEncoder(w).Encode(api.EgressPolicy{Egress: req.Egress, AllowDomains: req.AllowDomains, Applied: api.EgressAppliedLive})
	})
	m.Handle("/", proxy)
	srv := httptest.NewUnstartedServer(m)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	f.endpoint = sock
	return p
}

// Pausar, reanudar y cambiar el egress funcionan sobre el sandbox propio y dan
// 404 sobre el de otro inquilino, sin llegar al daemon.
func TestPausaReanudarYEgress(t *testing.T) {
	f := nuevoFalso(t)
	d := ponerPausa(t, f)
	srv, _ := montar(t, map[string]*falso{"a": f})
	sb := crea(t, srv, tokenAlice, CrearPeticion{Image: "base"})
	_, ref, _ := partirID(sb.ID)

	// Ajeno: 404 en las tres, y el daemon no se entera.
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes/"+sb.ID+"/pause", nil), 404)
	esperaCodigo(t, pide(t, srv, tokenBob, "POST", "/v1/sandboxes/"+sb.ID+"/resume", nil), 404)
	esperaCodigo(t, pide(t, srv, tokenBob, "PATCH", "/v1/sandboxes/"+sb.ID+"/egress", api.EgressRequest{Egress: "internet"}), 404)
	if v := d.vistas(); len(v) != 0 {
		t.Fatalf("the daemon saw %v from another tenant", v)
	}

	// Propio.
	resp := pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/pause", nil)
	esperaCodigo(t, resp, 200)
	if r := decodifica[Sandbox](t, resp); r.ID != sb.ID || r.State != api.StatePaused {
		t.Fatalf("pause: %+v", r)
	}
	resp = pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/resume", nil)
	esperaCodigo(t, resp, 200)
	if r := decodifica[Sandbox](t, resp); r.State != api.StateRunning {
		t.Fatalf("resume: %+v", r)
	}
	resp = pide(t, srv, tokenAlice, "PATCH", "/v1/sandboxes/"+sb.ID+"/egress", api.EgressRequest{Egress: "allowlist", AllowDomains: []string{"example.com"}})
	esperaCodigo(t, resp, 200)
	if r := decodifica[api.EgressPolicy](t, resp); r.Egress != "allowlist" || r.Applied != api.EgressAppliedLive {
		t.Fatalf("egress: %+v", r)
	}
	want := []string{"POST /machines/" + ref + "/pause", "POST /machines/" + ref + "/thaw", "PATCH /machines/" + ref + "/egress allowlist"}
	got := d.vistas()
	if len(got) != len(want) {
		t.Fatalf("daemon calls %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: %q, want %q", i, got[i], want[i])
		}
	}

	// Método equivocado: 405; cuerpo roto en egress: 400.
	esperaCodigo(t, pide(t, srv, tokenAlice, "GET", "/v1/sandboxes/"+sb.ID+"/pause", nil), 405)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/"+sb.ID+"/egress", nil), 405)
	esperaCodigo(t, pide(t, srv, tokenAlice, "PATCH", "/v1/sandboxes/"+sb.ID+"/egress", "{"), 400)
}

// S9: reanudar una congelada no se salta la cuota (la dormida ya contaba, así
// que el tenant no puede acabar con más de lo que permite) y una pausada sigue
// sujeta al plazo de abandono.
func TestReanudarRespetaCuotaYPausadaCaduca(t *testing.T) {
	f := nuevoFalso(t)
	ponerPausa(t, f)
	hace := func(d time.Duration) *time.Time { x := time.Now().Add(-d); return &x }
	dormida := f.sembrar(&api.Machine{State: api.StateWarm, MemMiB: 256, TTLSeconds: 600, TTLAt: hace(time.Minute),
		Labels: map[string]string{LabelTenant: "alice"}})
	f.sembrar(&api.Machine{State: api.StateRunning, MemMiB: 256, TTLSeconds: 600, TTLAt: hace(time.Minute),
		Labels: map[string]string{LabelTenant: "alice"}})
	pausada := f.sembrar(&api.Machine{State: api.StatePaused, MemMiB: 256, TTLSeconds: 600, TTLAt: hace(30 * time.Hour),
		Labels: map[string]string{LabelTenant: "bob"}})
	srv, s := montar(t, map[string]*falso{"a": f},
		Tenant{Nombre: "alice", Token: tokenAlice, MaxSandboxes: 2, MaxMemTotalMiB: 512},
		Tenant{Nombre: "bob", Token: tokenBob})

	// Cupo lleno: crear una más da 429 antes y después de reanudar.
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", MemMiB: 256}), 429)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes/a/"+dormida.ID+"/resume", nil), 200)
	esperaCodigo(t, pide(t, srv, tokenAlice, "POST", "/v1/sandboxes", CrearPeticion{Image: "base", MemMiB: 256}), 429)
	resp := pide(t, srv, tokenAlice, "GET", "/v1/sandboxes", nil)
	esperaCodigo(t, resp, 200)
	if l := decodifica[[]Sandbox](t, resp); len(l) != 2 {
		t.Fatalf("alice has %d sandboxes after the resume, want 2", len(l))
	}

	// La pausada de bob, vieja, la barre el plazo de abandono como a cualquiera.
	s.Limpiar(context.Background())
	if f.maquina(pausada.ID) != nil {
		t.Fatal("a paused sandbox past the abandonment deadline survived the sweep")
	}
	if f.maquina(dormida.ID) == nil {
		t.Fatal("the recent sandbox was removed")
	}
}
