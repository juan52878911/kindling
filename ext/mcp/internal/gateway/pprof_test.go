package gateway

import (
	"github.com/juan52878911/kindling/pkg/scheduler"
	"net/http"
	"net/http/httptest"
	"testing"
)

// pprof tiene que quedar DETRÁS del token, no al lado de /healthz.
//
// Auth envuelve el mux entero, así que sale gratis — pero es justo el tipo de
// cosa que alguien "arregla" añadiéndole una exención cuando le estorba, y un
// volcado de goroutines o la línea de comandos completa no son cosas que deba
// poder pedir quien no tenga el token.
func TestPprofQuedaDetrasDelToken(t *testing.T) {
	g := &Gateway{Scheduler: &scheduler.Scheduler{}, PprofEnabled: true}
	h := g.Handler("s3cr3t")

	rutas := []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/heap",
		"/debug/pprof/goroutine",
	}
	for _, ruta := range rutas {
		r := httptest.NewRequest(http.MethodGet, ruta, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s sin token devolvió %d, want 401", ruta, w.Code)
		}
	}

	// Y con el token sí responde: el guard no puede ser que esté roto.
	r := httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil)
	r.Header.Set("Authorization", "Bearer s3cr3t")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code == http.StatusUnauthorized {
		t.Error("con el token correcto pprof debería responder")
	}
}

// Sin -pprof no se registra nada: el 404 confirma que el flag apagado no deja
// las rutas puestas esperando a que alguien acierte el token.
func TestSinPprofNoHayRutas(t *testing.T) {
	g := &Gateway{Scheduler: &scheduler.Scheduler{}, PprofEnabled: false}
	r := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	r.Header.Set("Authorization", "Bearer s3cr3t")
	w := httptest.NewRecorder()
	g.Handler("s3cr3t").ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("con -pprof apagado debería ser 404, fue %d", w.Code)
	}
}
