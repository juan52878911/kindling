package machine

// Fijaciones de v0.17 (docs/actualizar.md §5, PR 7): un state.json, un
// meta.json y una receta tal como los escribía kling v0.17.0, con TODOS los
// campos que tenían entonces rellenos, y la prueba de que este binario los lee,
// los migra con su copia .v0.bak y no pierde ninguno al reescribirlos.
//
// Los ficheros de testdata/esquema/v0.17 se generaron con el pkg/api de la
// etiqueta v0.17.0 (git archive v0.17.0 go.mod pkg), rellenando por reflexión
// cada campo exportado con un valor distinto de cero y serializándolos como lo
// hacía aquel daemon: state.json era json.MarshalIndent del array de máquinas,
// meta.json el del api.Snapshot y la receta el del api.ImageRecipe. Unos pocos
// valores (id, estado, IP, base) se pusieron a mano para que sean verosímiles.
// No se regeneran: son el pasado, y lo que se prueba es que el presente los
// sigue entendiendo.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/esquema"
)

func leerFijacionV017(t *testing.T, fichero string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "esquema", "v0.17", fichero))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// conservaCampos falla si alguna clave de viejo falta en nuevo o tiene otro
// valor. Las claves que nuevo añade (las de la versión actual) dan igual.
func conservaCampos(t *testing.T, que string, viejo, nuevo []byte) {
	t.Helper()
	var v, n map[string]any
	if err := json.Unmarshal(viejo, &v); err != nil {
		t.Fatalf("%s: la fijación no es un objeto JSON: %v", que, err)
	}
	if err := json.Unmarshal(nuevo, &n); err != nil {
		t.Fatalf("%s: lo reescrito no es un objeto JSON: %v\n%s", que, err, nuevo)
	}
	var claves []string
	for k := range v {
		claves = append(claves, k)
	}
	sort.Strings(claves)
	for _, k := range claves {
		got, ok := n[k]
		if !ok {
			t.Errorf("%s: se perdió el campo %q de v0.17 (valía %v)", que, k, v[k])
			continue
		}
		if !reflect.DeepEqual(v[k], got) {
			t.Errorf("%s: el campo %q cambió: v0.17 %v, ahora %v", que, k, v[k], got)
		}
	}
}

// El state.json de v0.17 (un array sin schema) arranca, se copia a .v0.bak
// antes de reescribirlo y la máquina reescrita conserva todos sus campos.
func TestFijacionV017EstadoSeMigraSinPerderCampos(t *testing.T) {
	original := leerFijacionV017(t, "state.json")
	m := newTestManager(t)
	if err := os.WriteFile(m.statePath(), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := comprobarVersionEstado(m.root); err != nil {
		t.Fatalf("NewManager no arrancaría con el estado de v0.17: %v", err)
	}
	m.load()
	if m.barridoBloqueado() {
		t.Fatalf("el estado de v0.17 dejó el daemon en modo protegido: %s", m.estadoIlegible)
	}
	if mc := m.byID["aa11bb22cc33dd44"]; mc == nil || mc.Name != "vieja" {
		t.Fatalf("no cargó la máquina de v0.17: %+v", m.byID)
	}
	copia, err := os.ReadFile(esquema.RutaRespaldo(m.statePath(), 0))
	if err != nil || !bytes.Equal(copia, original) {
		t.Fatalf("la copia .v0.bak tiene que ser el original: %v", err)
	}

	m.mu.Lock()
	m.persist()
	m.mu.Unlock()
	m.writePending()
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		t.Fatal(err)
	}
	if v, err := esquema.Version(b); err != nil || v != versionEstado {
		t.Fatalf("tras escribir, schema = %d, %v; quería %d", v, err, versionEstado)
	}
	var viejas []json.RawMessage
	if err := json.Unmarshal(original, &viejas); err != nil || len(viejas) != 1 {
		t.Fatalf("fijación: %v", err)
	}
	var nuevo struct {
		Machines []json.RawMessage `json:"machines"`
	}
	if err := json.Unmarshal(b, &nuevo); err != nil || len(nuevo.Machines) != 1 {
		t.Fatalf("state.json reescrito: %v\n%s", err, b)
	}
	conservaCampos(t, "state.json", viejas[0], nuevo.Machines[0])
}

// El meta.json de v0.17 se lee entero; la primera anotación deja la copia
// .v0.bak y el meta reescrito (ya v1) conserva todos los campos.
func TestFijacionV017MetaSeMigraSinPerderCampos(t *testing.T) {
	original := leerFijacionV017(t, "meta.json")
	m := annotTestManager(t)
	writeSnapMeta(t, m, "svc", string(original))
	ruta := filepath.Join(m.snapDir("svc"), "meta.json")

	s, err := m.loadSnapshot("svc")
	if err != nil {
		t.Fatalf("el meta de v0.17 se tiene que poder leer: %v", err)
	}
	if s.Name != "svc" || s.Stale != "" {
		t.Fatalf("meta de v0.17 mal leído (un v0 sin vmm no está obsoleto): %+v", s)
	}
	// Leído al struct, no solo conservado: editMeta guarda tal cual las claves
	// que no conoce, así que comparar solo el fichero no vería un campo de
	// v0.17 que api.Snapshot ya no entiende.
	leido, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	conservaCampos(t, "meta.json leído", original, leido)
	if _, err := m.SetAnnotation("svc", "otra", json.RawMessage(`1`)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RemoveAnnotation("svc", "otra"); err != nil {
		t.Fatal(err)
	}
	copia, err := os.ReadFile(esquema.RutaRespaldo(ruta, 0))
	if err != nil || !bytes.Equal(copia, original) {
		t.Fatalf("la copia .v0.bak tiene que ser el original: %v", err)
	}
	b, err := os.ReadFile(ruta)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := esquema.Version(b); err != nil || v != metaSchema {
		t.Fatalf("tras escribir, schema = %d, %v; quería %d", v, err, metaSchema)
	}
	conservaCampos(t, "meta.json", original, b)
}

// La receta no tiene versión (se lee laxa y no se reescribe): la de v0.17 da
// la base, el techo de CPU y la pila IPv6 que pide, y api.ImageRecipe la
// relee y la vuelve a escribir sin perder nada.
func TestFijacionV017RecetaSeLee(t *testing.T) {
	original := leerFijacionV017(t, "recipe.json")
	m := newTestManager(t)
	if err := os.MkdirAll(filepath.Dir(m.recipePath("eco")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.recipePath("eco"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.recipeBase("eco"); got != "min" {
		t.Errorf("recipeBase = %q; quería min", got)
	}
	// cpu_pct_per_vcpu (80) manda sobre cpu_pct: 80 × 2 vCPU.
	if got := m.techoCPUPorDefecto("eco", 2, 0); got != 160 {
		t.Errorf("techoCPUPorDefecto = %d; quería 160", got)
	}
	if !m.ipv6DeReceta("eco") {
		t.Error("ipv6DeReceta no ve guest_ipv6_stack")
	}

	var rec api.ImageRecipe
	if err := json.Unmarshal(original, &rec); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	conservaCampos(t, "recipe.json", original, b)
}
