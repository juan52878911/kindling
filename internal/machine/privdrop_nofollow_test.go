package machine

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Un enlace simbólico en el sitio del fichero a ceder no se sigue: el VMM, que
// escribe en el directorio de la máquina, no puede llevar el chown a otro
// fichero. Se abre con O_NOFOLLOW y se cede por descriptor.
func TestOwnNoFollowNoSigueEnlaces(t *testing.T) {
	dir := t.TempDir()
	p := &Privileges{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}

	real := filepath.Join(dir, "overlay.ext4")
	if err := os.WriteFile(real, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.OwnNoFollow(real); err != nil {
		t.Fatalf("fichero normal: %v", err)
	}
	if err := p.OwnNoFollow(dir); err != nil {
		t.Fatalf("directorio: %v", err)
	}

	objetivo := filepath.Join(dir, "ajeno")
	if err := os.WriteFile(objetivo, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	enlace := filepath.Join(dir, "enlace.ext4")
	if err := os.Symlink(objetivo, enlace); err != nil {
		t.Fatal(err)
	}
	err := p.OwnNoFollow(enlace)
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("un enlace debe fallar con ELOOP sin seguirse, got %v", err)
	}
}

func TestOwnNoFollowSinPrivilegiosNoHaceNada(t *testing.T) {
	p := &Privileges{}
	if err := p.OwnNoFollow("/no/existe"); err != nil {
		t.Fatal(err)
	}
}
