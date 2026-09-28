//go:build !darwin

package machine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// cpFalso pone delante en el PATH un cp que rechaza --reflink (como en ext4) y
// pasa lo demás al de verdad.
func cpFalso(t *testing.T) {
	t.Helper()
	real, err := exec.LookPath("cp")
	if err != nil {
		t.Skip("no cp")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in --reflink=*) echo 'cp: failed to clone: Operation not supported' >&2; exit 1;; esac\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "cp"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
}

// Sin reflink se cae a copia completa, dispersa, y lo dice; pero antes pasa
// por la comprobación de sitio, cuyo error manda.
func TestClonarDiscoCaeACopiaSinReflink(t *testing.T) {
	cpFalso(t)
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("contenido"), 0o644); err != nil {
		t.Fatal(err)
	}
	llamada := false
	modo, err := clonarDisco(context.Background(), src, dst, func() error { llamada = true; return nil })
	if err != nil || modo != "copy" || !llamada {
		t.Fatalf("modo=%q err=%v comprobación=%v", modo, err, llamada)
	}
	if b, _ := os.ReadFile(dst); string(b) != "contenido" {
		t.Errorf("copia mal: %q", b)
	}

	lleno := errors.New("sin sitio")
	dst2 := filepath.Join(dir, "c")
	if _, err := clonarDisco(context.Background(), src, dst2, func() error { return lleno }); !errors.Is(err, lleno) {
		t.Errorf("quería el error de sitio, dio %v", err)
	}
	if _, err := os.Stat(dst2); !os.IsNotExist(err) {
		t.Errorf("dejó el destino a medias: %v", err)
	}
}

// Con el cp de verdad, reflink o copy según el sistema de ficheros, y el
// contenido igual en los dos casos.
func TestClonarDiscoConElCpDelSistema(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("contenido"), 0o644); err != nil {
		t.Fatal(err)
	}
	modo, err := clonarDisco(context.Background(), src, dst, func() error { return nil })
	if err != nil || (modo != "reflink" && modo != "copy") {
		t.Fatalf("modo=%q err=%v", modo, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "contenido" {
		t.Errorf("copia mal (%s): %q", modo, b)
	}
}
