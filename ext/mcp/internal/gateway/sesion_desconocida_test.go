package gateway

import (
	"net/http"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
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
