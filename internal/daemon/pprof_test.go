package daemon

import (
	"strings"
	"testing"
)

// /debug/pprof es solo del admin: un volcado de goroutines enseña rutas y
// argumentos de todas las máquinas.
func TestPprofSoloAdmin(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	rr := como(t, h, uidAdmin, "GET", "/debug/pprof/goroutine?debug=1", "")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "goroutine profile:") {
		t.Fatalf("admin: %d\n%.300s", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidAdmin, "GET", "/debug/pprof/", ""); rr.Code != 200 || !strings.Contains(rr.Body.String(), "heap") {
		t.Errorf("índice: %d\n%.300s", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidAdmin, "GET", "/debug/pprof/cmdline", ""); rr.Code != 200 {
		t.Errorf("cmdline: %d", rr.Code)
	}
	for _, uid := range []int{uidA, uidNadie} {
		if rr := como(t, h, uid, "GET", "/debug/pprof/goroutine?debug=1", ""); rr.Code != 403 {
			t.Errorf("uid %d: %d, quería 403", uid, rr.Code)
		}
	}
}
