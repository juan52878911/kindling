package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

// Lo que se mete en la imagen es lo que se validó: el from_host se abre una
// vez y se copia desde ese descriptor. Cambiar el fichero de lib después (por
// un enlace a algo de fuera, digamos) no cambia lo que llega a la imagen, y un
// enlace que sale de lib no se abre.
func TestCopiarDeLibUsaLoQueValido(t *testing.T) {
	lib, fuera, build := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(lib, "agente"), []byte("el de lib"), 0o644); err != nil {
		t.Fatal(err)
	}
	ajeno := filepath.Join(fuera, "ajeno")
	if err := os.WriteFile(ajeno, []byte("de fuera de lib"), 0o600); err != nil {
		t.Fatal(err)
	}

	p, borrar, err := copiarDeLib(lib, "agente", build)
	if err != nil {
		t.Fatal(err)
	}
	defer borrar()
	if err := os.Remove(filepath.Join(lib, "agente")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ajeno, filepath.Join(lib, "agente")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "el de lib" {
		t.Fatalf("lo que va a la imagen = %q, %v; quería lo que se validó", b, err)
	}
	if _, _, err := copiarDeLib(lib, "agente", build); err == nil {
		t.Fatal("abrió un enlace que sale de lib")
	}
	borrar()
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("la copia sigue tras borrarla: %v", err)
	}
}
