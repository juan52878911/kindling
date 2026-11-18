package plantilla

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Antes de borrar el snapshot de una plantilla se borra su fondo sin dueño (el
// daemon no borra un snapshot con instancias vivas); lo de un tenant, lo de
// otra plantilla y lo de otro dueño del daemon se queda.
func TestBorrarPrecalentadasSoloLasLibresDeLaPlantilla(t *testing.T) {
	maquina := func(id string, labels map[string]string) *api.Machine {
		l := map[string]string{api.LabelKind: api.KindSandbox}
		for k, v := range labels {
			l[k] = v
		}
		return &api.Machine{ID: id, State: api.StateRunning, Labels: l}
	}
	vivas := []*api.Machine{
		maquina("libre", map[string]string{EtiquetaPlantilla: "py"}),
		maquina("detenant", map[string]string{EtiquetaPlantilla: "py", etiquetaTenant: "ana"}),
		maquina("otra", map[string]string{EtiquetaPlantilla: "node"}),
		maquina("ajena", map[string]string{EtiquetaPlantilla: "py", api.LabelOwner: "x"}),
	}
	var mu sync.Mutex
	var borradas []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(vivas)
	})
	mux.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		borradas = append(borradas, r.PathValue("ref"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	n, err := BorrarPrecalentadas(context.Background(), arrancarConHandler(t, mux), "py")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || !reflect.DeepEqual(borradas, []string{"libre"}) {
		t.Fatalf("removed %d: %v, want only the free one", n, borradas)
	}
}
