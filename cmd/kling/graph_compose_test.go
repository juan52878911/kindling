package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sinEntorno(string) (string, bool) { return "", false }

// Los tres compose de testdata/compose se traducen a su grafo y sus avisos
// esperados (UPDATE_GOLDEN=1 los reescribe).
func TestComposeGolden(t *testing.T) {
	for _, nombre := range []string{"webapp", "health", "unsupported"} {
		t.Run(nombre, func(t *testing.T) {
			dir := filepath.Join("testdata", "compose")
			datos, err := os.ReadFile(filepath.Join(dir, nombre+".compose.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			tr, err := traducirCompose(datos, nombre, false, sinEntorno)
			if err != nil {
				t.Fatal(err)
			}
			sal := map[string]string{
				nombre + ".graph.yaml":   tr.Grafo,
				nombre + ".warnings.txt": strings.Join(tr.Avisos, "\n") + "\n",
			}
			for f, got := range sal {
				p := filepath.Join(dir, f)
				if os.Getenv("UPDATE_GOLDEN") != "" {
					if err := os.WriteFile(p, []byte(got), 0o644); err != nil {
						t.Fatal(err)
					}
					continue
				}
				want, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if got != string(want) {
					t.Errorf("%s differs:\n--- got\n%s--- want\n%s", f, got, want)
				}
			}
		})
	}
}

// El grafo traducido de webapp se lee con el mismo camino que `graph up`.
func TestComposeGrafoSeLee(t *testing.T) {
	datos, _ := os.ReadFile(filepath.Join("testdata", "compose", "webapp.graph.yaml"))
	req, err := leerGrafo("webapp.graph.yaml", datos, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Graph.Name != "shop" || len(req.Graph.Nodes) != 3 || len(req.Graph.Edges) != 4 {
		t.Errorf("graph = %+v", req.Graph)
	}
}

func TestComposeInterpolar(t *testing.T) {
	env := map[string]string{"A": "uno", "VACIA": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	var avisos []string
	got := interpolarCompose("${A} ${B:-dos} ${VACIA:-tres} ${VACIA-cuatro} $$A ${FALTA}", lookup,
		func(s string) { avisos = append(avisos, s) })
	if want := "uno dos tres  $A "; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(avisos) != 1 || !strings.Contains(avisos[0], "FALTA") {
		t.Errorf("avisos = %v", avisos)
	}
}

func TestComposeMemoria(t *testing.T) {
	for in, want := range map[string]int{"512m": 512, "1g": 1024, "256MB": 256, "1073741824": 1024, "2048k": 2} {
		if got := memoriaMiB(in, "x", func(string, ...any) {}); got != want {
			t.Errorf("%s = %d, want %d", in, got, want)
		}
	}
}

func TestComposeErrores(t *testing.T) {
	for nombre, src := range map[string]string{
		"sin servicios": "name: x\n",
		"solo build":    "services:\n  a:\n    build: .\n",
		"ancla":         "services:\n  a: &x\n    image: y\n",
		"lista":         "- a\n- b\n",
	} {
		if _, err := traducirCompose([]byte(src), "x", false, sinEntorno); err == nil {
			t.Errorf("%s: want an error", nombre)
		}
	}
}

// Un servicio con guion bajo se renombra y se avisa; -name manda sobre name:.
func TestComposeNombres(t *testing.T) {
	src := "name: viejo\nservices:\n  mi_web:\n    image: x/y\n    ports: [\"8000\"]\n"
	tr, err := traducirCompose([]byte(src), "nuevo", true, sinEntorno)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tr.Grafo, "name: nuevo\n") || !strings.Contains(tr.Grafo, "  mi-web: ") {
		t.Errorf("grafo:\n%s", tr.Grafo)
	}
	if !strings.Contains(strings.Join(tr.Avisos, "\n"), `renamed to node "mi-web"`) {
		t.Errorf("avisos = %v", tr.Avisos)
	}
}
