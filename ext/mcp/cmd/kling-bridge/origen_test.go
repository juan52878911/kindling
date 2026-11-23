package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// B4: fuera de una microVM el puente no autentica, así que tiene que negarse
// a lo que una página web puede mandarle: un Host que no es loopback (DNS
// rebinding), un Origin ajeno y un POST que no es JSON (un formulario entre
// sitios), /reset incluido. Dentro de la microVM (PID 1) el gateway llega por
// la tap con la IP del invitado y no se le pone nada delante.
func TestPuenteSoloLocal(t *testing.T) {
	llego := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { llego++ })
	mux.HandleFunc("/reset", func(w http.ResponseWriter, r *http.Request) { llego++; w.WriteHeader(http.StatusNoContent) })

	pide := func(h http.Handler, method, ruta, host, origin, ctype string) int {
		t.Helper()
		cuerpo := ""
		if method == http.MethodPost {
			cuerpo = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
		}
		req := httptest.NewRequest(method, ruta, strings.NewReader(cuerpo))
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	fuera := manejadorDelPuente(mux, false, nil)
	for _, c := range []struct {
		nombre, method, ruta, host, origin, ctype string
		quiere                                    int
	}{
		{"host ajeno", http.MethodPost, "/mcp", "atacante.example:9100", "", "application/json", http.StatusForbidden},
		{"host ajeno en GET", http.MethodGet, "/mcp", "atacante.example", "", "", http.StatusForbidden},
		{"host ajeno en /reset", http.MethodPost, "/reset", "atacante.example", "", "application/json", http.StatusForbidden},
		{"origin ajeno", http.MethodPost, "/mcp", "127.0.0.1:9100", "https://atacante.example", "application/json", http.StatusForbidden},
		{"origin nulo", http.MethodPost, "/mcp", "127.0.0.1:9100", "null", "application/json", http.StatusForbidden},
		{"post de formulario", http.MethodPost, "/mcp", "127.0.0.1:9100", "", "text/plain", http.StatusUnsupportedMediaType},
		{"post sin content-type", http.MethodPost, "/mcp", "localhost:9100", "", "", http.StatusUnsupportedMediaType},
		{"reset de formulario", http.MethodPost, "/reset", "127.0.0.1:9100", "", "application/x-www-form-urlencoded", http.StatusUnsupportedMediaType},
		{"json en loopback", http.MethodPost, "/mcp", "127.0.0.1:9100", "", "application/json; charset=utf-8", http.StatusOK},
		{"json en localhost con origin local", http.MethodPost, "/mcp", "localhost:9100", "http://localhost:3000", "application/json", http.StatusOK},
		{"reset json en ::1", http.MethodPost, "/reset", "[::1]:9100", "", "application/json", http.StatusNoContent},
		{"get en loopback", http.MethodGet, "/mcp", "127.0.0.1:9100", "", "", http.StatusOK},
	} {
		if got := pide(fuera, c.method, c.ruta, c.host, c.origin, c.ctype); got != c.quiere {
			t.Errorf("%s: %d, quiero %d", c.nombre, got, c.quiere)
		}
	}

	// Lo rechazado no llega al manejador.
	llego = 0
	pide(fuera, http.MethodPost, "/reset", "atacante.example", "", "application/json")
	pide(fuera, http.MethodPost, "/reset", "127.0.0.1", "", "text/plain")
	if llego != 0 {
		t.Fatalf("%d peticiones rechazadas llegaron al puente", llego)
	}

	// Hosts añadidos a mano (KLING_BRIDGE_ALLOWED_HOSTS): el gateway que llega
	// por la LAN con el nombre o la IP de esta máquina.
	lan := manejadorDelPuente(mux, false, hostsPermitidos(" 192.168.1.20 , Mac.lan. ,"))
	if got := pide(lan, http.MethodPost, "/mcp", "192.168.1.20:9100", "", "application/json"); got != http.StatusOK {
		t.Errorf("host permitido a mano: %d", got)
	}
	if got := pide(lan, http.MethodPost, "/mcp", "mac.lan:9100", "", "application/json"); got != http.StatusOK {
		t.Errorf("nombre permitido a mano (mayúsculas y punto final): %d", got)
	}
	if got := pide(lan, http.MethodPost, "/mcp", "atacante.example", "", "application/json"); got != http.StatusForbidden {
		t.Errorf("host no permitido con lista: %d", got)
	}
	if got := pide(lan, http.MethodPost, "/mcp", "192.168.1.20:9100", "", "text/plain"); got != http.StatusUnsupportedMediaType {
		t.Errorf("la lista no quita la exigencia de JSON: %d", got)
	}
	if hostsPermitidos("") != nil {
		t.Errorf("sin variable no se añade nada")
	}

	// PID 1 (la microVM): sin filtro, el gateway llega con la IP del invitado.
	dentro := manejadorDelPuente(mux, true, nil)
	if got := pide(dentro, http.MethodPost, "/reset", "172.16.0.2:8080", "", ""); got != http.StatusNoContent {
		t.Errorf("dentro de la microVM: %d", got)
	}
}
