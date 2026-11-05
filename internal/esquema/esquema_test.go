package esquema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersion(t *testing.T) {
	casos := []struct {
		in    string
		v     int
		error bool
	}{
		{`[]`, 0, false},
		{`[{"id":"x"}]`, 0, false},
		{`{"machines":[]}`, 0, false},
		{`  {"schema": 3, "x": 1}`, 3, false},
		{`{"schema": "3"}`, 0, true},
		{`{"schema": -1}`, 0, true},
		{`{"schema": 1.5}`, 0, true},
		{`{"schema": 1`, 0, true},
		{``, 0, false},
	}
	for _, c := range casos {
		v, err := Version([]byte(c.in))
		if (err != nil) != c.error || v != c.v {
			t.Errorf("Version(%q) = %d, %v; se esperaba %d, error=%v", c.in, v, err, c.v, c.error)
		}
	}
}

func TestComprobarMasNuevo(t *testing.T) {
	if _, err := Comprobar("f.json", []byte(`{"schema":2}`), 2); err != nil {
		t.Fatalf("la versión soportada no es del futuro: %v", err)
	}
	v, err := Comprobar("f.json", []byte(`{"schema":3}`), 2)
	if !EsMasNuevo(err) || v != 3 {
		t.Fatalf("Comprobar = %d, %v; se esperaba ErrMasNuevo con 3", v, err)
	}
}

// La copia buena es la primera: un segundo Respaldar no la pisa.
func TestRespaldarNoPisaLaPrimera(t *testing.T) {
	dir := t.TempDir()
	ruta := filepath.Join(dir, "state.json")
	if err := Respaldar(ruta, 0); err != nil {
		t.Fatalf("sin fichero no hay nada que guardar, y no es error: %v", err)
	}
	if err := os.WriteFile(ruta, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Respaldar(ruta, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ruta, []byte("a medio migrar"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Respaldar(ruta, 0); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(RutaRespaldo(ruta, 0))
	if err != nil || string(b) != "original" {
		t.Fatalf("copia = %q, %v", b, err)
	}
	if fi, _ := os.Stat(RutaRespaldo(ruta, 0)); fi.Mode().Perm() != 0o600 {
		t.Errorf("la copia no conserva los permisos: %v", fi.Mode().Perm())
	}
}

// PrepararEscritura guarda la copia de una versión anterior, se niega ante
// una del futuro y no estorba si no hay fichero o es ilegible.
func TestPrepararEscritura(t *testing.T) {
	dir := t.TempDir()
	ruta := filepath.Join(dir, "f.json")
	if err := PrepararEscritura(ruta, 1, 1); err != nil {
		t.Fatalf("sin fichero: %v", err)
	}
	os.WriteFile(ruta, []byte(`[1,2]`), 0o600)
	if err := PrepararEscritura(ruta, 1, 1); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(RutaRespaldo(ruta, 0)); err != nil || string(b) != `[1,2]` {
		t.Fatalf("copia v0 = %q, %v", b, err)
	}
	os.WriteFile(ruta, []byte(`{"schema":1}`), 0o600)
	if err := PrepararEscritura(ruta, 1, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(RutaRespaldo(ruta, 1)); !os.IsNotExist(err) {
		t.Fatal("copió un fichero que ya estaba en la versión que se escribe")
	}
	// Escribir una versión más baja que la del disco, dentro de lo que se
	// entiende, no es migrar: sin copia.
	os.WriteFile(ruta, []byte(`{"schema":3}`), 0o600)
	if err := PrepararEscritura(ruta, 1, 3); err != nil {
		t.Fatal(err)
	}
	err := PrepararEscritura(ruta, 1, 2)
	if !EsMasNuevo(err) {
		t.Fatalf("fichero del futuro: err = %v", err)
	}
	if !strings.Contains(err.Error(), "update kling") {
		t.Fatalf("el mensaje no dice que hay que actualizar kling: %v", err)
	}
	os.WriteFile(ruta, []byte(`{"schema":"x"}`), 0o600)
	if err := PrepararEscritura(ruta, 1, 1); err != nil {
		t.Fatalf("ilegible: %v", err)
	}
}
