package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

func testServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	root := t.TempDir()
	bus := events.New()
	mgr, err := machine.NewManager(root, "", "", bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	s := &Server{mgr: mgr, root: root, bus: bus, store: &store{dir: filepath.Join(root, "store")}}
	return s, s.routes()
}

func call(t *testing.T, h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rr
}

func TestStoreRoundTrip(t *testing.T) {
	_, h := testServer(t)

	if rr := call(t, h, "GET", "/store/mcp/links", ""); rr.Code != 404 {
		t.Fatalf("clave inexistente: quería 404, salió %d", rr.Code)
	}
	if rr := call(t, h, "PUT", "/store/mcp/links", `{"a":1}`); rr.Code != 204 {
		t.Fatalf("PUT: %d %s", rr.Code, rr.Body)
	}
	rr := call(t, h, "GET", "/store/mcp/links", "")
	if rr.Code != 200 || strings.TrimSpace(rr.Body.String()) != `{"a":1}` {
		t.Fatalf("GET devolvió %d %q", rr.Code, rr.Body)
	}
	rr = call(t, h, "GET", "/store/mcp", "")
	var k api.StoreKeys
	if json.Unmarshal(rr.Body.Bytes(), &k); len(k.Keys) != 1 || k.Keys[0] != "links" {
		t.Fatalf("listado de claves: %s", rr.Body)
	}
	if rr := call(t, h, "DELETE", "/store/mcp/links", ""); rr.Code != 204 {
		t.Fatalf("DELETE: %d", rr.Code)
	}
	if rr := call(t, h, "DELETE", "/store/mcp/links", ""); rr.Code != 204 {
		t.Fatalf("borrar lo que no existe no es un error: %d", rr.Code)
	}
}

// Nada del cuerpo ni de la ruta puede sacar un fichero del directorio del store.
// Una ruta con ".." ni siquiera llega al handler: ServeMux la limpia y responde
// con una redirección, así que lo que se exige es que nunca haya un 2xx.
func TestStoreRechaza(t *testing.T) {
	_, h := testServer(t)
	for _, c := range []struct{ path, body string }{
		{"/store/MCP/links", `1`},
		{"/store/mcp/..", `1`},
		{"/store/mcp/%2e%2e%2fetc", `1`},
		{"/store/mcp/k", `no es json`},
	} {
		if rr := call(t, h, "PUT", c.path, c.body); rr.Code >= 200 && rr.Code < 300 {
			t.Errorf("PUT %s debió fallar, salió %d", c.path, rr.Code)
		}
	}
}

func TestAnotacionesPorHTTP(t *testing.T) {
	s, h := testServer(t)
	dir := filepath.Join(s.root, "snapshots", "eco")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"name":"eco","image":"eco"}`), 0o644)

	if rr := call(t, h, "GET", "/snapshots/nada", ""); rr.Code != 404 {
		t.Fatalf("snapshot inexistente: %d", rr.Code)
	}
	rr := call(t, h, "PUT", "/snapshots/eco/annotations/mcp.tools", `{"tools":[{"name":"echo"}]}`)
	if rr.Code != 200 {
		t.Fatalf("PUT anotación: %d %s", rr.Code, rr.Body)
	}
	var snap api.Snapshot
	json.Unmarshal(call(t, h, "GET", "/snapshots/eco", "").Body.Bytes(), &snap)
	if snap.Annotations["mcp.tools"] == nil || len(snap.Tools) != 1 {
		t.Fatalf("GET /snapshots/eco sin la anotación o sin espejo: %+v", snap)
	}

	// La ruta de v0.4 sigue funcionando y escribe la misma anotación.
	if rr := call(t, h, "PUT", "/snapshots/eco/health", `{"healthy":true}`); rr.Code != 200 {
		t.Fatalf("ruta antigua /health: %d", rr.Code)
	}
	json.Unmarshal(call(t, h, "GET", "/snapshots/eco", "").Body.Bytes(), &snap)
	if snap.Health != "healthy" || snap.Annotations["mcp.health"] == nil {
		t.Fatalf("la ruta antigua no escribió mcp.health: %+v", snap.Health)
	}
	if rr := call(t, h, "DELETE", "/snapshots/eco/annotations/mcp.health", ""); rr.Code != 204 {
		t.Fatalf("DELETE anotación: %d", rr.Code)
	}
}

// Las rutas /links de v0.4 funcionan encima del store, y el links.json antiguo
// se migra al arrancar sin perder nada.
func TestLinksSobreElStoreYMigracion(t *testing.T) {
	s, h := testServer(t)
	old := `[{"name":"engram","url":"http://mac:9100/mcp","created_at":"2026-08-01T00:00:00Z"}]`
	os.WriteFile(filepath.Join(s.root, "links.json"), []byte(old), 0o644)
	migrateLinks(s.root, s.store)

	if _, err := os.Stat(filepath.Join(s.root, "links.json.migrated")); err != nil {
		t.Fatal("el original debe quedar como links.json.migrated")
	}
	var list []*api.Link
	json.Unmarshal(call(t, h, "GET", "/links", "").Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "engram" || list[0].CreatedAt.IsZero() {
		t.Fatalf("links tras migrar: %s", call(t, h, "GET", "/links", "").Body)
	}

	if rr := call(t, h, "PUT", "/links", `{"name":"Obsidian","url":"http://mac:9200/mcp"}`); rr.Code != 200 {
		t.Fatalf("PUT /links: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "PUT", "/links", `{"name":"sin-url"}`); rr.Code != 400 {
		t.Fatalf("un link sin URL debe rechazarse: %d", rr.Code)
	}
	json.Unmarshal(call(t, h, "GET", "/links", "").Body.Bytes(), &list)
	if len(list) != 2 || list[0].Name != "Obsidian" {
		t.Fatalf("links tras añadir (ordenados): %d", len(list))
	}
	if rr := call(t, h, "DELETE", "/links/engram", ""); rr.Code != 204 {
		t.Fatalf("DELETE /links: %d", rr.Code)
	}
	if rr := call(t, h, "DELETE", "/links/engram", ""); rr.Code != 400 {
		t.Fatalf("borrar un link inexistente: %d (v0.4 devolvía 400)", rr.Code)
	}
	// Una segunda migración no pisa lo que ya hay en el store.
	os.WriteFile(filepath.Join(s.root, "links.json"), []byte(old), 0o644)
	migrateLinks(s.root, s.store)
	json.Unmarshal(call(t, h, "GET", "/links", "").Body.Bytes(), &list)
	if len(list) != 1 || list[0].Name != "Obsidian" {
		t.Fatalf("la migración repetida pisó el store: %+v", list)
	}
}
