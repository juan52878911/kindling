package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// Borrar una imagen o un snapshot contesta con el código de su causa: 400 un
// nombre que no vale, 404 lo que no existe, 409 lo que está en uso. Antes una
// imagen era 409 para todo y un snapshot 400 para todo.
func TestCodigosDeBorrarImagenYSnapshot(t *testing.T) {
	s, h := testServer(t)
	if err := os.MkdirAll(filepath.Join(s.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, "images", "usada.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, "snapshots", "dorado")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(`{"name":"dorado","image":"usada"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		path string
		want int
	}{
		{"/images/nada", http.StatusNotFound},
		{"/images/usada", http.StatusConflict},
		{"/snapshots/nada", http.StatusNotFound},
	} {
		if rr := call(t, h, "DELETE", c.path, ""); rr.Code != c.want {
			t.Errorf("DELETE %s = %d (%s), quería %d", c.path, rr.Code, strings.TrimSpace(rr.Body.String()), c.want)
		}
	}

	// Un nombre inválido no pasa el filtro de rutas; el handler, por su lado,
	// también contesta 400.
	for _, c := range []struct {
		handler func(http.ResponseWriter, *http.Request)
		path    string
	}{
		{s.handleRemoveImage, "/images/x"},
		{s.handleRemoveSnapshot, "/snapshots/x"},
	} {
		req := httptest.NewRequest(http.MethodDelete, c.path, nil)
		req.SetPathValue("name", "-malo")
		rr := httptest.NewRecorder()
		c.handler(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s con nombre inválido = %d, quería 400", c.path, rr.Code)
		}
	}

	// El snapshot sigue ahí y el mensaje no cambió.
	rr := call(t, h, "DELETE", "/images/usada", "")
	if !strings.Contains(rr.Body.String(), `is used by the golden snapshot`) {
		t.Errorf("mensaje cambiado: %s", rr.Body)
	}
	// Las anotaciones y el fondo también dicen 404 de un snapshot que no existe.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/snapshots/nada", ""},
		{"PUT", "/snapshots/nada/annotations/k", `1`},
		{"DELETE", "/snapshots/nada/annotations/k", ""},
		{"GET", "/snapshots/nada/pool", ""},
		{"PUT", "/snapshots/nada/pool", `{"size":1}`},
	} {
		if rr := call(t, h, c.method, c.path, c.body); rr.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d (%s), quería 404", c.method, c.path, rr.Code, strings.TrimSpace(rr.Body.String()))
		}
	}
}

// Un cuerpo que pasa del tope es un 413 que lo dice, no un 400 "unexpected
// EOF" de un JSON cortado a mitad.
func TestCuerposGrandesSon413(t *testing.T) {
	_, h := testServer(t)
	grande := func(n int) string { return `{"x":"` + strings.Repeat("a", n) + `"}` }
	for _, c := range []struct {
		method, path, body string
	}{
		{"POST", "/machines/nada/exec", grande(2*api.ExecMaxStdin + 64<<10)},
		{"POST", "/machines/nada/resize", grande(4 << 10)},
		{"POST", "/machines/nada/mmds", grande(api.MaxMMDSBytes)},
		{"POST", "/machines/nada/credentials", grande(256 << 10)},
		{"PUT", "/snapshots/nada/credentials", grande(256 << 10)},
		{"PUT", "/snapshots/nada/annotations/k", grande(api.MaxValueBytes)},
		{"PUT", "/store/ns/k", grande(api.MaxValueBytes)},
	} {
		rr := call(t, h, c.method, c.path, c.body)
		if rr.Code == http.StatusNotFound || rr.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s %s: la ruta no existe (%d); revisa el test", c.method, c.path, rr.Code)
		}
		if rr.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("%s %s = %d (%s), quería 413", c.method, c.path, rr.Code, strings.TrimSpace(rr.Body.String()))
		}
		if strings.Contains(rr.Body.String(), "response exceeds") {
			t.Errorf("%s %s: el mensaje habla de respuesta en una petición: %s", c.method, c.path, rr.Body)
		}
	}
	// Un JSON roto sigue siendo 400.
	if rr := call(t, h, "POST", "/machines/nada/resize", `{"mem_mib":`); rr.Code != http.StatusBadRequest {
		t.Errorf("JSON roto = %d, quería 400", rr.Code)
	}
}

// -max-output por encima de los 8 MiB por defecto llega entero: daemon y
// agente de verdad, con el tope sacado de la petición como en handleExec.
func TestExecMaxOutputPasaDeOchoMiB(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/exec/stream", guest.StreamExecHandler(nil))
	g := httptest.NewServer(mux)
	defer g.Close()

	const quiero = 9 << 20
	req := api.ExecRequest{
		Cmd:            []string{"head", "-c", "9437184", "/dev/zero"},
		MaxOutputBytes: api.ExecMaxOutput,
	}
	_, maxOut, err := req.Limits()
	if err != nil {
		t.Fatal(err)
	}
	body, code, err := openGuestExec(context.Background(), g.URL, req)
	if err != nil {
		t.Fatalf("%d %v", code, err)
	}
	defer body.Close()
	res, err := aggregateExec(readExecEvents(body, maxOut))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Stdout) != quiero || res.Truncated {
		t.Fatalf("stdout = %d bytes (truncated=%v), quería %d", len(res.Stdout), res.Truncated, quiero)
	}
}

// M13: POST /machines contesta 4xx a lo que está mal en la petición, no 500
// como si el host fallara.
func TestCodigosDeCrearMaquina(t *testing.T) {
	// En Linux, sin jailer ni usuario sin privilegios, Run se niega antes de
	// mirar la petición: aquí se mide lo que viene después.
	t.Setenv("KLING_JAILER", "0")
	s, h := testServer(t)
	if err := os.MkdirAll(filepath.Join(s.root, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.root, "images", "hay.ext4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"image":"nope"}`, http.StatusNotFound},
		{`{"image":"hay","on_ttl":"explode"}`, http.StatusBadRequest},
		{`{"image":"hay","disk_mib":1}`, http.StatusBadRequest},
		{`{"image":"hay","mem_mib":512,"mem_max_mib":256}`, http.StatusBadRequest},
		{`{"image":"../x"}`, http.StatusBadRequest},
		// El kernel que falta es del host.
		{`{"image":"hay"}`, http.StatusInternalServerError},
		{`{"image":"hay","from":"nada"}`, http.StatusNotFound},
	} {
		rr := call(t, h, "POST", "/machines", c.body)
		if rr.Code != c.want {
			t.Errorf("POST /machines %s = %d (%s), quería %d", c.body, rr.Code, strings.TrimSpace(rr.Body.String()), c.want)
		}
	}
}

// M13: en las operaciones sobre una máquina que existe, lo que falla en el host
// (aquí, arrancar sin kernel) es 500 y no 400; lo que está mal en la petición
// sigue siendo 400.
func TestCodigosCicloHostYPeticion(t *testing.T) {
	root := t.TempDir()
	b, _ := json.Marshal([]*api.Machine{{ID: "cccc000000000003", Name: "parada", State: api.StateStopped, Image: "hay"}})
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
		method, path, body string
		want               int
	}{
		{"POST", "/machines/parada/start", "", http.StatusInternalServerError},
		{"POST", "/machines/parada/credentials", `{"credentials":[]}`, http.StatusBadRequest},
		{"DELETE", "/machines/parada/credentials/mal-env", "", http.StatusBadRequest},
	} {
		rr := call(t, h, c.method, c.path, c.body)
		if rr.Code != c.want {
			t.Errorf("%s %s = %d (%s), quería %d", c.method, c.path, rr.Code, strings.TrimSpace(rr.Body.String()), c.want)
		}
	}
}
