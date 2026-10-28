package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/events"
	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// GET /machines/{ref}/egress: la política de la máquina con sus destinos
// cortados (vacíos si no hay nada), y 404 si no existe.
func TestHandleEgress(t *testing.T) {
	root := t.TempDir()
	seed := []*api.Machine{
		{ID: "d1d1d1d1d1d1d1d1d1", Name: "sin", State: api.StateWarm},
		{ID: "d2d2d2d2d2d2d2d2d2", Name: "net", State: api.StateWarm, Egress: "internet"},
	}
	b, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
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

	for ref, want := range map[string]string{"sin": "none", "net": "internet"} {
		rr := call(t, h, "GET", "/machines/"+ref+"/egress", "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", ref, rr.Code, rr.Body)
		}
		var got api.EgressBlocked
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Egress != want || got.Total != 0 || len(got.Destinations) != 0 {
			t.Fatalf("%s: %+v", ref, got)
		}
	}
	if rr := call(t, h, "GET", "/machines/nadie/egress", ""); rr.Code != http.StatusNotFound {
		t.Fatalf("máquina inexistente: %d", rr.Code)
	}
}
