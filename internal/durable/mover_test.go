package durable

import (
	"os"
	"path/filepath"
	"testing"
)

// Mover publica el fichero en su sitio nuevo y no deja el viejo.
func TestMover(t *testing.T) {
	dir := t.TempDir()
	origen, destino := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := Escribir(origen, []byte("sello"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Mover(origen, destino); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(destino); err != nil || string(b) != "sello" {
		t.Fatalf("destino: %q, %v", b, err)
	}
	if _, err := os.Stat(origen); !os.IsNotExist(err) {
		t.Fatalf("el origen sigue: %v", err)
	}
	if err := Mover(origen, destino); !os.IsNotExist(err) {
		t.Fatalf("sin origen: %v; quería ErrNotExist", err)
	}
}

// Medición de P2: retirar el sello de un volcado antes de cargarlo, con el
// fsync del fichero (Renombrar) y sin él (Mover).
// go test ./internal/durable -run x -bench 'Marca'
func BenchmarkMarcaRenombrar(b *testing.B) { benchMarca(b, Renombrar) }
func BenchmarkMarcaMover(b *testing.B)     { benchMarca(b, Mover) }

func benchMarca(b *testing.B, mover func(string, string) error) {
	dir := b.TempDir()
	a, c := filepath.Join(dir, "volcado.ok"), filepath.Join(dir, "volcado.en-curso")
	if err := Escribir(a, []byte(`{"schema":1}`), 0o600); err != nil {
		b.Fatal(err)
	}
	for i := 0; b.Loop(); i++ {
		de, a2 := a, c
		if i%2 == 1 {
			de, a2 = c, a
		}
		if err := mover(de, a2); err != nil {
			b.Fatal(err)
		}
	}
}
