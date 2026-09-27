package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// POST /sandboxes/{ref}/fork. El Manager es el de verdad, con las máquinas
// sembradas en state.json antes de arrancarlo (como TestRenewDeMaquina): sin
// KVM no hay ninguna running, así que se prueba lo que decide el handler
// —qué es un sandbox, qué petición vale, qué código sale de cada error— y que
// nada de eso llegue a crear un snapshot. El flujo completo está en
// internal/machine/fork_test.go.
func TestForkSandboxHandler(t *testing.T) {
	root := t.TempDir()
	hace1m := time.Now().Add(-time.Minute)
	sembradas := []*api.Machine{
		{ID: "a1a1a1a1a1a1a1a1", Name: "svc", State: api.StateWarm,
			Labels: map[string]string{api.LabelService: "svc"}, TTLSeconds: 120, TTLAt: &hace1m},
		{ID: "b2b2b2b2b2b2b2b2", Name: "caja", State: api.StateWarm, OnTTL: api.OnTTLRemove,
			Labels: map[string]string{api.LabelKind: api.KindSandbox}, TTLSeconds: 120, TTLAt: &hace1m},
	}
	b, _ := json.Marshal(sembradas)
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	bus := events.New()
	mgr, err := machine.NewManager(root, "", "", bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	h := (&Server{mgr: mgr, root: root, bus: bus}).routes()

	cases := []struct {
		name, path, body string
		want             int
		msg              string
	}{
		{"no existe", "/sandboxes/nada/fork", "", 404, "no sandbox"},
		// Solo sandboxes: una máquina de servicio no se ramifica por aquí.
		{"no es sandbox", "/sandboxes/svc/fork", "", 404, "no sandbox"},
		{"demasiadas", "/sandboxes/caja/fork", fmt.Sprintf(`{"count":%d}`, api.ForkMax+1), 400, "count"},
		{"negativo", "/sandboxes/caja/fork", `{"count":-2}`, 400, "count"},
		{"ttl", "/sandboxes/caja/fork", `{"ttl_seconds":999999}`, 400, "ttl_seconds"},
		{"on_ttl", "/sandboxes/caja/fork", `{"on_ttl":"explode"}`, 400, "on_ttl"},
		{"json roto", "/sandboxes/caja/fork", `{`, 400, ""},
		// Válida, pero el sandbox duerme: se dice, y no se toca nada.
		{"dormido", "/sandboxes/caja/fork", `{"count":2}`, 409, "thaw it first"},
		{"dormido sin cuerpo", "/sandboxes/caja/fork", "", 409, "running"},
	}
	for _, c := range cases {
		rr := call(t, h, "POST", c.path, c.body)
		if rr.Code != c.want || !strings.Contains(rr.Body.String(), c.msg) {
			t.Errorf("%s: POST %s %s = %d %s, quería %d con %q",
				c.name, c.path, c.body, rr.Code, strings.TrimSpace(rr.Body.String()), c.want, c.msg)
		}
	}
	if dirs, _ := filepath.Glob(filepath.Join(root, "snapshots", "*")); len(dirs) != 0 {
		t.Errorf("peticiones rechazadas dejaron snapshots: %v", dirs)
	}
	if mc, _ := mgr.Get("caja"); mc.State != api.StateWarm {
		t.Errorf("el sandbox cambió de estado: %s", mc.State)
	}
}

// forkStatus: los errores de la máquina de origen son 409 (el cliente puede
// hacer algo: descongelarla, quitarle el volumen), no 500.
func TestForkStatus(t *testing.T) {
	casos := []struct {
		err  error
		want int
	}{
		{fmt.Errorf("x: %w", machine.ErrNoMachine), http.StatusNotFound},
		{fmt.Errorf("x: %w", machine.ErrNotRunning), http.StatusConflict},
		{fmt.Errorf("x: %w", machine.ErrFork), http.StatusConflict},
		{fmt.Errorf("forking caja: %w", machine.ErrSharesCommit), http.StatusConflict},
		{fmt.Errorf("fork 1 of 2: %w", machine.ErrExecNotInSnapshot), http.StatusConflict},
		{fmt.Errorf("no space left on device"), http.StatusInternalServerError},
	}
	for _, c := range casos {
		if got := forkStatus(c.err); got != c.want {
			t.Errorf("forkStatus(%v) = %d, quería %d", c.err, got, c.want)
		}
	}
}

// El cliente habla con la ruta que sirve el daemon, y la capacidad se anuncia.
func TestForkSandboxClienteYCapacidad(t *testing.T) {
	has := false
	for _, c := range Capabilities {
		has = has || c == "fork"
	}
	if !has {
		t.Error(`la capacidad "fork" no se anuncia en GET /info`)
	}
	_, h := testServer(t)
	c := clientePara(t, h)
	_, err := c.ForkSandbox(t.Context(), "nada", api.ForkRequest{Count: 2})
	var se *api.StatusError
	if !errors.As(err, &se) || se.Code != http.StatusNotFound || !strings.Contains(se.Message, "no sandbox") {
		t.Fatalf("ForkSandbox de algo que no existe: %v", err)
	}
}

// clientePara sirve h en un socket unix y devuelve un api.Client que le habla.
// En /tmp y no en t.TempDir(): sun_path no admite rutas largas.
func clientePara(t *testing.T, h http.Handler) *api.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kfk")
	if err != nil {
		t.Skipf("sin /tmp para un socket unix: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede escuchar en un socket unix: %v", err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return api.NewClient(sock)
}
