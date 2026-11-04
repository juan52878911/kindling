package digest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a")
	if err := os.WriteFile(p, []byte("hola"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := File(p)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hola") conocido, para detectar cualquier cambio de algoritmo.
	const want = "b221d9dbb083a7f33428d7c2a3c3198ae925614d70210e28716ccaa7cd4ddb79"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
	if got2, err := File(p); err != nil || got2 != got {
		t.Errorf("no es determinista: %s vs %s (err=%v)", got, got2, err)
	}
	if err := os.WriteFile(p, []byte("adios"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got3, err := File(p); err != nil || got3 == got {
		t.Errorf("no cambió al cambiar el contenido")
	}
}

func TestFileNoExiste(t *testing.T) {
	if _, err := File(filepath.Join(t.TempDir(), "no-existe")); err == nil {
		t.Error("esperaba un error")
	}
}
