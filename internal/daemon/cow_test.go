package daemon

import (
	"strings"
	"testing"
)

// POST /cow/store/grow es solo de admin (es espacio del host), y sin almacén
// lo dice en vez de hacer nada.
func TestGrowCoWStore(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	if rr := como(t, h, uidA, "POST", "/cow/store/grow", `{"add_mib":1024}`); rr.Code != 403 {
		t.Errorf("un inquilino llega: %d %s", rr.Code, rr.Body)
	}
	rr := como(t, h, uidAdmin, "POST", "/cow/store/grow", `{"add_mib":1024}`)
	if rr.Code != 400 || !strings.Contains(rr.Body.String(), "store") {
		t.Errorf("admin sin almacén: %d %s", rr.Code, rr.Body)
	}
	rr = como(t, h, uidAdmin, "POST", "/cow/store/grow", `{"size_mib":1024,"add_mib":1024}`)
	if rr.Code != 400 {
		t.Errorf("los dos a la vez: %d %s", rr.Code, rr.Body)
	}
	if !strings.Contains(strings.Join(Capabilities, " "), "cow-grow") {
		t.Error("sin la capacidad cow-grow")
	}
}
