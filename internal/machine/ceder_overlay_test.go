package machine

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// B-4: el overlay se cede por un descriptor abierto con O_NOFOLLOW; un enlace
// simbólico en su sitio no lleva el chown al fichero al que apunta.
func TestCederOverlayNoSigueEnlaces(t *testing.T) {
	p := &Privileges{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}
	dir := t.TempDir()
	ajeno := filepath.Join(dir, "ajeno")
	if err := os.WriteFile(ajeno, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay.ext4")
	if err := os.Symlink(ajeno, overlay); err != nil {
		t.Fatal(err)
	}
	err := cederSinSeguirEnlaces(p, overlay)
	if err == nil || !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("con un enlace en el sitio del overlay, err = %v; quería ELOOP", err)
	}
	// Un fichero normal se cede; uno que falta no es un fallo (como Own).
	real := filepath.Join(dir, "real.ext4")
	if err := os.WriteFile(real, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cederSinSeguirEnlaces(p, real); err != nil {
		t.Fatal(err)
	}
	if err := cederSinSeguirEnlaces(p, filepath.Join(dir, "no-esta")); err != nil {
		t.Fatalf("un fichero que falta no es un fallo: %v", err)
	}
	if err := cederSinSeguirEnlaces(&Privileges{}, overlay); err != nil {
		t.Fatalf("sin privilegios no hace nada: %v", err)
	}
}
