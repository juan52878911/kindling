package machine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// Sin debugfs, hasFile tiene que decir ErrNoDebugfs y no un error cualquiera:
// quien lo llama (sandbox create, run con volúmenes) sigue adelante con ese y
// solo con ese. Antes buscaba con LookPath y /sbin, y en un Mac con e2fsprogs
// keg-only de Homebrew no lo encontraba aunque estuviera.
func TestHasFileSinDebugfs(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	viejo := dirsE2fsExtra
	dirsE2fsExtra = nil
	t.Cleanup(func() { dirsE2fsExtra = viejo })
	if debugfsBin() != "" {
		t.Skip("debugfs en /sbin o /usr/sbin: no se puede simular su ausencia")
	}
	_, err := hasFile(context.Background(), "/no/existe.ext4", "/x")
	if !errors.Is(err, ErrNoDebugfs) {
		t.Fatalf("err = %v; quería ErrNoDebugfs", err)
	}
}

// hasFile pasaba la ruta a debugfs sin comillas ni validar: una comilla o un
// salto de línea se colaban en la orden. Ahora va como las de put_debugfs.go.
func TestHasFileRutaConEspacio(t *testing.T) {
	ruta := filepath.Join(t.TempDir(), "img.ext4")
	crearExt4(t, ruta, "mkdir etc", "write /dev/null etc/a")
	ctx := context.Background()
	if has, err := hasFile(ctx, ruta, "/etc/a"); err != nil || !has {
		t.Fatalf("/etc/a: %v %v, quiero true", has, err)
	}
	if has, err := hasFile(ctx, ruta, "/etc/a b"); err != nil || has {
		t.Fatalf("/etc/a b: %v %v, quiero false", has, err)
	}
	if _, err := hasFile(ctx, ruta, "/etc/a\"b"); err == nil {
		t.Fatal("una comilla en la ruta debería ser un error, no un stat")
	}
}

// Una orden que falla sin decir nada no deja un ": " colgando ("debugfs on x:
// exit status 1: ").
func TestConSalida(t *testing.T) {
	err := errors.New("exit status 1")
	if got := conSalida(err, []byte(" \n")); got != "exit status 1" {
		t.Errorf("sin salida = %q", got)
	}
	if got := conSalida(err, []byte("not here\n")); got != "exit status 1: not here" {
		t.Errorf("con salida = %q", got)
	}
}
