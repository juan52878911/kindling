package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/credproxy"
)

// servidorConMaquina: un daemon con una máquina congelada (svc), sin VMM.
func servidorConMaquina(t *testing.T) (*Server, string, string) {
	t.Helper()
	root := t.TempDir()
	const id = "c4c4c4c4c4c4c4c4c4"
	b, _ := json.Marshal([]*api.Machine{{ID: id, Name: "svc", State: api.StateWarm,
		Labels: map[string]string{api.LabelOwner: "a"}}})
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	bus := events.New()
	mgr, err := machine.NewManager(root, "", "", bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	return &Server{mgr: mgr, root: root, bus: bus}, root, id
}

func comoInquilino(r *http.Request, admin bool) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), claveRol{}, Rol{Tenant: "a", Admin: admin}))
}

// conLog recoge lo que el daemon escribe en su log durante el test.
func conLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	antes := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(antes) })
	return &buf
}

// Un inquilino no ve rutas del host en los errores de logs y credaudit; el
// admin sí, y el detalle queda en el log del daemon.
func TestErroresSinRutasParaElInquilino(t *testing.T) {
	s, root, id := servidorConMaquina(t)
	// Auditoría ilegible (un enlace: se abre con O_NOFOLLOW), donde sea que la
	// busque esta plataforma.
	for _, p := range []string{filepath.Join(root, "audit", id+".jsonl"), filepath.Join(root, "machines", id, credproxy.AuditFile)} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("/nada", p); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []struct {
		nombre, ruta string
		h            http.HandlerFunc
		code         int
	}{
		{"logs", "/machines/svc/logs", s.handleLogs, http.StatusNotFound},
		{"credaudit", "/machines/svc/credaudit", s.handleCredAudit, http.StatusInternalServerError},
	} {
		for _, admin := range []bool{false, true} {
			logd := conLog(t)
			r := comoInquilino(httptest.NewRequest("GET", c.ruta, nil), admin)
			r.SetPathValue("ref", "svc")
			rr := httptest.NewRecorder()
			c.h(rr, r)
			if rr.Code != c.code {
				t.Fatalf("%s (admin=%v) = %d %s, quería %d", c.nombre, admin, rr.Code, rr.Body, c.code)
			}
			ve := strings.Contains(rr.Body.String(), root)
			if ve != admin {
				t.Errorf("%s (admin=%v): ¿lleva la ruta? %v: %s", c.nombre, admin, ve, rr.Body)
			}
			if !admin && !strings.Contains(logd.String(), root) {
				t.Errorf("%s: el detalle no quedó en el log del daemon: %q", c.nombre, logd)
			}
		}
	}
}

// Lo mismo con la dirección interna del invitado en /guest.
func TestErrorDeGuestSinIPParaElInquilino(t *testing.T) {
	s, _, _ := servidorConMaquina(t)
	const addr = "127.0.0.1:1" // nadie escucha
	_, code, err := proxyGuest(context.Background(), addr, api.GuestRequest{Path: "/"},
		func(context.Context, time.Duration) error { return nil })
	if err == nil || !strings.Contains(err.Error(), addr) {
		t.Fatalf("el error de partida no lleva la dirección: %d %v", code, err)
	}
	for _, admin := range []bool{false, true} {
		logd := conLog(t)
		r := comoInquilino(httptest.NewRequest("POST", "/machines/svc/guest", nil), admin)
		rr := httptest.NewRecorder()
		s.failGuest(rr, r, code, err, addr, 8080, "svc")
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("admin=%v: %d", admin, rr.Code)
		}
		ve := strings.Contains(rr.Body.String(), "127.0.0.1")
		if ve != admin {
			t.Errorf("admin=%v: ¿lleva la IP? %v: %s", admin, ve, rr.Body)
		}
		if !admin {
			if !strings.Contains(rr.Body.String(), "connection refused") || !strings.Contains(rr.Body.String(), "8080") {
				t.Errorf("el inquilino pierde la causa: %s", rr.Body)
			}
			if !strings.Contains(logd.String(), addr) {
				t.Errorf("el detalle no quedó en el log: %q", logd)
			}
		}
	}
}
