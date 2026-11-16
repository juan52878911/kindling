package daemon

import (
	"net/http"
	"strings"
	"testing"
)

// Con política de autorización, publicar en el loopback del host es de
// admin, por las dos puertas: POST /machines/{ref}/publish y el campo publish
// de POST /machines. Quitar una publicación de lo suyo sigue abierto.
func TestPublicarEsDeAdmin(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	stubs := conStubs(s)
	const puerto = `{"ports":[{"host":0,"guest":5432}]}`

	rr := como(t, h, uidA, "POST", "/machines/svc-a/publish", puerto)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "publish is admin-only") {
		t.Fatalf("inquilino publica: %d %s", rr.Code, rr.Body)
	}
	rr = como(t, h, uidA, "POST", "/machines/svc-a/publish", `{"ports":[{"host":40123,"guest":5432}]}`)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("inquilino publica un puerto alto: %d %s", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidAdmin, "POST", "/machines/svc-a/publish", puerto); rr.Code == http.StatusForbidden {
		t.Fatalf("admin no puede publicar: %d %s", rr.Code, rr.Body)
	}
	rr = como(t, stubs, uidA, "POST", "/machines", `{"image":"min","publish":[{"host":0,"guest":80}]}`)
	if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "publish is admin-only") {
		t.Fatalf("inquilino crea con publish: %d %s", rr.Code, rr.Body)
	}
	if rr := como(t, stubs, uidA, "DELETE", "/machines/svc-a/publish", ""); rr.Code != 299 {
		t.Fatalf("inquilino quita lo suyo: %d %s", rr.Code, rr.Body)
	}
}
