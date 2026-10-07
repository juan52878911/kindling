package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// Las operaciones de ciclo de vida distinguen "no existe" (404) de "su estado
// no lo admite" (409). Antes las dos eran un 400, el mismo código que una
// petición mal formada, y quien llamaba solo podía distinguirlas leyendo el
// texto.
func TestCodigosCicloDeVida(t *testing.T) {
	root := t.TempDir()
	maquinas := []*api.Machine{
		{ID: "cccc000000000001", Name: "parada", State: api.StateStopped},
		{ID: "cccc000000000002", Name: "dormida", State: api.StateWarm},
	}
	b, _ := json.Marshal(maquinas)
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	bus := events.New()
	mgr, err := machine.NewManager(root, "", "", bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	s := &Server{mgr: mgr, root: root, bus: bus, store: &store{dir: filepath.Join(root, "store")}}
	h := s.routes()

	for _, c := range []struct {
		method, path string
		want         int
	}{
		{"POST", "/machines/nadie/freeze", 404},
		{"POST", "/machines/nadie/pause", 404},
		{"POST", "/machines/nadie/thaw", 404},
		{"POST", "/machines/nadie/stop", 404},
		{"DELETE", "/machines/nadie", 404},
		{"POST", "/machines/parada/freeze", 409},
		{"POST", "/machines/parada/pause", 409},
		{"POST", "/machines/parada/thaw", 409},
		{"POST", "/machines/dormida/pause", 409},
		{"POST", "/machines/nadie/start", 404},
		{"POST", "/machines/dormida/start", 409},
		{"POST", "/machines/dormida/squeeze", 409},
		{"POST", "/machines/parada/mmds", 409},
	} {
		body := ""
		if c.path == "/machines/parada/mmds" {
			body = "{}"
		}
		rr := call(t, h, c.method, c.path, body)
		if rr.Code != c.want {
			t.Errorf("%s %s = %d (%s), quería %d", c.method, c.path, rr.Code, rr.Body, c.want)
		}
	}
	// El texto no cambia: hay clientes (y personas) que lo leen.
	rr := call(t, h, "POST", "/machines/nadie/thaw", "")
	var e api.Error
	if json.Unmarshal(rr.Body.Bytes(), &e); e.Message != `machine "nadie" doesn't exist` {
		t.Errorf("mensaje: %q", e.Message)
	}
	if !api.IsNotFound(&api.StatusError{Code: rr.Code, Message: e.Message}) {
		t.Error("un cliente no lo reconoce como 404")
	}
}
