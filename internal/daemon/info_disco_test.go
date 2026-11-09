package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// /info dice cuánto disco queda bajo la raíz: `kling doctor` lo compara con
// el mínimo de la admisión antes de que un run lo descubra con un 503.
func TestInfoTraeElDiscoLibre(t *testing.T) {
	root := t.TempDir()
	s := &Server{mgr: nuevoManager(t, root), root: root}
	rr := httptest.NewRecorder()
	s.routes().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/info", nil))
	var info api.Info
	if err := json.Unmarshal(rr.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.DiskFreeBytes <= 0 {
		t.Fatalf("disk_free_bytes = %d", info.DiskFreeBytes)
	}
}
