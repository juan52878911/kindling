package aigw

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// En TCP sin token, una petición dirigida a otro nombre o venida de otro
// origen recibe 403; en el socket Unix (Host cualquiera) no se mira.
func TestServeSinTokenSoloLoopback(t *testing.T) {
	g, _, _ := newTestGateway(t, nil)
	pedir := func(h http.Handler, host, origin string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/admin/reload", nil)
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	tcp := g.ServeHandler("", true)
	for _, c := range []struct{ host, origin string }{
		{"ajeno.example:8090", ""},
		{"127.0.0.1:8090", "http://ajeno.example"},
	} {
		if code := pedir(tcp, c.host, c.origin); code != http.StatusForbidden {
			t.Errorf("tcp Host %q Origin %q: %d, want 403", c.host, c.origin, code)
		}
	}
	if code := pedir(tcp, "127.0.0.1:8090", "http://localhost:8090"); code == http.StatusForbidden {
		t.Errorf("tcp loopback: 403")
	}
	if code := pedir(g.ServeHandler("", false), "ai", ""); code == http.StatusForbidden {
		t.Errorf("socket: 403")
	}
	// Con token manda la autenticación, no el Host.
	if code := pedir(g.ServeHandler("secreto", true), "ajeno.example", ""); code != http.StatusUnauthorized {
		t.Errorf("con token sin Authorization: %d, want 401", code)
	}
}

// Un POST que no es JSON (lo que un formulario puede mandar sin preguntar)
// recibe 415 antes de llegar a la ruta.
func TestPostSinJSON415(t *testing.T) {
	g, _, _ := newTestGateway(t, nil)
	h := g.Handler("")
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/feedback", strings.NewReader(`{}`))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: %d, want 415", ct, rec.Code)
		}
	}
	rec := do(t, h, http.MethodPost, "/v1/admin/reload", "", nil)
	if rec.Code == http.StatusUnsupportedMediaType {
		t.Errorf("application/json: 415")
	}
}

// Un cliente que pide streaming y deja de leer no retiene la réplica: la
// escritura vence y la réplica se suelta.
func TestProxyClienteQueNoLeeSueltaLaReplica(t *testing.T) {
	antes := plazoEscritura
	plazoEscritura = 200 * time.Millisecond
	t.Cleanup(func() { plazoEscritura = antes })

	g, ll, reps := newTestGateway(t, nil)
	// ~20 MiB de respuesta: más de lo que cabe en los búferes de los sockets.
	ll.set(strings.Repeat(strings.Repeat("x", 1000)+" ", 20000))
	srv := httptest.NewServer(g.Handler(""))
	t.Cleanup(srv.Close)

	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	body := `{"model":"smol","stream":true,"messages":[{"role":"user","content":"hola"}]}`
	fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	// Y no se lee nada.
	limite := time.Now().Add(10 * time.Second)
	for reps.released.Load() == 0 {
		if time.Now().After(limite) {
			t.Fatalf("la réplica sigue retenida por un cliente que no lee (acquired=%d)", reps.acquired.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
}
