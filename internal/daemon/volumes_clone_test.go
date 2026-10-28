package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

func postClone(s *Server, name, body string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/volumes/"+name+"/clone", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.routes().ServeHTTP(rr, req)
	return rr
}

// POST /volumes/{name}/clone: sin reflink y sin copy, 409 que explica; con
// copy, 200 y el volumen nuevo. En un host que sí clona, las dos dan 200 con
// el método de la plataforma.
func TestCloneVolumeHandler(t *testing.T) {
	s, root := servidorBlobs(t)
	vols := filepath.Join(root, "volumes")
	if err := os.MkdirAll(vols, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vols, "datos.ext4"), []byte("contenido"), 0o644); err != nil {
		t.Fatal(err)
	}

	info := s.mgr.CloneInfo()
	rr := postClone(s, "datos", `{"to":"rama"}`)
	if info.Method == "" {
		if rr.Code != http.StatusConflict {
			t.Fatalf("sin reflink: %d, quiero 409 (%s)", rr.Code, rr.Body)
		}
		if !strings.Contains(rr.Body.String(), "copy") {
			t.Errorf("el 409 debe decir cómo seguir: %s", rr.Body)
		}
		rr = postClone(s, "datos", `{"to":"rama","copy":true}`)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("clon: %d %s", rr.Code, rr.Body)
	}
	var res api.CloneVolumeResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Volume == nil || res.Volume.Name != "rama" || res.Method == "" {
		t.Fatalf("respuesta = %+v", res)
	}

	// Repetirlo choca con el que ya existe: 409, no 400.
	if rr := postClone(s, "datos", `{"to":"rama","copy":true}`); rr.Code != http.StatusConflict {
		t.Errorf("destino existente: %d, quiero 409 (%s)", rr.Code, rr.Body)
	}
	if rr := postClone(s, "datos", `{"to":"../x","copy":true}`); rr.Code != http.StatusBadRequest {
		t.Errorf("nombre inválido: %d, quiero 400", rr.Code)
	}
}

// GET /info anuncia la capacidad y trae la sonda.
func TestInfoTraeLaSondaDeClon(t *testing.T) {
	s, _ := servidorBlobs(t)
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/info", nil))
	var info api.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if !info.Has("volume-clone") {
		t.Error("falta la capacidad volume-clone")
	}
	if info.Clone == nil || len(info.Clone.Dirs) == 0 || info.Clone.Dirs[0].Dir != "volumes" {
		t.Fatalf("sonda = %+v", info.Clone)
	}
}
