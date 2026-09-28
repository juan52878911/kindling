package machine

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Qué errno significa "aquí no se clona" y cuál es un fallo de verdad. Si
// ENOSPC se tomase por "no clona", un -copy intentaría copiar gigas en un
// disco que acaba de decir que está lleno.
func TestSinClonClasificaErrno(t *testing.T) {
	for _, e := range []syscall.Errno{syscall.EOPNOTSUPP, syscall.EXDEV, syscall.EINVAL, syscall.ENOTTY} {
		if !sinClon(e) {
			t.Errorf("%v debería ser 'no clona'", e)
		}
	}
	for _, e := range []syscall.Errno{syscall.ENOSPC, syscall.EIO, syscall.EDQUOT} {
		if sinClon(e) {
			t.Errorf("%v es un fallo real, no 'no clona'", e)
		}
	}
}

// FICLONE de verdad: en este sistema de ficheros o clona (y es idéntico), o
// da errSinClon con el errno dentro. Nunca otra cosa, y nunca deja el destino.
func TestClonarFicheroReal(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("datos que clonar"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := clonarFichero(src, dst)
	if err != nil {
		if !errors.Is(err, errSinClon) {
			t.Fatalf("error inesperado: %v", err)
		}
		var e syscall.Errno
		if !errors.As(err, &e) {
			t.Errorf("el error debe llevar el errno: %v", err)
		}
		if _, err := os.Stat(dst); !os.IsNotExist(err) {
			t.Error("un clon fallido no debe dejar el destino")
		}
		t.Logf("%s no clona: %v", tipoFS(dir), err)
		return
	}
	mismoContenido(t, src, dst)
	// O_EXCL: nunca pisa un fichero existente.
	if err := clonarFichero(src, dst); !errors.Is(err, os.ErrExist) {
		t.Errorf("clonar encima de un fichero existente debería fallar con ErrExist: %v", err)
	}
	t.Logf("%s clona con FICLONE", tipoFS(dir))
}

func TestTipoFSNombraLosConocidos(t *testing.T) {
	if fs := tipoFS(t.TempDir()); fs == "" {
		t.Error("tipoFS de un directorio que existe no puede ser vacío")
	}
	if fs := tipoFS("/no/existe"); fs != "" {
		t.Errorf("tipoFS de algo que no existe = %q", fs)
	}
}
