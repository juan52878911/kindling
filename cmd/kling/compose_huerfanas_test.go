package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Una máquina fallida de un up anterior, con la etiqueta de grafo, se borra;
// una del usuario con el mismo nombre y sin etiqueta, no.
func TestQuitarHuerfanas(t *testing.T) {
	var mu sync.Mutex
	var borradas []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]*api.Machine{
			{ID: "a1", Name: "dev-db", State: api.StateFailed, Labels: map[string]string{api.LabelGraph: "viejo"}},
			{ID: "b2", Name: "dev-app"},
			{ID: "c3", Name: "otro-db", Labels: map[string]string{api.LabelGraph: "x"}},
		})
	})
	mux.HandleFunc("DELETE /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		borradas = append(borradas, r.PathValue("ref"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	c := fakeDaemon(t, mux)
	tr := traducirParaPlantillas(t, composePlantillas)
	quitarHuerfanas(context.Background(), c, tr)
	if len(borradas) != 1 || borradas[0] != "a1" {
		t.Fatalf("borradas = %v", borradas)
	}
}
