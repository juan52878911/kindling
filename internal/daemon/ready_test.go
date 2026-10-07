package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

func TestEsperaDeQuery(t *testing.T) {
	casos := []struct {
		q      string
		quiero time.Duration
		mal    bool
	}{
		{"", 0, false},
		{"wait=30s", 30 * time.Second, false},
		{"wait=45", 45 * time.Second, false},
		{"wait=0", 0, false},
		{"wait=-1s", 0, true},
		{"wait=abc", 0, true},
		{"wait=12x", 0, true},
		{fmt.Sprintf("wait=%d", api.ReadyMaxWaitSeconds+1), 0, true},
	}
	for _, c := range casos {
		r := httptest.NewRequest("GET", "/machines/x/ready?"+c.q, nil)
		d, err := esperaDeQuery(r)
		if (err != nil) != c.mal || (!c.mal && d != c.quiero) {
			t.Errorf("?%s = %s, %v", c.q, d, err)
		}
	}
}

func TestEstadoDeListo(t *testing.T) {
	casos := map[error]int{
		fmt.Errorf("%w: x", machine.ErrNoMachine):  http.StatusNotFound,
		fmt.Errorf("%w: x", machine.ErrNotRunning): http.StatusConflict,
		errors.New("otra cosa"):                    http.StatusInternalServerError,
	}
	for err, quiero := range casos {
		if got := estadoDeListo(err); got != quiero {
			t.Errorf("estadoDeListo(%v) = %d, quiero %d", err, got, quiero)
		}
	}
}

func TestEscribirListo(t *testing.T) {
	espera := api.ReadyResult{ID: "x", Name: "x", Ready: api.ReadyWaiting, Detail: "booting"}
	casos := []struct {
		err    error
		quiero int
	}{
		{nil, 200},
		{fmt.Errorf("%w: x after 1s: booting", machine.ErrNotReady), 200},
		{fmt.Errorf("%w: x", machine.ErrNoMachine), 404},
		{fmt.Errorf("%w: x is stopped", machine.ErrNotRunning), 409},
	}
	for _, c := range casos {
		rr := httptest.NewRecorder()
		escribirListo(rr, espera, c.err)
		if rr.Code != c.quiero {
			t.Errorf("%v → %d, quiero %d", c.err, rr.Code, c.quiero)
			continue
		}
		var res api.ReadyResult
		if c.quiero == 200 && (json.Unmarshal(rr.Body.Bytes(), &res) != nil || res.Ready != api.ReadyWaiting || res.Detail != "booting") {
			t.Errorf("%v → cuerpo %s", c.err, rr.Body)
		}
	}
}

// Los códigos de las rutas, con un gestor de verdad: lo que no existe, lo
// que no está en marcha y un ?wait= mal formado.
func TestHandleReadyCodigos(t *testing.T) {
	root := t.TempDir()
	maquinas := []*api.Machine{{ID: "dddd000000000001", Name: "parada", State: api.StateStopped}}
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
		metodo, path string
		quiero       int
	}{
		{"GET", "/machines/nadie/ready", 404},
		{"GET", "/machines/parada/ready", 409},
		{"GET", "/machines/parada/ready?wait=nada", 400},
		{"GET", "/machines/parada/ready?wait=999999", 400},
		{"POST", "/machines/nadie/hooks", 404},
		{"POST", "/machines/parada/hooks", 409},
	} {
		if rr := call(t, h, c.metodo, c.path, ""); rr.Code != c.quiero {
			t.Errorf("%s %s = %d (%s), quiero %d", c.metodo, c.path, rr.Code, rr.Body, c.quiero)
		}
	}
}
