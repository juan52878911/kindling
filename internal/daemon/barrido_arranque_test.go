package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// Lo que deja una construcción o una descarga cortadas (el daemon murió, el
// Mac se apagó) solo se borraba en el defer que ya no corrió: se iba
// acumulando en build/ y en la caché OCI. Al arrancar, antes de Listen, se
// barre; lo bueno se queda.
func TestBarrerTemporalesDelArranque(t *testing.T) {
	root := t.TempDir()
	// mk crea p como fichero o, con dir, como directorio con algo dentro.
	mk := func(p string, dir bool) {
		t.Helper()
		f := filepath.Join(root, p)
		if dir {
			f = filepath.Join(f, "out", "f")
		}
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const hex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	basura := []string{
		"build/imagefile-42",
		"cache/oci/sha256/" + hex + ".part",
		"cache/oci/sha256/" + hex + ".part-998877",
		"cache/oci/sha256/" + hex + ".a1b2c3d4e5f6.part",
		"cache/verified/oci/sha256/.tmp-1234",
		"cache/verified/registry-ghcr.io/oci/sha256/.tmp-5678",
	}
	buenos := []string{
		"cache/oci/sha256/" + hex,
		"cache/verified/oci/sha256/" + hex,
		"build/README",
		"images/alpine.ext4",
	}
	for _, p := range basura {
		mk(p, false)
	}
	for _, p := range []string{"build/alpine.123456789", "build/node.20.987654"} {
		mk(p, true)
		basura = append(basura, p)
	}
	for _, p := range buenos {
		mk(p, false)
	}
	if n := barrerTemporales(root); n != len(basura) {
		t.Errorf("swept %d, want %d", n, len(basura))
	}
	for _, p := range basura {
		if _, err := os.Lstat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep: %v", p, err)
		}
	}
	for _, p := range buenos {
		if _, err := os.Lstat(filepath.Join(root, p)); err != nil {
			t.Errorf("%s was swept: %v", p, err)
		}
	}
	// Sin nada que barrer (un root nuevo), nada falla.
	if n := barrerTemporales(t.TempDir()); n != 0 {
		t.Errorf("swept %d in an empty root", n)
	}
}
