package machine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Hipótesis 1.1: enlazar y desenlazar la imagen (lo que hace cada
// restauración con jailer) mueve su ctime e invalida el digest cacheado.
func TestEnlaceDuroInvalidaDigestCacheado(t *testing.T) {
	m := newTestManager(t)
	img := filepath.Join(t.TempDir(), "img.ext4")
	if err := os.WriteFile(img, []byte("imagen"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := contarDigests(t, 0)
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	otro := img + ".lnk"
	time.Sleep(10 * time.Millisecond) // que el ctime se distinga
	if err := os.Link(img, otro); err != nil {
		t.Fatal(err)
	}
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(otro); err != nil {
		t.Fatal(err)
	}
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("lecturas de la imagen = %d, quiero 1 (enlazar y borrar no cambia su contenido)", got)
	}
}
