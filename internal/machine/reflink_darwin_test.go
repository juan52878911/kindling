package machine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func discoDePrueba(t *testing.T) (dir, src string, datos []byte) {
	t.Helper()
	dir = t.TempDir()
	if !esAPFS(dir) {
		t.Skip("el directorio temporal no está en APFS")
	}
	src = filepath.Join(dir, "src.ext4")
	datos = bytes.Repeat([]byte("kling"), 4096)
	if err := os.WriteFile(src, datos, 0o640); err != nil {
		t.Fatal(err)
	}
	return dir, src, datos
}

// copiarDisco y clonarDisco clonan sin `cp`: con fclonefileat la copia sale
// igual y con el modo del origen.
func TestCopiarDiscoClonaSinCp(t *testing.T) {
	dir, src, datos := discoDePrueba(t)
	t.Setenv("PATH", "") // sin cp a mano: si lo lanzase, fallaría
	dst := filepath.Join(dir, "dst.ext4")
	if out, err := copiarDisco(context.Background(), src, dst); err != nil {
		t.Fatalf("copiarDisco: %v %s", err, out)
	}
	dst2 := filepath.Join(dir, "dst2.ext4")
	modo, err := clonarDisco(context.Background(), src, dst2, func() error { t.Fatal("no es una copia completa"); return nil })
	if err != nil || modo != "clone" {
		t.Fatalf("clonarDisco = %q, %v", modo, err)
	}
	for _, p := range []string{dst, dst2} {
		b, err := os.ReadFile(p)
		if err != nil || !bytes.Equal(b, datos) {
			t.Fatalf("%s no es la copia: %v", p, err)
		}
		if st, _ := os.Stat(p); st.Mode().Perm() != 0o640 {
			t.Fatalf("%s con modo %v, quiero el del origen", p, st.Mode().Perm())
		}
	}
	if err := clonarVolFichero(src, dst); !errors.Is(err, os.ErrExist) {
		t.Fatalf("clonarVolFichero sobre un destino existente: %v", err)
	}
}

// Si fclonefileat no puede (ENOTSUP, EXDEV), copiarDisco cae a `cp -c` como
// siempre y clonarVolFichero contesta errSinClon, sin copiar.
func TestCopiarDiscoSinClonCaeACp(t *testing.T) {
	dir, src, datos := discoDePrueba(t)
	orig := fclonefileat
	fclonefileat = func(int, int, string, int) error { return syscall.ENOTSUP }
	t.Cleanup(func() { fclonefileat = orig })
	dst := filepath.Join(dir, "dst.ext4")
	if out, err := copiarDisco(context.Background(), src, dst); err != nil {
		t.Fatalf("copiarDisco: %v %s", err, out)
	}
	if b, _ := os.ReadFile(dst); !bytes.Equal(b, datos) {
		t.Fatal("la copia de cp no es el origen")
	}
	dst2 := filepath.Join(dir, "dst2.ext4")
	if err := clonarVolFichero(src, dst2); !errors.Is(err, errSinClon) {
		t.Fatalf("clonarVolFichero sin clon = %v, quiero errSinClon", err)
	}
	if _, err := os.Lstat(dst2); !os.IsNotExist(err) {
		t.Fatal("clonarVolFichero sin clon dejó un fichero")
	}
}
