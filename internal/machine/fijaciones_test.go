package machine

// Fijaciones de v0.18, v0.19 y v0.20, como las de v0.17 (fijaciones_v017_test.go):
// state.json, meta.json y recipe.json, y desde v0.19 fondos.json y un grafo,
// tal como los escribía cada etiqueta. Se generaron con el pkg/api de la
// etiqueta (git archive vX go.mod pkg), rellenando por reflexión cada campo
// exportado con un valor distinto de cero (prefijo v018-, v019-, v020-) y
// serializándolos como lo hacía aquel daemon; id, nombre, estado, IP y base
// se pusieron a mano. publish.json (v0.20) lo prueba internal/publicar. No se
// regeneran: son el pasado.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/esquema"
)

// versionesFijadas son las fijaciones con su state.json y el esquema que este
// binario escribe al reescribirlo: v0.18 no tenía campos de necesitaV3 (se
// queda en el 2 de su diff_base, sin copia); v0.19 y v0.20 sí (al 3, con la
// copia del 2).
var versionesFijadas = []struct {
	dir     string
	escribe int
}{
	{"v0.18", versionEstadoDiff},
	{"v0.19", versionEstadoCampos},
	{"v0.20", versionEstadoCampos},
}

func leerFijacion(t *testing.T, version, fichero string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "esquema", version, fichero))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFijacionesEstado(t *testing.T) {
	for _, f := range versionesFijadas {
		t.Run(f.dir, func(t *testing.T) {
			original := leerFijacion(t, f.dir, "state.json")
			m := newTestManager(t)
			if err := os.WriteFile(m.statePath(), original, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := comprobarVersionEstado(m.root); err != nil {
				t.Fatalf("NewManager no arrancaría: %v", err)
			}
			m.load()
			if m.barridoBloqueado() {
				t.Fatalf("modo protegido: %s", m.estadoIlegible)
			}
			if mc := m.byID["aa11bb22cc33dd44"]; mc == nil || mc.Name != "vieja" {
				t.Fatalf("no cargó la máquina: %+v", m.byID)
			}
			m.mu.Lock()
			m.persist()
			m.mu.Unlock()
			m.writePending()
			b, _ := os.ReadFile(m.statePath())
			if v, err := esquema.Version(b); err != nil || v != f.escribe {
				t.Fatalf("schema escrito = %d, %v; quería %d", v, err, f.escribe)
			}
			bak, err := os.ReadFile(esquema.RutaRespaldo(m.statePath(), versionEstadoDiff))
			if f.escribe > versionEstadoDiff {
				if err != nil || !bytes.Equal(bak, original) {
					t.Fatalf("la copia .v2.bak tiene que ser el original: %v", err)
				}
			} else if err == nil {
				t.Fatal("copió sin subir de esquema")
			}
			var viejo, nuevo struct {
				Machines []json.RawMessage `json:"machines"`
			}
			if json.Unmarshal(original, &viejo) != nil || json.Unmarshal(b, &nuevo) != nil || len(nuevo.Machines) != 1 {
				t.Fatalf("state.json reescrito:\n%s", b)
			}
			conservaCampos(t, "state.json", viejo.Machines[0], nuevo.Machines[0])
		})
	}
}

func TestFijacionesMeta(t *testing.T) {
	for _, f := range versionesFijadas {
		t.Run(f.dir, func(t *testing.T) {
			original := leerFijacion(t, f.dir, "meta.json")
			m := annotTestManager(t)
			writeSnapMeta(t, m, "svc", string(original))
			ruta := filepath.Join(m.snapDir("svc"), "meta.json")
			s, err := m.loadSnapshot("svc")
			if err != nil || s.Name != "svc" {
				t.Fatalf("meta: %+v, %v", s, err)
			}
			leido, _ := json.Marshal(s)
			var sinSchema map[string]any
			json.Unmarshal(original, &sinSchema)
			delete(sinSchema, "schema")
			sin, _ := json.Marshal(sinSchema)
			conservaCampos(t, "meta.json leído", sin, leido)
			if _, err := m.SetAnnotation("svc", "otra", json.RawMessage(`1`)); err != nil {
				t.Fatal(err)
			}
			if _, err := m.RemoveAnnotation("svc", "otra"); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(ruta)
			if v, _ := esquema.Version(b); v != metaSchema {
				t.Fatalf("schema = %d", v)
			}
			if _, err := os.Stat(esquema.RutaRespaldo(ruta, 2)); err == nil {
				t.Fatal("copió un meta que ya estaba en su versión")
			}
			conservaCampos(t, "meta.json", original, b)
		})
	}
}

func TestFijacionesReceta(t *testing.T) {
	for _, f := range versionesFijadas {
		t.Run(f.dir, func(t *testing.T) {
			original := leerFijacion(t, f.dir, "recipe.json")
			m := newTestManager(t)
			if err := os.MkdirAll(filepath.Dir(m.recipePath("eco")), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.recipePath("eco"), original, 0o644); err != nil {
				t.Fatal(err)
			}
			if got := m.recipeBase("eco"); got != "min" {
				t.Errorf("recipeBase = %q", got)
			}
			var rec api.ImageRecipe
			if err := json.Unmarshal(original, &rec); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(rec)
			conservaCampos(t, "recipe.json", original, b)
		})
	}
}

// fondos.json de v0.19/v0.20 (un mapa sin versión) se lee, y la primera
// escritura lo pasa al 1 con su .v0.bak.
func TestFijacionesFondos(t *testing.T) {
	for _, v := range []string{"v0.19", "v0.20"} {
		t.Run(v, func(t *testing.T) {
			original := leerFijacion(t, v, "fondos.json")
			m := newTestManager(t)
			if err := os.WriteFile(m.fondosPath(), original, 0o644); err != nil {
				t.Fatal(err)
			}
			tam, err := leerFondos(m.fondosPath(), original)
			if err != nil || tam["svc"] != 2 {
				t.Fatalf("leerFondos = %v, %v", tam, err)
			}
			m.fondoMu.Lock()
			m.fondoDe("svc").size = tam["svc"]
			err = m.guardarFondos()
			m.fondoMu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if bak, err := os.ReadFile(esquema.RutaRespaldo(m.fondosPath(), 0)); err != nil || !bytes.Equal(bak, original) {
				t.Fatalf("copia .v0.bak: %v", err)
			}
			b, _ := os.ReadFile(m.fondosPath())
			if v, _ := esquema.Version(b); v != versionFondos {
				t.Fatalf("schema = %d:\n%s", v, b)
			}
			if tam, err := leerFondos(m.fondosPath(), b); err != nil || tam["svc"] != 2 {
				t.Fatalf("relee = %v, %v", tam, err)
			}
			// Uno del futuro no se lee ni se pisa.
			futuro := []byte(`{"schema": 9, "pools": {"svc": 1}}`)
			os.WriteFile(m.fondosPath(), futuro, 0o644)
			if _, err := leerFondos(m.fondosPath(), futuro); !esquema.EsMasNuevo(err) {
				t.Fatalf("leerFondos del futuro = %v", err)
			}
			m.fondoMu.Lock()
			err = m.guardarFondos()
			m.fondoMu.Unlock()
			if !esquema.EsMasNuevo(err) {
				t.Fatalf("guardarFondos sobre uno del futuro = %v", err)
			}
			if b, _ := os.ReadFile(m.fondosPath()); !bytes.Equal(b, futuro) {
				t.Fatal("pisó el del futuro")
			}
		})
	}
}

// El grafo de v0.19/v0.20 (sin "format") se carga, y al guardarlo lleva
// "format": 1 sin perder nada. Uno con un format mayor no se carga ni se pisa.
func TestFijacionesGrafo(t *testing.T) {
	for _, v := range []string{"v0.19", "v0.20"} {
		t.Run(v, func(t *testing.T) {
			original := leerFijacion(t, v, "graph.json")
			m := newTestManager(t)
			const id = "0123456789abcdef"
			if err := os.MkdirAll(m.dirGrafos(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(m.rutaGrafo(id), original, 0o600); err != nil {
				t.Fatal(err)
			}
			m.cargarGrafos()
			if g := m.grafos[id]; g == nil || g.Name != "g" {
				t.Fatalf("no cargó el grafo: %+v", m.grafos)
			}
			if err := m.guardarGrafo(id); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(m.rutaGrafo(id))
			if formatoGrafo(b) != formatoGrafoActual {
				t.Fatalf("format = %d:\n%s", formatoGrafo(b), b)
			}
			conservaCampos(t, "graph.json", original, b)

			futuro := []byte(`{"format": 9, "id": "` + id + `", "name": "g", "nodes": {}}`)
			os.WriteFile(m.rutaGrafo(id), futuro, 0o600)
			m2 := newTestManager(t)
			m2.root = m.root
			m2.cargarGrafos()
			if m2.grafos[id] != nil {
				t.Fatal("cargó un grafo del futuro")
			}
			if err := m.guardarGrafo(id); err == nil {
				t.Fatal("pisó un grafo del futuro")
			}
			if b, _ := os.ReadFile(m.rutaGrafo(id)); !bytes.Equal(b, futuro) {
				t.Fatal("el grafo del futuro cambió")
			}
		})
	}
}
