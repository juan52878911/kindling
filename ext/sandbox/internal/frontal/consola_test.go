package frontal

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/ext/sandbox/internal/consola"
	"github.com/juan52878911/kindling/ext/sandbox/internal/hosts"
)

func servidorConsola(t *testing.T) *Servidor {
	t.Helper()
	s := &Servidor{}
	s.rutas()
	return s
}

func TestConsolaSirveRecursosConCabeceras(t *testing.T) {
	s := servidorConsola(t)
	for _, p := range []string{"/console/", "/console/app.js", "/console/app.css"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 {
			t.Fatalf("%s: %d", p, w.Code)
		}
		h := w.Header()
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") ||
			!strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", p, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") == "" {
			t.Errorf("%s: faltan cabeceras", p)
		}
	}
}

func TestConsola404YSoloGET(t *testing.T) {
	s := servidorConsola(t)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("GET", "/console/no-existe", nil))
	if w.Code != http.StatusNotFound {
		t.Errorf("ruta inexistente: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest("POST", "/console/", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestConsolaSinRutasFueraDePrefijo(t *testing.T) {
	h := consola.Handler()
	for _, p := range []string{"/", "/consola", "/v1/health", "/other/app.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code == 200 {
			t.Errorf("%s no debería servirse", p)
		}
	}
}

func TestConsolaJSSinInsercionPeligrosa(t *testing.T) {
	b, err := fs.ReadFile(consola.Recursos(), "app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, mal := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "document.write", "new Function"} {
		if strings.Contains(string(b), mal) {
			t.Errorf("app.js contiene %q", mal)
		}
	}
}

func TestConsolaHTMLNoCargaDeFuera(t *testing.T) {
	b, err := fs.ReadFile(consola.Recursos(), "index.html")
	if err != nil {
		t.Fatal(err)
	}
	if m := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?\s*(https?:)?//`).FindString(string(b)); m != "" {
		t.Errorf("recurso externo: %q", m)
	}
	if strings.Contains(string(b), "<script>") || strings.Contains(string(b), "style=") {
		t.Error("script o estilo en línea: la CSP lo bloquearía")
	}
}

// Con SinConsola la ruta no se registra: /console/ contesta 404.
func TestConsolaApagada(t *testing.T) {
	s := &Servidor{sinConsola: true}
	s.rutas()
	for _, p := range []string{"/console/", "/console/app.js"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 404 {
			t.Errorf("%s con la consola apagada: %d", p, w.Code)
		}
	}
	// Por el constructor también: Opciones.SinConsola llega al servidor.
	n, err := Nuevo(Opciones{Hosts: hosts.Nuevo(nil), Tenants: []Tenant{{Nombre: "x", Token: tokenAlice}}, SinConsola: true})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	n.ServeHTTP(w, httptest.NewRequest("GET", "/console/", nil))
	if w.Code != 404 {
		t.Errorf("Nuevo con SinConsola: %d", w.Code)
	}
}
