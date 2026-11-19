package main

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCpDirIdaYVuelta(t *testing.T) {
	src := t.TempDir()
	_ = os.MkdirAll(filepath.Join(src, "a", "b"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "a", "f.txt"), []byte("hola"), 0o600)
	_ = os.Symlink("a/f.txt", filepath.Join(src, "ln"))
	var buf bytes.Buffer
	if err := escribirTarDe(&buf, src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "sub", "out")
	res, err := extraerTarLocal(&buf, dst)
	if err != nil || res.Files != 1 || res.Dirs != 2 || res.Symlinks != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a", "f.txt")); string(b) != "hola" {
		t.Fatalf("contenido %q", b)
	}
}

// Una máquina hostil no puede escribir fuera del directorio de destino.
func TestCpDirDeMaquinaHostilNoEscapa(t *testing.T) {
	for nombre, h := range map[string]tar.Header{
		"punto-punto": {Name: "../fuera", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		"absoluta":    {Name: "/tmp/kling-fuera-test", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1},
		"enlace":      {Name: "l", Typeflag: tar.TypeSymlink, Linkname: "../../etc"},
	} {
		t.Run(nombre, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			_ = tw.WriteHeader(&h)
			if h.Size > 0 {
				_, _ = tw.Write([]byte("x"))
			}
			_ = tw.Close()
			base := t.TempDir()
			if _, err := extraerTarLocal(&buf, filepath.Join(base, "dst")); err == nil {
				t.Fatal("debía rechazarse")
			}
			if _, err := os.Lstat(filepath.Join(base, "fuera")); err == nil {
				t.Fatal("escribió fuera")
			}
			if _, err := os.Lstat("/tmp/kling-fuera-test"); err == nil {
				t.Fatal("escribió fuera")
			}
		})
	}
}
