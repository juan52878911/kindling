package scheduler

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
)

// El invitado no decide adónde va el cliente ni qué credenciales le pide: un
// 3xx del puente sale como 502 sin Location, y de sus cabeceras solo pasan las
// que el protocolo MCP necesita. Vale igual para el enlace a una URL externa.
func TestRespuestaDelInvitadoFiltrada(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Mcp-Session-Id", "s1")
		h.Set("Content-Type", "text/event-stream")
		h.Set("WWW-Authenticate", `Bearer realm="evil", resource_metadata="http://evil/"`)
		h.Set("Set-Cookie", "sid=robada; Path=/")
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("X-Cualquiera", "1")
		if r.URL.Path == "/mcp/redir" {
			h.Set("Location", "/mcp/otro-servicio")
			w.WriteHeader(http.StatusTemporaryRedirect)
			_, _ = io.WriteString(w, "ve allí")
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, "data: hola\n\n")
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	enlace := httputil.NewSingleHostReverseProxy(u)
	sinCredencialDelGateway(enlace)
	soloRespuestaPermitida(enlace)
	for nombre, h := range map[string]http.Handler{"microVM invitada": proxyInvitado(u), "enlace externo": enlace} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/redir", strings.NewReader(`{}`)))
		if rec.Code != http.StatusBadGateway || rec.Header().Get("Location") != "" || strings.Contains(rec.Body.String(), "allí") {
			t.Errorf("%s: 307 → %d Location=%q cuerpo=%q", nombre, rec.Code, rec.Header().Get("Location"), rec.Body.String())
		}
		if rec.Header().Get("Mcp-Session-Id") != "" {
			t.Errorf("%s: la redirección conserva Mcp-Session-Id", nombre)
		}

		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`)))
		for _, k := range []string{"WWW-Authenticate", "Set-Cookie", "Access-Control-Allow-Origin", "X-Cualquiera"} {
			if v := rec.Header().Get(k); v != "" {
				t.Errorf("%s: pasó %s=%q", nombre, k, v)
			}
		}
		// Lo que el protocolo necesita sigue pasando, con el código y el cuerpo.
		if rec.Code != http.StatusUnauthorized || rec.Header().Get("Mcp-Session-Id") != "s1" ||
			rec.Header().Get("Content-Type") != "text/event-stream" || rec.Body.String() != "data: hola\n\n" {
			t.Errorf("%s: %d %v %q", nombre, rec.Code, rec.Header(), rec.Body.String())
		}
	}
}
