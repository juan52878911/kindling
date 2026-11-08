package machine

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Un VMM comprometido (sin jailer) es dueño del directorio del snapshot y puede
// cambiar mem.file por un enlace a un fichero de root entre que el daemon mira
// la ruta y le hace el chmod. Con chmod por ruta, el chmod sigue el enlace y
// cambia el modo del fichero de fuera. Con fchmod sobre un fd abierto con
// O_NOFOLLOW, no.
func TestEnsureReadableCarreraConEnlace(t *testing.T) {
	dentro, fuera := t.TempDir(), t.TempDir()
	victima := filepath.Join(fuera, "shadow")
	if err := os.WriteFile(victima, []byte("x"), 0o604); err != nil {
		t.Fatal(err)
	}
	mem := filepath.Join(dentro, "mem.file")
	var parar atomic.Bool
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		tmpF, tmpL := filepath.Join(dentro, ".f"), filepath.Join(dentro, ".l")
		for !parar.Load() {
			_ = os.WriteFile(tmpF, []byte("ram"), 0o644)
			_ = os.Rename(tmpF, mem)
			_ = os.Symlink(victima, tmpL)
			_ = os.Rename(tmpL, mem)
		}
	}()
	p := &Privileges{Enabled: true, UID: os.Getuid(), GID: os.Getgid()}
	for i := 0; i < 3000; i++ {
		p.EnsureReadable(dentro)
		cerrarVolcado(dentro)
		if m := modo(t, victima); m != 0o604 {
			parar.Store(true)
			<-hecho
			t.Fatalf("el fichero de fuera pasó a %o en la vuelta %d: se siguió el enlace", m, i)
		}
	}
	parar.Store(true)
	<-hecho
}
