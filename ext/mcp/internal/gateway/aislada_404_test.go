package gateway

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// invitadoSinSesion acepta el initialize y luego contesta 404 a todo POST:
// su servidor MCP perdió la sesión (cosechada, o el proceso murió).
type invitadoSinSesion struct{ invitadoRepetidor }

func (v *invitadoSinSesion) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := api.LeerCuerpo(r.Body, 1<<20)
	if r.Method == http.MethodPost && !strings.Contains(string(body), `"initialize"`) {
		http.Error(w, "session unknown or expired", http.StatusNotFound)
		return
	}
	r.Body = http.NoBody
	v.invitadoRepetidor.ServeHTTP(w, r)
}

// Un 404 del puente a una sesión aislada olvida su ruta, y con ella la única
// forma de llegar a su máquina: el cliente rehace el initialize y recibe otra.
// La vieja se destruye en ese momento en vez de quedarse ocupando memoria y
// overlay hasta su caducidad.
func TestAislada404LiberaSuMaquina(t *testing.T) {
	srv := httptest.NewServer(&invitadoSinSesion{invitadoRepetidor{nombre: "A"}})
	t.Cleanup(srv.Close)
	d := &daemonAislado{service: "notas", invitados: []string{strings.TrimPrefix(srv.URL, "http://")}}
	h := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "").Handler("")
	rec := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit)
	sid := rec.Header().Get(SessionHeader)
	if rec.Code != 200 || sid == "" || !d.existe("m1") {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	if rec := pedir(t, h, "POST", "/mcp/notas", sid, cuerpoCall); rec.Code != http.StatusNotFound {
		t.Fatalf("la llamada: %d %s (quería el 404 del puente)", rec.Code, rec.Body)
	}
	if d.existe("m1") {
		t.Fatal("la máquina de la sesión aislada sigue viva tras perder su ruta")
	}
}

// El aviso de tipos reparados no copia los argumentos al log del gateway:
// pueden llevar secretos o datos del usuario.
func TestTiposReparadosNoVanAlLog(t *testing.T) {
	const secreto = "valor-que-no-debe-salir"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := api.LeerCuerpo(r.Body, 1<<20)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(SessionHeader, "s1")
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case bytes.Contains(body, []byte(`"tools/list"`)):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo","inputSchema":` +
				`{"type":"object","properties":{"n":{"type":"integer"},"clave":{"type":"string"}}}}]}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
		}
	}))
	t.Cleanup(srv.Close)
	h := New(api.NewClient(daemonDosMaquinas(t, "eco", strings.TrimPrefix(srv.URL, "http://"))),
		5*time.Minute, false, 0, "").Handler("")
	sid := abrirAgregado(t, h)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	rec := pedir(t, h, "POST", "/mcp/_all", sid, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":`+
		`{"name":"call_tool","arguments":{"name":"eco.echo","arguments":{"n":"5","clave":"`+secreto+`"}}}}`)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("call_tool: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(buf.String(), "repaired") {
		t.Fatalf("no se reparó nada (el test no prueba lo que dice): %q", buf.String())
	}
	if strings.Contains(buf.String(), secreto) {
		t.Fatalf("los argumentos salieron al log: %q", buf.String())
	}
}
