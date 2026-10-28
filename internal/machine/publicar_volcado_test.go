//go:build darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// El daemon publica el volcado que el VMM dejó en su directorio, y no se fía
// de lo que haya ahí: enlaces, hardlinks, un destino que ya existe o el
// directorio cambiado por un enlace no llegan al dorado.
func TestPublicarVolcado(t *testing.T) {
	m := &Manager{}
	ctx := context.Background()
	raiz := t.TempDir()
	mdir, dorado := filepath.Join(raiz, "maquina"), filepath.Join(raiz, "dorado")
	for _, d := range []string{mdir, dorado} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	escribe := func(p, s string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	escribe(filepath.Join(mdir, volcadoTmpMem), "memoria")
	dst := filepath.Join(dorado, "mem.file")
	if err := m.publicarVolcado(ctx, mdir, volcadoTmpMem, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "memoria" {
		t.Fatalf("mem.file = %q", b)
	}
	// Lo que el VMM escriba después en el origen no llega al dorado.
	escribe(filepath.Join(mdir, volcadoTmpMem), "otra cosa")
	if b, _ := os.ReadFile(dst); string(b) != "memoria" {
		t.Fatalf("el dorado cambió con el origen: %q", b)
	}
	// Nunca pisa un fichero que ya está.
	if err := m.publicarVolcado(ctx, mdir, volcadoTmpMem, dst); err == nil {
		t.Fatal("pisó un mem.file existente")
	}

	secreto := filepath.Join(raiz, "secreto")
	escribe(secreto, "clave")
	casos := []struct {
		nombre  string
		prepara func(p string) error
	}{
		{"enlace simbólico", func(p string) error { return os.Symlink(secreto, p) }},
		{"hardlink", func(p string) error { return os.Link(secreto, p) }},
		{"directorio", func(p string) error { return os.Mkdir(p, 0o700) }},
	}
	for i, c := range casos {
		os.RemoveAll(filepath.Join(mdir, volcadoTmpSnap))
		if err := c.prepara(filepath.Join(mdir, volcadoTmpSnap)); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dorado, "snap"+strings.Repeat("x", i)+".file")
		if err := m.publicarVolcado(ctx, mdir, volcadoTmpSnap, dst); err == nil {
			t.Fatalf("%s: publicado", c.nombre)
		}
		if _, err := os.Lstat(dst); err == nil {
			t.Fatalf("%s: dejó %s", c.nombre, dst)
		}
	}

	// El directorio de la máquina cambiado por un enlace a otro con un
	// volcado: no se sigue.
	otro := filepath.Join(raiz, "otro")
	if err := os.Mkdir(otro, 0o700); err != nil {
		t.Fatal(err)
	}
	escribe(filepath.Join(otro, volcadoTmpMem), "de otro")
	enlace := filepath.Join(raiz, "enlace")
	if err := os.Symlink(otro, enlace); err != nil {
		t.Fatal(err)
	}
	if err := m.publicarVolcado(ctx, enlace, volcadoTmpMem, filepath.Join(dorado, "x.file")); err == nil {
		t.Fatal("siguió el directorio de la máquina cambiado por un enlace")
	}
	quitarVolcadoTemporal(enlace)
	if _, err := os.Stat(filepath.Join(otro, volcadoTmpMem)); err != nil {
		t.Fatal("borró a través del enlace")
	}

	quitarVolcadoTemporal(mdir)
	if _, err := os.Lstat(filepath.Join(mdir, volcadoTmpMem)); err == nil {
		t.Fatal("el volcado temporal sigue en el directorio de la máquina")
	}
}
