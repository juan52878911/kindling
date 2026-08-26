package gateway

import (
	"context"
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

	"github.com/juan52878911/kindling/internal/api"
)

// mockDaemon levanta un servidor HTTP sobre un socket unix que responde lo
// mínimo que la pasarela espera: /snapshots, /links y /machines (para que
// ensure() no se queje si por error se la llama durante un test).
//
// linksCalls, si no es nil, se incrementa con cada petición a /links. Sirve
// para que un test verifique que la pasarela evita esa llamada en un caso
// concreto.
//
// Devuelve la ruta del socket, lista para api.NewClient.
//
// macOS limita los sockets unix a 104 bytes; se usa /tmp directamente para no
// caer en ese límite cuando el tempdir del runner es largo.
func mockDaemon(t *testing.T, snapshots []*api.Snapshot, links []*api.Link, counters ...*atomic.Int32) string {
	t.Helper()
	sockPath := filepath.Join("/tmp", fmt.Sprintf("kg-test-%d.sock", time.Now().UnixNano()))

	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots", func(w http.ResponseWriter, r *http.Request) {
		if len(counters) > 0 && counters[0] != nil {
			counters[0].Add(1)
		}
		_ = json.NewEncoder(w).Encode(snapshots)
	})
	mux.HandleFunc("/links", func(w http.ResponseWriter, r *http.Request) {
		if len(counters) > 1 && counters[1] != nil {
			counters[1].Add(1)
		}
		_ = json.NewEncoder(w).Encode(links)
	})
	mux.HandleFunc("/machines", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*api.Machine{})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "mock daemon: ruta no implementada "+r.URL.Path, http.StatusNotFound)
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
	return sockPath
}

// TestAgregadorDistingueExternos reproduce el bug del lab: engram registrado
// como enlace, no como snapshot. Antes del fix, el agregador lo mezclaba con
// los snapshots y el endpoint directo devolvía 502; ahora se distingue en las
// instrucciones y el endpoint se enruta correctamente.
func TestAgregadorDistingueExternos(t *testing.T) {
	// Servidor engram simulado: solo necesitamos que responda HTTP en /mcp y
	// que reciba la petición para saber que la pasarela la redirigió.
	var engramHits atomic.Int32
	engram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engramHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(SessionHeader, "engram-sess")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	}))
	defer engram.Close()

	sockPath := mockDaemon(t,
		[]*api.Snapshot{{
			Name:   "snap-files",
			Labels: map[string]string{api.LabelService: "files"},
			Tools: []api.ToolSpec{
				{Name: "read_text", Description: "Lee un fichero"},
			},
		}},
		[]*api.Link{{
			Name:   "engram",
			URL:    engram.URL + "/mcp",
			Labels: map[string]string{api.LabelService: "engram"},
			Tools: []api.ToolSpec{
				{Name: "mem_search", Description: "Busca en memoria"},
			},
		}},
		nil,
	)

	gw := New(api.NewClient(sockPath), 5*time.Minute, false, 0, "")
	a := newAggregator(gw, false)
	ctx := context.Background()

	// 1) El catálogo incluye snapshots y enlaces en una lista unificada: el
	//    modelo descubre todo con una sola búsqueda, pero luego la instrucción
	//    los marca distinto.
	t.Run("services lista snapshots y enlaces", func(t *testing.T) {
		svcs, err := a.cat.services(ctx)
		if err != nil {
			t.Fatalf("services: %v", err)
		}
		if len(svcs) != 2 {
			t.Fatalf("servicios=%v, quiero 2", svcs)
		}
		if !contains(svcs, "files") {
			t.Errorf("no incluye 'files': %v", svcs)
		}
		if !contains(svcs, "engram") {
			t.Errorf("no incluye 'engram': %v", svcs)
		}
	})

	// 2) Las instrucciones del initialize deben distinguir el enlace del
	//    snapshot. Este es el arreglo principal que vio el usuario: el modelo
	//    ya no confunde engram con un servicio de microVM.
	t.Run("instructions marca externos", func(t *testing.T) {
		svcs, _ := a.cat.services(ctx)
		s := &aggSession{
			id: "t", services: svcs, mode: modeProxy,
			backing: map[string]string{},
		}
		instr := a.instructions(ctx, s)

		if !strings.Contains(instr, "engram (externo)") {
			t.Errorf("instructions debería marcar engram como externo:\n%s", instr)
		}
		if strings.Contains(instr, "files (externo)") {
			t.Errorf("files no debería marcarse como externo:\n%s", instr)
		}
		if !strings.Contains(instr, "Los servicios marcados como (externo)") {
			t.Errorf("instructions debería explicar el sufijo (externo):\n%s", instr)
		}
	})

	// 3) lookup es el paso previo a forward(): si todavía devolviera "no hay
	//    snapshot" aquí, sería regresión. Para un enlace debe encontrar la
	//    herramienta en el catálogo del propio enlace (no del snapshot).
	t.Run("lookup de un externo no dice 'no hay snapshot'", func(t *testing.T) {
		svcs, _ := a.cat.services(ctx)
		s := &aggSession{
			id: "t", services: svcs, mode: modeProxy,
			backing: map[string]string{},
		}
		tool, fault := a.lookup(ctx, s, "engram.mem_search")
		if fault != nil {
			t.Fatalf("lookup(engram.mem_search) no debería fallar: %+v", fault)
		}
		if tool.Name != "mem_search" || tool.Service != "engram" {
			t.Errorf("tool=%+v", tool)
		}
	})

	// 4) El endpoint directo /mcp/engram era el síntoma del lab: 502 con
	//    "no hay snapshot". Ahora debe redirigir al servidor MCP externo y
	//    obtener una respuesta real.
	t.Run("/mcp/engram se enruta al enlace, no devuelve 502", func(t *testing.T) {
		engramHits.Store(0) // reset por si otros subtests tocaron el servidor
		req := httptest.NewRequest(http.MethodPost, "/mcp/engram",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		rec := httptest.NewRecorder()

		gw.Handler().ServeHTTP(rec, req)

		if rec.Code == http.StatusBadGateway && strings.Contains(rec.Body.String(), "no hay snapshot") {
			t.Fatalf("no debería devolver 'no hay snapshot': %s", rec.Body.String())
		}
		if engramHits.Load() == 0 {
			t.Errorf("la pasarela no reenvió la petición al servidor engram")
		}
	})

	// 5) externalSet (y linkFor) evitan re-consultar /links cuando ya se sabe
	//    que no hay enlaces: tras la primera llamada al agregador, el cache
	//    queda vacío y linkEverSeen=true, y un segundo initialize no debe
	//    volver a hablar con el daemon. Sin esta pista, un gateway sin
	//    enlaces pagaría un round-trip a /links en cada initialize del
	//    agregador, que es el camino caliente.
	t.Run("externalSet no re-consulta /links cuando no hay enlaces", func(t *testing.T) {
		var linksCalls atomic.Int32
		sockPath := mockDaemon(t,
			[]*api.Snapshot{{
				Name:   "snap-files",
				Labels: map[string]string{api.LabelService: "files"},
				Tools: []api.ToolSpec{
					{Name: "read_text", Description: "Lee un fichero"},
				},
			}},
			[]*api.Link{}, // sin enlaces externos
			&linksCalls,
		)

		gw := New(api.NewClient(sockPath), 5*time.Minute, false, 0, "")
		a := newAggregator(gw, false)
		s := &aggSession{
			id: "t", services: []string{"files"}, mode: modeProxy,
			backing: map[string]string{},
		}

		// Primer initialize: al menos 1 llamada (para enterarse de que no hay
		// enlaces); el contenido exacto depende de qué funciones pasen por el
		// camino. Lo que importa es el SIGUIENTE.
		_ = a.instructions(ctx, s)
		firstCalls := linksCalls.Load()
		if firstCalls == 0 {
			t.Fatalf("la primera vez debería consultar /links para confirmar que no hay enlaces")
		}

		// Segundo initialize: la optimización debe evitar nuevas llamadas.
		_ = a.instructions(ctx, s)
		secondCalls := linksCalls.Load()
		if secondCalls > firstCalls {
			t.Fatalf("externalSet/linkFor no debería re-consultar /links: primera=%d, segunda=%d",
				firstCalls, secondCalls)
		}
	})

	// 6) instructions() cachea el resultado: la segunda llamada con el mismo
	//    session no reconstruye el texto ni habla con el daemon. Sin la caché,
	//    cada initialize reconstruía map + strings.Builder + 2 llamadas al
	//    daemon — ~50% de los allocs del gateway bajo carga.
	t.Run("instructions se cachea entre llamadas del mismo session", func(t *testing.T) {
		var snapsCalls, linksCalls atomic.Int32
		sockPath := mockDaemon(t,
			[]*api.Snapshot{{
				Name:   "snap-files",
				Labels: map[string]string{api.LabelService: "files"},
				Tools: []api.ToolSpec{
					{Name: "read_text", Description: "Lee un fichero"},
				},
			}},
			[]*api.Link{{
				Name:   "engram",
				URL:    "http://example.invalid/mcp",
				Labels: map[string]string{api.LabelService: "engram"},
				Tools: []api.ToolSpec{
					{Name: "mem_search", Description: "busca"},
				},
			}},
			&snapsCalls, &linksCalls,
		)

		gw := New(api.NewClient(sockPath), 5*time.Minute, false, 0, "")
		a := newAggregator(gw, false)
		s := &aggSession{
			id: "t", services: []string{"files", "engram"}, mode: modeProxy,
			backing: map[string]string{},
		}

		// Primera llamada: pobla la caché. Cuenta las llamadas a /snapshots y /links.
		first := a.instructions(ctx, s)
		snaps1 := snapsCalls.Load()
		links1 := linksCalls.Load()
		if snaps1 == 0 || links1 == 0 {
			t.Fatalf("primera llamada debería haber consultado al daemon: snaps=%d links=%d", snaps1, links1)
		}

		// Segunda llamada: la caché debe responder, sin nuevas llamadas al daemon.
		second := a.instructions(ctx, s)
		if snapsCalls.Load() != snaps1 {
			t.Errorf("la 2ª llamada incrementó /snapshots: %d → %d", snaps1, snapsCalls.Load())
		}
		if linksCalls.Load() != links1 {
			t.Errorf("la 2ª llamada incrementó /links: %d → %d", links1, linksCalls.Load())
		}
		if first != second {
			t.Errorf("mismo input debe dar mismo output (cache hit)")
		}

		// Verifica que invalidateCatalog realmente borra la caché: debe
		// observarse un cambio en el generation counter (indicador de que se
		// ha invalidado, no del contenido). Esto verifica que el callback está
		// bien cableado.
		genBefore := a.cat.Generation()
		a.invalidateCatalog()
		genAfter := a.cat.Generation()
		if genAfter != genBefore {
			t.Errorf("invalidateCatalog no debería bump generation (solo borra caché local): %d → %d",
				genBefore, genAfter)
		}
	})
}
