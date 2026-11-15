package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// El DELETE de una sesión aislada guarda la respuesta del invitado con tope:
// uno que contesta 64 MiB da 502, el gateway no los guarda y la máquina se
// destruye igual.
func TestDeleteAisladaConRespuestaSinFin(t *testing.T) {
	bloque := []byte(strings.Repeat("x", 1<<20))
	inv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(SessionHeader, "s1")
		if r.Method == http.MethodDelete {
			for i := 0; i < 64; i++ {
				if _, err := w.Write(bloque); err != nil {
					return
				}
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(inv.Close)
	d := &daemonAislado{service: "notas", invitados: []string{strings.TrimPrefix(inv.URL, "http://")}}
	h := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "").Handler("")

	rec := pedir(t, h, "POST", "/mcp/notas", "", cuerpoInit)
	sid := rec.Header().Get(SessionHeader)
	if rec.Code != http.StatusOK || sid == "" {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	rec = pedir(t, h, "DELETE", "/mcp/notas", sid, "")
	if rec.Code != http.StatusBadGateway || rec.Body.Len() > 1<<10 {
		t.Fatalf("DELETE: %d, %d bytes; quería 502 sin el cuerpo", rec.Code, rec.Body.Len())
	}
	d.mu.Lock()
	n := len(d.maquinas)
	d.mu.Unlock()
	if n != 0 {
		t.Fatalf("quedan %d máquinas aisladas tras el DELETE", n)
	}
}
