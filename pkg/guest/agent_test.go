package guest

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Las rutas que el daemon usa con cualquier invitado tienen que existir, y
// /exec NO sin el parámetro del kernel: en una máquina de servicio ni se registra.
func TestRegisterRutasDelAgente(t *testing.T) {
	if ExecEnabled() {
		t.Skip("este anfitrión arrancó con kling.exec=1: la prueba de ausencia no aplica")
	}
	a := &Agent{Reaper: DefaultReaper, Volumes: &Volumes{}}
	mux := http.NewServeMux()
	a.Register(mux)

	casos := []struct {
		metodo, ruta string
		quiero       int
	}{
		{"GET", "/healthz", 200},
		{"POST", "/volume/sync", 204},
		{"POST", "/volume/release", 204},
		{"POST", "/volume/acquire", 204},
		{"POST", "/exec", 404},
	}
	for _, c := range casos {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(c.metodo, c.ruta, nil))
		if rr.Code != c.quiero {
			t.Errorf("%s %s: %d, quería %d", c.metodo, c.ruta, rr.Code, c.quiero)
		}
	}
}
