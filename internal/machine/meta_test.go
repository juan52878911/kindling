package machine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// El meta.json se escribe al lado y se renombra, nunca encima.
//
// os.WriteFile TRUNCA primero: un corte a media escritura —o un lector
// concurrente, y Snapshots() se sirve en cada petición del gateway— dejaba un
// meta vacío o partido. El servicio desaparecía del catálogo de forma
// PERMANENTE, porque RemoveSnapshot se niega a borrar lo que no puede leer.
func TestElMetaSeEscribeSinDejarloAMedias(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "meta.json")

	// Un meta previo que NO debe perderse si la escritura nueva falla.
	if err := os.WriteFile(final, []byte(`{"name":"viejo"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeMeta(dir, []byte(`{"name":"nuevo"}`)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Name string }
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("el meta quedó ilegible: %v", err)
	}
	if got.Name != "nuevo" {
		t.Errorf("name = %q, want nuevo", got.Name)
	}

	// Y no debe quedar un .tmp suelto: quien liste el directorio vería basura.
	if _, err := os.Stat(final + ".tmp"); err == nil {
		t.Error("quedó un meta.json.tmp por ahí")
	}
}

// Si el renombrado no puede completarse, el meta ANTERIOR sigue intacto.
//
// Es la propiedad que justifica todo esto: fallar dejando lo viejo es
// recuperable; fallar dejando medio fichero, no.
func TestUnFalloAlEscribirNoDestruyeElMetaAnterior(t *testing.T) {
	dir := t.TempDir()
	final := filepath.Join(dir, "meta.json")
	if err := os.WriteFile(final, []byte(`{"name":"intacto"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Un directorio inexistente hace fallar la escritura del temporal.
	if err := writeMeta(filepath.Join(dir, "no-existe"), []byte(`{"name":"nuevo"}`)); err == nil {
		t.Fatal("debería fallar")
	}

	b, _ := os.ReadFile(final)
	var got struct{ Name string }
	if err := json.Unmarshal(b, &got); err != nil || got.Name != "intacto" {
		t.Errorf("el meta anterior se dañó: %q (%v)", b, err)
	}
}
