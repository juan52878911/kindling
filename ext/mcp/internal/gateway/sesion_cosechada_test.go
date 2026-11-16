package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un 404 del puente a un POST de la sesión (la cosechó, o su servidor MCP
// murió) olvida la ruta en el gateway, igual que un DELETE. Antes la ruta
// seguía viva: contaba como sesión de la instancia y forzaba réplicas para
// sesiones que ya no existían.
func TestPuente404OlvidaLaRuta(t *testing.T) {
	var olvidada atomic.Bool
	var posts atomic.Int32
	inv := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := api.LeerCuerpo(r.Body, 1<<20)
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		posts.Add(1)
		if strings.Contains(string(body), `"initialize"`) {
			w.Header().Set(SessionHeader, "del-invitado")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			return
		}
		if olvidada.Load() {
			http.Error(w, "session del-invitado unknown or expired", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
	})
	srv := httptest.NewServer(inv)
	defer srv.Close()
	gw := New(api.NewClient(daemonDosMaquinas(t, "eco", strings.TrimPrefix(srv.URL, "http://"))),
		5*time.Minute, false, 0, "")
	h := gw.Handler("")

	rec := pedir(t, h, "POST", "/mcp/eco", "", cuerpoInit)
	ext := rec.Header().Get(SessionHeader)
	if rec.Code != 200 || ext == "" {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	if rec := pedir(t, h, "POST", "/mcp/eco", ext, cuerpoCall); rec.Code != 200 {
		t.Fatalf("tools/call: %d %s", rec.Code, rec.Body)
	}

	olvidada.Store(true) // el puente cosecha la sesión
	if rec := pedir(t, h, "POST", "/mcp/eco", ext, cuerpoCall); rec.Code != http.StatusNotFound {
		t.Fatalf("tools/call tras cosechar: %d %s; quería el 404 del puente", rec.Code, rec.Body)
	}
	if gw.PeekRoute(ext) != nil {
		t.Fatal("la ruta de una sesión que el puente ya no conoce sigue viva")
	}
	// Y la siguiente ya no molesta al invitado: el gateway contesta solo.
	antes := posts.Load()
	if rec := pedir(t, h, "POST", "/mcp/eco", ext, cuerpoCall); rec.Code != http.StatusNotFound {
		t.Fatalf("tras olvidar: %d %s", rec.Code, rec.Body)
	}
	if posts.Load() != antes {
		t.Fatal("una sesión olvidada llegó al invitado")
	}
}
