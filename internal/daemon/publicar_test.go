package daemon

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// POST/DELETE /machines/{ref}/publish y GET /publish, con una máquina
// congelada (publicar no la despierta: lo hace la primera conexión).
func TestRutasPublicar(t *testing.T) {
	root := t.TempDir()
	seed := []*api.Machine{{ID: "p1p1p1p1p1p1p1p1p1", Name: "db", State: api.StateWarm,
		Labels: map[string]string{api.LabelPorts: "5432"}}}
	b, _ := json.Marshal(seed)
	if err := os.WriteFile(filepath.Join(root, "state.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	mgr, err := machine.NewManager(root, "", "", events.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mgr.Close)
	s := &Server{mgr: mgr, root: root}
	h := s.routes()
	if !slices.Contains(Capabilities, api.CapabilityPublish) {
		t.Fatal("no anuncia publish")
	}

	if rr := call(t, h, "POST", "/machines/nada/publish", `{"ports":[{"host":0,"guest":80}]}`); rr.Code != http.StatusNotFound {
		t.Fatalf("máquina que no existe: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "POST", "/machines/db/publish", `{"ports":[{"host":0,"guest":8080}]}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("puerto del agente: %d %s", rr.Code, rr.Body)
	}
	rr := call(t, h, "POST", "/machines/db/publish", `{"ports":[{"host":0,"guest":5432}]}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("publish: %d %s", rr.Code, rr.Body)
	}
	var pub api.Publication
	if err := json.Unmarshal(rr.Body.Bytes(), &pub); err != nil || len(pub.Ports) != 1 || pub.Ports[0].Host == 0 {
		t.Fatalf("publication = %+v, %v", pub, err)
	}
	h1 := pub.Ports[0].Host
	if _, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(h1)); err == nil {
		t.Fatal("el puerto publicado está libre")
	}
	rr = call(t, h, "GET", "/publish", "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"name":"db"`) {
		t.Fatalf("GET /publish: %d %s", rr.Code, rr.Body)
	}
	if rr := call(t, h, "DELETE", "/machines/db/publish?host="+strconv.Itoa(h1), ""); rr.Code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", rr.Code, rr.Body)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(h1))
	if err != nil {
		t.Fatalf("tras DELETE el puerto sigue tomado: %v", err)
	}
	ln.Close()
	if rr := call(t, h, "GET", "/publish", ""); strings.TrimSpace(rr.Body.String()) != "[]" {
		t.Fatalf("GET /publish tras DELETE: %s", rr.Body)
	}
}

// RunRequest.Publish con un puerto ocupado falla ANTES de crear la máquina.
func TestRunConPuertoOcupadoNoCreaNada(t *testing.T) {
	root := t.TempDir()
	mgr := nuevoManager(t, root)
	s := &Server{mgr: mgr, root: root}
	ocupado, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ocupado.Close()
	p := ocupado.Addr().(*net.TCPAddr).Port
	cuerpo := `{"name":"x","image":"no-existe","publish":[{"host":` + strconv.Itoa(p) + `,"guest":80}]}`
	rr := call(t, s.routes(), "POST", "/machines", cuerpo)
	if rr.Code != http.StatusConflict || !strings.Contains(rr.Body.String(), "in use") {
		t.Fatalf("POST /machines = %d %s", rr.Code, rr.Body)
	}
	if n := len(mgr.List()); n != 0 {
		t.Fatalf("quedaron %d máquinas", n)
	}
}
