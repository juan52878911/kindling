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

func pedir(s *Server, metodo, url, cuerpo string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(metodo, url, strings.NewReader(cuerpo)))
	return rr
}

// Las rutas de snapshots de volumen, con sus códigos: 400 nombre, 404 no
// existe, 409 conflicto; y el ciclo snapshot → restore → rm.
func TestRutasDeSnapshotsDeVolumen(t *testing.T) {
	s, root := servidorBlobs(t)
	vols := filepath.Join(root, "volumes")
	if err := os.MkdirAll(vols, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(vols, "datos.ext4"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if rr := pedir(s, http.MethodPost, "/volumes/datos/snapshots", `{"name":"uno"}`); rr.Code != http.StatusOK {
		t.Fatalf("snapshot = %d %s", rr.Code, rr.Body)
	} else {
		var snap api.VolumeSnapshot
		if err := json.Unmarshal(rr.Body.Bytes(), &snap); err != nil || snap.Name != "uno" || snap.Mode == "" {
			t.Errorf("respuesta: %+v %v", snap, err)
		}
	}
	for _, c := range []struct {
		metodo, url, cuerpo string
		code                int
	}{
		{http.MethodPost, "/volumes/datos/snapshots", `{"name":"uno"}`, http.StatusConflict},
		{http.MethodPost, "/volumes/datos/snapshots", `{"name":"undo"}`, http.StatusBadRequest},
		{http.MethodPost, "/volumes/datos/snapshots", `{"name":"Mal"}`, http.StatusBadRequest},
		{http.MethodPost, "/volumes/no-existe/snapshots", `{}`, http.StatusNotFound},
		{http.MethodGet, "/volumes/no-existe/snapshots", ``, http.StatusNotFound},
		{http.MethodPost, "/volumes/datos/restore", `{"snapshot":"otro"}`, http.StatusNotFound},
		{http.MethodPost, "/volumes/datos/restore", `{}`, http.StatusBadRequest},
		{http.MethodDelete, "/volumes/datos/snapshots/otro", ``, http.StatusNotFound},
		{http.MethodDelete, "/volumes/datos", ``, http.StatusConflict},
		{http.MethodDelete, "/volumes/datos?snapshots=quiza", ``, http.StatusBadRequest},
		// %2F: el filtro corta antes del handler.
		{http.MethodPost, "/volumes/..%2Fimages%2Fmin/snapshots", `{}`, http.StatusBadRequest},
		{http.MethodPost, "/volumes/datos/restore", `{"snapshot":"../../images/min"}`, http.StatusBadRequest},
		{http.MethodDelete, "/volumes/datos/snapshots/..%2F..%2Fdatos", ``, http.StatusBadRequest},
		{http.MethodGet, "/volumes/..%2fimages/snapshots", ``, http.StatusBadRequest},
	} {
		if rr := pedir(s, c.metodo, c.url, c.cuerpo); rr.Code != c.code {
			t.Errorf("%s %s %s = %d, quería %d (%s)", c.metodo, c.url, c.cuerpo, rr.Code, c.code, rr.Body)
		}
	}

	rr := pedir(s, http.MethodPost, "/volumes/datos/restore", `{"snapshot":"uno"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("restore = %d %s", rr.Code, rr.Body)
	}
	var res api.RestoreVolumeResult
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil || res.Undo == nil || res.Undo.Name != "undo" {
		t.Errorf("restore: %+v %v", res, err)
	}
	rr = pedir(s, http.MethodGet, "/volumes/datos/snapshots", "")
	var l []api.VolumeSnapshot
	if err := json.Unmarshal(rr.Body.Bytes(), &l); err != nil || len(l) != 2 {
		t.Errorf("lista: %d %s", rr.Code, rr.Body)
	}
	if rr := pedir(s, http.MethodDelete, "/volumes/datos/snapshots/uno", ""); rr.Code != http.StatusNoContent {
		t.Errorf("rm snapshot = %d %s", rr.Code, rr.Body)
	}
	if rr := pedir(s, http.MethodDelete, "/volumes/datos?snapshots=1", ""); rr.Code != http.StatusNoContent {
		t.Errorf("rm -snapshots = %d %s", rr.Code, rr.Body)
	}
	if _, err := os.Stat(filepath.Join(vols, "snapshots", "datos")); !os.IsNotExist(err) {
		t.Errorf("los snapshots siguen ahí: %v", err)
	}
}
