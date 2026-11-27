package daemon

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// POST /snapshots/{name}/warm: 404 si el dorado no existe y 200 con el
// resultado si existe, con cuerpo o sin él (C9-10). Antes, 501 siempre.
func TestWarmSnapshot404Y200(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	if rr := como(t, h, uidAdmin, "POST", "/snapshots/nada/warm", `{}`); rr.Code != http.StatusNotFound {
		t.Fatalf("dorado que no existe: %d %s", rr.Code, rr.Body)
	}
	for _, cuerpo := range []string{`{}`, ``, `{"wait":true}`} {
		rr := como(t, h, uidAdmin, "POST", "/snapshots/snap-a/warm", cuerpo)
		if rr.Code != http.StatusOK {
			t.Fatalf("cuerpo %q: %d %s", cuerpo, rr.Code, rr.Body)
		}
		var res api.WarmSnapshotResult
		if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
			t.Fatalf("cuerpo %q: respuesta %s: %v", cuerpo, rr.Body, err)
		}
		if res.Pinned {
			t.Errorf("cuerpo %q: anclado sin pedirlo", cuerpo)
		}
	}
	// El inquilino calienta lo suyo, sin anclar.
	if rr := como(t, h, uidA, "POST", "/snapshots/snap-a/warm", `{}`); rr.Code != http.StatusOK {
		t.Fatalf("inquilino calienta lo suyo: %d %s", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidAdmin, "POST", "/snapshots/snap-a/warm", `{"wait":`); rr.Code != http.StatusBadRequest {
		t.Fatalf("cuerpo roto: %d %s", rr.Code, rr.Body)
	}
}

// Anclar (y soltar) es RAM no recuperable del host: solo administradores. Un
// inquilino recibe 403 aunque el dorado sea suyo. Donde no se puede anclar
// (macOS), 501.
func TestAnclarExigeAdmin(t *testing.T) {
	s := servidorAuthz(t, politicaDePrueba(t))
	h := s.routes()
	for _, cuerpo := range []string{`{"pin":true}`, `{"pin":false}`} {
		rr := como(t, h, uidA, "POST", "/snapshots/snap-a/warm", cuerpo)
		if rr.Code != http.StatusForbidden || !strings.Contains(rr.Body.String(), "admin") {
			t.Fatalf("inquilino con %s: %d %s", cuerpo, rr.Code, rr.Body)
		}
	}
	rr := como(t, h, uidAdmin, "POST", "/snapshots/snap-a/warm", `{"pin":true}`)
	if rr.Code == http.StatusForbidden {
		t.Fatalf("admin no puede anclar: %d %s", rr.Code, rr.Body)
	}
	if runtime.GOOS != "linux" && rr.Code != http.StatusNotImplemented {
		t.Fatalf("anclar fuera de Linux: %d %s, quería 501", rr.Code, rr.Body)
	}
	if rr := como(t, h, uidAdmin, "POST", "/snapshots/snap-a/warm", `{"pin":false}`); rr.Code != http.StatusOK {
		t.Fatalf("admin suelta: %d %s", rr.Code, rr.Body)
	}
}
