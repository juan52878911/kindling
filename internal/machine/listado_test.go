package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Dos directorios cuyos metas declaran el MISMO nombre no pueden salir como dos
// servicios idénticos en el listado.
//
// Es lo que se observó: `sequentialthinking` aparecía dos veces con tamaños
// distintos y un solo directorio en disco. El listado se creía el nombre del
// meta, pero quien instancia (`runFrom`) resuelve por DIRECTORIO — así que uno de
// los dos era un fantasma que no se podía arrancar.
func TestElListadoUsaElDirectorioYNoElMeta(t *testing.T) {
	raiz := t.TempDir()
	m := &Manager{root: raiz}
	if err := os.MkdirAll(filepath.Join(raiz, "snapshots"), 0o755); err != nil {
		t.Fatal(err)
	}
	escribirMeta := func(dir, nombreEnMeta string, bytes int64) {
		d := m.snapDir(dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(map[string]any{"name": nombreEnMeta, "mem_bytes": bytes})
		if err := os.WriteFile(filepath.Join(d, "meta.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Dos directorios distintos, los dos declarando llamarse "alfa".
	escribirMeta("alfa", "alfa", 100)
	escribirMeta("beta", "alfa", 200)

	nombres := map[string]int{}
	for _, s := range m.Snapshots() {
		nombres[s.Name]++
	}
	if nombres["alfa"] != 1 {
		t.Errorf("debería haber exactamente un 'alfa', hubo %d: %v", nombres["alfa"], nombres)
	}
	if nombres["beta"] != 1 {
		t.Errorf("el segundo debería listarse por su directorio ('beta'), no como 'alfa': %v", nombres)
	}
}

// Un solo snapshot con el meta coherente sigue funcionando igual: el arreglo no
// debe cambiar el caso normal.
func TestElListadoNormalNoCambia(t *testing.T) {
	raiz := t.TempDir()
	m := &Manager{root: raiz}
	d := m.snapDir("uno")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"name": "uno", "mem_bytes": 42})
	if err := os.WriteFile(filepath.Join(d, "meta.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	got := m.Snapshots()
	if len(got) != 1 || got[0].Name != "uno" {
		t.Fatalf("esperaba un snapshot llamado 'uno', obtuve %v", got)
	}
	if got[0].MemBytes != 42 {
		t.Errorf("el resto del meta debe conservarse: mem_bytes = %d", got[0].MemBytes)
	}
}
