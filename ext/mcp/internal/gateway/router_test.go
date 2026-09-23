package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling-mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
)

// mockRouterDaemon es mockDaemon (ver aggregate_test.go) más POST /machines y
// GET /procstats, que hacen falta para ejercitar Ensure() y el desempate por
// memoria del Router. Vive aparte para no complicar el mock que ya usan los
// tests de un solo host.
//
// runMachines decide qué contesta el POST /machines de ESTE daemon; nil
// significa "no debería llamarse" (falla el test si se llama).
func mockRouterDaemon(t *testing.T, snapshots []*api.Snapshot, links []*mcp.Link, availableMiB int64, runMachines http.HandlerFunc) (sockPath string, machineHits *int32) {
	t.Helper()
	sockPath = filepath.Join("/tmp", fmt.Sprintf("kg-router-test-%d.sock", time.Now().UnixNano()))
	var hits int32
	machineHits = &hits

	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(snapshots)
	})
	mux.HandleFunc("/store/mcp/links", func(w http.ResponseWriter, r *http.Request) {
		m := map[string]*mcp.Link{}
		for _, l := range links {
			m[l.Name] = l
		}
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Info{Capabilities: []string{"annotations", "store"}})
	})
	mux.HandleFunc("/machines", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			atomic.AddInt32(&hits, 1)
			if runMachines == nil {
				t.Errorf("POST /machines llamado sin runMachines configurado (host inesperado)")
				http.Error(w, `{"message":"unexpected"}`, http.StatusInternalServerError)
				return
			}
			runMachines(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]*api.Machine{})
	})
	mux.HandleFunc("/procstats", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ProcStats{AvailableMiB: availableMiB})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mock router daemon: ruta no implementada "+r.URL.Path, http.StatusNotFound)
	})

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		_ = server.Close()
		_ = os.Remove(sockPath)
	})
	return sockPath, machineHits
}

// linkedService fabrica un servicio ENLAZADO (no una microVM), que responde
// como un servidor MCP real detrás de httptest.Server. Usarlo evita todo el
// camino de despertar una máquina (Ensure/waitReady), que necesita KVM.
type linkedService struct {
	name string
	srv  *httptest.Server
	hits atomic.Int32
}

func newLinkedService(t *testing.T, name, toolName, toolDesc string) *linkedService {
	t.Helper()
	ls := &linkedService{name: name}
	ls.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ls.hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(SessionHeader, name+"-sess")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	t.Cleanup(ls.srv.Close)
	_ = toolName
	_ = toolDesc
	return ls
}

func (ls *linkedService) link(toolName, toolDesc string) *mcp.Link {
	return &mcp.Link{
		Name:   ls.name,
		URL:    ls.srv.URL + "/mcp",
		Labels: map[string]string{api.LabelService: ls.name},
		Tools:  []mcp.ToolSpec{{Name: toolName, Description: toolDesc}},
	}
}

// TestRouterEnrutaPorServicio comprueba lo básico: /mcp/<servicio> va al host
// que lo tiene, y ninguno responde por un servicio que no está en ningún lado.
func TestRouterEnrutaPorServicio(t *testing.T) {
	svcA := newLinkedService(t, "alfa", "hace_a", "hace A")
	svcB := newLinkedService(t, "beta", "hace_b", "hace B")

	sockUno, _ := mockRouterDaemon(t, nil, []*mcp.Link{svcA.link("hace_a", "hace A")}, 0, nil)
	sockDos, _ := mockRouterDaemon(t, nil, []*mcp.Link{svcB.link("hace_b", "hace B")}, 0, nil)

	r := NewRouter([]RouterHost{
		{Name: "uno", GW: New(api.NewClient(sockUno), 5*time.Minute, false, 0, "")},
		{Name: "dos", GW: New(api.NewClient(sockDos), 5*time.Minute, false, 0, "")},
	})
	h := r.Handler("")

	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := post("/mcp/alfa", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if svcA.hits.Load() == 0 {
		t.Errorf("/mcp/alfa no llegó al host 'uno' (code=%d, body=%s)", rec.Code, rec.Body.String())
	}
	if svcB.hits.Load() != 0 {
		t.Errorf("/mcp/alfa llegó también a beta, que vive en otro host")
	}

	rec = post("/mcp/beta", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if svcB.hits.Load() == 0 {
		t.Errorf("/mcp/beta no llegó al host 'dos' (code=%d, body=%s)", rec.Code, rec.Body.String())
	}

	rec = post("/mcp/gamma", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("servicio inexistente: código=%d, quería %d (Not Found)", rec.Code, http.StatusNotFound)
	}
}

// TestRouterUnSoloHostNoCambiaNada: con un solo host, Router es un paso
// intermedio transparente — la petición llega al único candidato exactamente
// igual que si se hablara con su Gateway directamente.
func TestRouterUnSoloHostNoCambiaNada(t *testing.T) {
	svc := newLinkedService(t, "solo", "hace_algo", "hace algo")
	sock, _ := mockRouterDaemon(t, nil, []*mcp.Link{svc.link("hace_algo", "hace algo")}, 0, nil)

	r := NewRouter([]RouterHost{
		{Name: "único", GW: New(api.NewClient(sock), 5*time.Minute, false, 0, "")},
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp/solo",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	r.Handler("").ServeHTTP(rec, req)

	if svc.hits.Load() != 1 {
		t.Errorf("con un solo host, la petición debería llegar una vez al enlace: llegó %d veces (code=%d, body=%s)",
			svc.hits.Load(), rec.Code, rec.Body.String())
	}
}

// TestRouterReintentaEnElSiguienteHostSi507 es el caso que motiva Router: un
// servicio vive (por catálogo) en dos hosts a la vez, el primero por memoria
// disponible no tiene sitio (507) y el segundo sí debe intentarse.
//
// Ninguno de los dos mocks "consigue" de verdad la máquina — devolver 507
// desde runFresh corta ANTES de esperar a que el invitado escuche, así que el
// test no necesita KVM ni una espera de 20s (readyTimeout).
func TestRouterReintentaEnElSiguienteHostSi507(t *testing.T) {
	compute := func() []*api.Snapshot {
		return []*api.Snapshot{mcp.WithTools(&api.Snapshot{
			Name:   "snap-compute",
			Labels: map[string]string{api.LabelService: "compute"},
		}, []mcp.ToolSpec{{Name: "run", Description: "hace algo"}})}
	}
	sinCabida := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInsufficientStorage) // 507
		_, _ = w.Write([]byte(`{"message":"not enough memory"}`))
	}

	sockPrimero, hitsPrimero := mockRouterDaemon(t, compute(), nil, 4096, sinCabida)
	sockSegundo, hitsSegundo := mockRouterDaemon(t, compute(), nil, 1024, sinCabida)

	r := NewRouter([]RouterHost{
		// El orden de -hosts es "primero, segundo", pero "primero" tiene MÁS
		// memoria disponible (4096 > 1024): debe probarse antes por eso, no
		// por el orden de declaración.
		{Name: "primero", GW: New(api.NewClient(sockPrimero), 5*time.Minute, false, 0, "")},
		{Name: "segundo", GW: New(api.NewClient(sockSegundo), 5*time.Minute, false, 0, "")},
	})

	req := httptest.NewRequest(http.MethodPost, "/mcp/compute",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()

	r.Handler("").ServeHTTP(rec, req)

	if got := atomic.LoadInt32(hitsPrimero); got != 1 {
		t.Errorf("host 'primero' (más memoria, se prueba antes): %d intento(s) a /machines, quería 1", got)
	}
	if got := atomic.LoadInt32(hitsSegundo); got != 1 {
		t.Errorf("host 'segundo' (el reintento): %d intento(s) a /machines, quería 1 — ¿no se reintentó?", got)
	}
	// Ninguno de los dos tenía sitio: la respuesta la traduce el ÚLTIMO host
	// probado (segundo), con el mismo código que un solo host habría dado.
	if rec.Code < http.StatusInternalServerError {
		t.Errorf("ningún host tenía sitio: esperaba un error del servidor, código=%d body=%s", rec.Code, rec.Body.String())
	}
}

// TestRouterAllCombinaVariosHosts comprueba /mcp/_all: el catálogo agregado
// incluye servicios de TODOS los hosts, y cada call_tool llega al host dueño.
// Un tercer host inalcanzable no debe tumbar el resultado de los otros dos.
func TestRouterAllCombinaVariosHosts(t *testing.T) {
	svcA := newLinkedService(t, "alfa", "hace_a", "hace A de verdad")
	svcB := newLinkedService(t, "beta", "hace_b", "hace B de verdad")

	sockUno, _ := mockRouterDaemon(t, nil, []*mcp.Link{svcA.link("hace_a", "hace A de verdad")}, 0, nil)
	sockDos, _ := mockRouterDaemon(t, nil, []*mcp.Link{svcB.link("hace_b", "hace B de verdad")}, 0, nil)

	r := NewRouter([]RouterHost{
		{Name: "uno", GW: New(api.NewClient(sockUno), 5*time.Minute, false, 0, "")},
		{Name: "dos", GW: New(api.NewClient(sockDos), 5*time.Minute, false, 0, "")},
		// Socket que no existe: host inalcanzable, debe ignorarse sin romper
		// el resto ("si un host no contesta, _all sigue con los demás").
		{Name: "roto", GW: New(api.NewClient(filepath.Join("/tmp", "no-existe-"+fmt.Sprint(time.Now().UnixNano())+".sock")), 5*time.Minute, false, 0, "")},
	})
	h := r.Handler("")

	// 1) initialize: sesión nueva, instrucciones con ambos servicios.
	req := httptest.NewRequest(http.MethodPost, "/mcp/_all",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	sid := rec.Header().Get(SessionHeader)
	if sid == "" {
		t.Fatalf("initialize no devolvió %s: code=%d body=%s", SessionHeader, rec.Code, rec.Body.String())
	}
	var initResp struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("initialize: respuesta no es JSON válido: %v (%s)", err, rec.Body.String())
	}
	if !strings.Contains(initResp.Result.Instructions, "alfa (host=uno)") {
		t.Errorf("instructions no menciona 'alfa (host=uno)':\n%s", initResp.Result.Instructions)
	}
	if !strings.Contains(initResp.Result.Instructions, "beta (host=dos)") {
		t.Errorf("instructions no menciona 'beta (host=dos)':\n%s", initResp.Result.Instructions)
	}

	call := func(name string, args map[string]any) map[string]any {
		params, _ := json.Marshal(map[string]any{
			"name":      "call_tool",
			"arguments": map[string]any{"name": name, "arguments": args},
		})
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": json.RawMessage(params),
		})
		req := httptest.NewRequest(http.MethodPost, "/mcp/_all", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(SessionHeader, sid)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("tools/call %s: respuesta no es JSON: %v (%s)", name, err, rec.Body.String())
		}
		return out
	}

	// 2) call_tool alfa.hace_a debe llegar al servidor real de 'uno'.
	res := call("alfa.hace_a", nil)
	if res["error"] != nil {
		t.Fatalf("alfa.hace_a devolvió error: %v", res["error"])
	}
	if svcA.hits.Load() == 0 {
		t.Errorf("call_tool alfa.hace_a no llegó al servidor real de alfa")
	}

	// 3) call_tool beta.hace_b debe llegar al servidor real de 'dos', no al de 'uno'.
	res = call("beta.hace_b", nil)
	if res["error"] != nil {
		t.Fatalf("beta.hace_b devolvió error: %v", res["error"])
	}
	if svcB.hits.Load() == 0 {
		t.Errorf("call_tool beta.hace_b no llegó al servidor real de beta")
	}
}

// TestByAvailableMemoryOrdena es la parte determinista y barata de aislar:
// dado /procstats de cada host, el orden de candidatos debe ir de más a menos
// memoria disponible, y un host que no contesta se va al final.
func TestByAvailableMemoryOrdena(t *testing.T) {
	sockPoca, _ := mockRouterDaemon(t, nil, nil, 512, nil)
	sockMucha, _ := mockRouterDaemon(t, nil, nil, 8192, nil)

	r := NewRouter([]RouterHost{
		{Name: "poca", GW: New(api.NewClient(sockPoca), time.Minute, false, 0, "")},
		{Name: "mucha", GW: New(api.NewClient(sockMucha), time.Minute, false, 0, "")},
		{Name: "rota", GW: New(api.NewClient(filepath.Join("/tmp", "no-existe-mem.sock")), time.Minute, false, 0, "")},
	})

	ranked := r.byAvailableMemory(t.Context(), []*RouterHost{r.hosts[0], r.hosts[1], r.hosts[2]})
	if len(ranked) != 3 {
		t.Fatalf("esperaba 3 candidatos, hubo %d", len(ranked))
	}
	if ranked[0].Name != "mucha" || ranked[1].Name != "poca" {
		var names []string
		for _, h := range ranked {
			names = append(names, h.Name)
		}
		t.Errorf("orden = %v, quería [mucha poca rota]", names)
	}
	if ranked[2].Name != "rota" {
		t.Errorf("el host inalcanzable debería ir al final, quedó en %v", ranked[2].Name)
	}
}
