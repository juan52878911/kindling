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

// R3-4: «no existe» no es un fallo, como en Own.
func TestOwnNoFollowNoExisteNoEsFallo(t *testing.T) {
	p := &Privileges{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}
	if err := p.OwnNoFollow(filepath.Join(t.TempDir(), "no-existe")); err != nil {
		t.Fatalf("un fichero que no existe: %v", err)
	}
}

// R3-4: en el arranque en frío, un enlace plantado en el sitio del overlay no
// lleva el chown al fichero al que apunta: falla con ELOOP.
func TestArranqueFrioNoCedeUnEnlaceEnElOverlay(t *testing.T) {
	m := newTestManager(t)
	m.priv = &Privileges{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}
	dir := t.TempDir()
	ajeno := filepath.Join(t.TempDir(), "de-root")
	if err := os.WriteFile(ajeno, []byte("y"), 0o600); err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(dir, "overlay.ext4")
	if err := os.Symlink(ajeno, overlay); err != nil {
		t.Fatal(err)
	}
	if err := m.cederArranqueFrio(dir, overlay); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("con un enlace en el sitio del overlay: %v, quería ELOOP", err)
	}
	// Con el overlay normal, se cede.
	if err := os.Remove(overlay); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.cederArranqueFrio(dir, overlay); err != nil {
		t.Fatalf("overlay normal: %v", err)
	}
}
