//go:build linux

package machine

import (
	"os"
	"path/filepath"
	"testing"
)

// Borrar el jail no debe invalidar el digest cacheado del disco base: sus
// enlaces duros se sueltan bajo enlaceEstable.
func TestSoltarEnlacesBaseConservaElDigest(t *testing.T) {
	m := newTestManager(t)
	antes := huellaConfiable // los tests no corren como root
	huellaConfiable = func(os.FileInfo) bool { return true }
	t.Cleanup(func() { huellaConfiable = antes })
	dir := t.TempDir()
	img := filepath.Join(dir, "base.ext4")
	if err := os.WriteFile(img, []byte("imagen base"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	jail := filepath.Join(dir, "jail", "root")
	if err := os.MkdirAll(jail, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.enlaceEstable(img, func() error { return os.Link(img, filepath.Join(jail, "rootfs.ext4")) }); err != nil {
		t.Fatal(err)
	}
	lecturas := 0
	viejo := digestArbol
	digestArbol = func(p string) (string, error) { lecturas++; return viejo(p) }
	t.Cleanup(func() { digestArbol = viejo })

	m.soltarEnlacesBase(filepath.Join(dir, "jail"))
	if _, err := os.Stat(filepath.Join(jail, "rootfs.ext4")); !os.IsNotExist(err) {
		t.Fatalf("el enlace del jail sigue ahí: %v", err)
	}
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	if lecturas != 0 {
		t.Fatalf("borrar el jail obligó a releer la imagen (%d lecturas)", lecturas)
	}
}
