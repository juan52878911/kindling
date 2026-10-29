package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Una máquina fallida de un up anterior, con la etiqueta de grafo, se borra;
// una -prep con la etiqueta del proyecto (Ctrl-C dentro de su Run), también;
// una del usuario con el mismo nombre y sin etiqueta, no, ni la -prep de
// otro proyecto.
func TestQuitarHuerfanas(t *testing.T) {
	var mu sync.Mutex
	var borradas []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /machines", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]*api.Machine{
			{ID: "a1", Name: "dev-db", State: api.StateFailed, Labels: map[string]string{api.LabelGraph: "viejo"}},
			{ID: "b2", Name: "dev-app"},
			{ID: "c3", Name: "otro-db", Labels: map[string]string{api.LabelGraph: "x"}},
			{ID: "d4", Name: "dev-app-1a2b3c-prep", Labels: map[string]string{labelComposeProject: "dev"}},
			{ID: "e5", Name: "otro-app-1a2b3c-prep", Labels: map[string]string{labelComposeProject: "otro"}},
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
	slices.Sort(borradas)
	if !slices.Equal(borradas, []string{"a1", "d4"}) {
		t.Fatalf("borradas = %v", borradas)
	}
}

// M9: un proyecto es de su compose. Otro fichero con el mismo nombre de
// proyecto, o un grafo que no hizo compose, no se tocan; uno de un kling
// anterior (sin la ruta, con kling.compose) sí.
func TestDuenoDelProyecto(t *testing.T) {
	g := &api.Graph{ID: "g1", Name: "dev"}
	maq := func(l map[string]string) []*api.Machine {
		l[api.LabelGraph] = "g1"
		return []*api.Machine{{ID: "m", Name: "dev-db", Labels: l}, {ID: "x", Name: "ajena", Labels: map[string]string{api.LabelGraph: "otro"}}}
	}
	mio := maq(map[string]string{labelComposeFile: "/code/a/compose.yaml", labelComposeProject: "dev"})
	if err := duenoDe(g, mio, "dev", "/code/a/compose.yaml"); err != nil {
		t.Fatalf("el suyo: %v", err)
	}
	err := duenoDe(g, mio, "dev", "/code/b/compose.yaml")
	if err == nil || !strings.Contains(err.Error(), "another compose file (/code/a/compose.yaml)") {
		t.Fatalf("de otro fichero: %v", err)
	}
	if err := duenoDe(g, mio, "dev", ""); err != nil {
		t.Fatalf("down -p sin fichero: %v", err)
	}
	if err := duenoDe(g, maq(map[string]string{labelComposeProject: "dev"}), "dev", "/code/b/compose.yaml"); err != nil {
		t.Fatalf("de un kling anterior: %v", err)
	}
	if err := duenoDe(g, maq(map[string]string{}), "dev", "/code/a/compose.yaml"); err == nil || !strings.Contains(err.Error(), "not made by kling compose") {
		t.Fatalf("grafo a mano: %v", err)
	}
}

// M9: dos up del mismo proyecto contra el mismo daemon, no a la vez.
func TestCerrojoProyecto(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	a, err := cerrojoProyecto("unix:///tmp/x.sock", "dev")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cerrojoProyecto("unix:///tmp/x.sock", "dev"); err == nil || !strings.Contains(err.Error(), "another kling compose up") {
		t.Fatalf("segundo up: %v", err)
	}
	// Otro proyecto u otro daemon, sí.
	b, err := cerrojoProyecto("unix:///tmp/x.sock", "otro")
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	a.Close()
	c, err := cerrojoProyecto("unix:///tmp/x.sock", "dev")
	if err != nil {
		t.Fatalf("tras soltarlo: %v", err)
	}
	c.Close()
}

// Los nodos llevan de qué compose son (M9), y el grafo sigue siendo válido.
func TestGrafoLlevaElCompose(t *testing.T) {
	tr := traducirParaPlantillas(t, composePlantillas)
	tr.etiquetas = map[string]string{labelComposeProject: "dev", labelComposeFile: "/code/dev app/compose.yaml"}
	g := grafoDeCompose(tr, "dev", tr.vivos, tr.por, func(string, ...any) {})
	req, err := leerGrafo("dev.yaml", []byte(g), nil)
	if err != nil {
		t.Fatalf("%v\n%s", err, g)
	}
	for n, nd := range req.Graph.Nodes {
		if nd.Labels[labelComposeFile] != "/code/dev app/compose.yaml" || nd.Labels[labelComposeProject] != "dev" {
			t.Errorf("%s: labels = %v", n, nd.Labels)
		}
	}
}
