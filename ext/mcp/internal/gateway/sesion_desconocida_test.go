package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

const cuerpoNotif = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

// Un Mcp-Session-Id que el agregador no conoce (reinicio del gateway, sesión
// segada) tiene que dar 404 para que el cliente rehaga el initialize. Antes se
// acuñaba una sesión nueva en silencio, con backing vacío: cada tools/call
// disparaba un initialize y otro proceso en la microVM.
func TestAgregadorSidDesconocidoDa404(t *testing.T) {
	sock := mockDaemon(t, nil, nil)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	h := gw.Handler("")

	for _, body := range []string{cuerpoCall, cuerpoNotif} {
		rec := pedir(t, h, "POST", "/mcp/_all", "no-existe", body)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("sid desconocido (%s): %d %s; quería 404", body, rec.Code, rec.Body)
		}
		if got := rec.Header().Get(SessionHeader); got != "" {
			t.Fatalf("sid desconocido acuñó la sesión %q", got)
		}
	}
	// Una notificación sin cabecera no abre conversación: nadie va a volver
	// con un id que nunca recibió.
	if rec := pedir(t, h, "POST", "/mcp/_all", "", cuerpoNotif); rec.Code != http.StatusAccepted || rec.Header().Get(SessionHeader) != "" {
		t.Fatalf("notificación sin sesión: %d sid=%q", rec.Code, rec.Header().Get(SessionHeader))
	}
	gw.agg.mu.Lock()
	n := len(gw.agg.sessions)
	gw.agg.mu.Unlock()
	if n != 0 {
		t.Fatalf("quedan %d sesiones del agregador; quería 0", n)
	}
}

func TestRouterAllSidDesconocidoDa404(t *testing.T) {
	sock := mockDaemon(t, nil, nil)
	r := NewRouter([]RouterHost{{Name: "uno", GW: New(api.NewClient(sock), 5*time.Minute, false, 0, "")}})
	h := r.Handler("")

	for _, body := range []string{cuerpoCall, cuerpoNotif} {
		rec := pedir(t, h, "POST", "/mcp/_all", "no-existe", body)
		if rec.Code != http.StatusNotFound || rec.Header().Get(SessionHeader) != "" {
			t.Fatalf("sid desconocido (%s): %d sid=%q; quería 404 sin sesión", body, rec.Code, rec.Header().Get(SessionHeader))
		}
	}
	if rec := pedir(t, h, "POST", "/mcp/_all", "", cuerpoNotif); rec.Code != http.StatusAccepted || rec.Header().Get(SessionHeader) != "" {
		t.Fatalf("notificación sin sesión: %d sid=%q", rec.Code, rec.Header().Get(SessionHeader))
	}
	r.agg.mu.Lock()
	n := len(r.agg.sessions)
	r.agg.mu.Unlock()
	if n != 0 {
		t.Fatalf("quedan %d sesiones del router; quería 0", n)
	}

	// El camino normal sigue: initialize acuña y el sid vale después.
	rec := pedir(t, h, "POST", "/mcp/_all", "", cuerpoInit)
	sid := rec.Header().Get(SessionHeader)
	if rec.Code != 200 || sid == "" {
		t.Fatalf("initialize: %d sid=%q", rec.Code, sid)
	}
	if rec := pedir(t, h, "POST", "/mcp/_all", sid, `{"jsonrpc":"2.0","id":3,"method":"ping"}`); rec.Code != 200 {
		t.Fatalf("ping con sesión válida: %d %s", rec.Code, rec.Body)
	}
}

// El modo router crea microVMs aisladas por conversación (forward con
// claveAislada de su sesión sombra), así que cerrar o segar la conversación
// del router tiene que destruirlas, igual que hace un solo host.
func TestRouterSueltaLasAisladas(t *testing.T) {
	_, addrs := invitadosFalsos(t, 6)
	d := &daemonAislado{service: "notas", invitados: addrs}
	r := NewRouter([]RouterHost{{Name: "uno", GW: New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")}})
	h := r.Handler("")
	vivas := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		return len(d.maquinas)
	}
	conversar := func() string {
		t.Helper()
		rec := pedir(t, h, "POST", "/mcp/_all?mode=expand", "", cuerpoInit)
		sid := rec.Header().Get(SessionHeader)
		if rec.Code != 200 || sid == "" {
			t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
		}
		rec = pedir(t, h, "POST", "/mcp/_all", sid,
			`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"notas.echo","arguments":{}}}`)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "soy") {
			t.Fatalf("tools/call: %d %s", rec.Code, rec.Body)
		}
		return sid
	}

	// DELETE de la conversación.
	sid := conversar()
	if vivas() != 1 {
		t.Fatalf("tras la llamada hay %d máquinas; quería 1 (la de la conversación)", vivas())
	}
	if rec := pedir(t, h, "DELETE", "/mcp/_all", sid, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d", rec.Code)
	}
	if n := vivas(); n != 0 {
		t.Fatalf("tras el DELETE quedan %d máquinas aisladas", n)
	}

	// Segador.
	conversar()
	if vivas() != 1 {
		t.Fatalf("segunda conversación: %d máquinas", vivas())
	}
	r.agg.reap(t.Context(), 0)
	if n := vivas(); n != 0 {
		t.Fatalf("tras segar quedan %d máquinas aisladas", n)
	}
}

// Una clave de sesión es de quien la abrió: otro token válido (otro tenant) que
// la conozca no puede usarla ni cerrarla. Antes solo se comprobaba el servicio.
func TestSesionLigadaAlTenant(t *testing.T) {
	_, addrs := invitadosFalsos(t, 4)
	d := &daemonAislado{service: "notas", invitados: addrs, compartido: true}
	gw := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "")
	gw.SetTenants([]scheduler.TenantLimit{{Name: "ana", Token: "tok-ana"}, {Name: "bea", Token: "tok-bea"}})
	h := gw.Handler("")
	con := func(tok, metodo, ruta, sid, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(metodo, ruta, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		if sid != "" {
			req.Header.Set(SessionHeader, sid)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for _, ruta := range []string{"/mcp/notas", "/mcp/_all"} {
		rec := con("tok-ana", "POST", ruta, "", cuerpoInit)
		sid := rec.Header().Get(SessionHeader)
		if rec.Code != 200 || sid == "" {
			t.Fatalf("%s initialize de ana: %d %s", ruta, rec.Code, rec.Body)
		}
		ping := `{"jsonrpc":"2.0","id":3,"method":"ping"}`
		if rec := con("tok-bea", "POST", ruta, sid, ping); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: bea usó la sesión de ana: %d %s", ruta, rec.Code, rec.Body)
		}
		if rec := con("tok-bea", "DELETE", ruta, sid, ""); rec.Code == http.StatusNoContent && ruta == "/mcp/notas" {
			t.Fatalf("%s: bea cerró la sesión de ana", ruta)
		}
		if rec := con("tok-ana", "POST", ruta, sid, ping); rec.Code != 200 {
			t.Fatalf("%s: ana perdió su sesión: %d %s", ruta, rec.Code, rec.Body)
		}
	}
}
