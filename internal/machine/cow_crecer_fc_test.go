//go:build !darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// El almacén de verdad crece en caliente (root, KLING_TEST_MOUNTS=1, el
// sistema de ficheros en el núcleo y sus herramientas): se crea de 1 GiB con
// una instancia dentro, se agranda 512 MiB y la instancia se sigue leyendo.
func TestCrecerAlmacenDeVerdad(t *testing.T) {
	if os.Geteuid() != 0 || os.Getenv("KLING_TEST_MOUNTS") != "1" {
		t.Skip("needs root and KLING_TEST_MOUNTS=1")
	}
	for _, fs := range []string{"xfs", "btrfs"} {
		t.Run(fs, func(t *testing.T) {
			bin, _ := argsCrecerFS(fs, "")
			if b, _ := os.ReadFile("/proc/filesystems"); !soportados(string(b))[fs] || buscarE2fs("mkfs."+fs) == "" || buscarE2fs(bin) == "" {
				t.Skipf("needs %s in the kernel, mkfs.%s and %s", fs, fs, bin)
			}
			root := t.TempDir()
			a := nuevoAlmacen(root, &Privileges{})
			a.candidatos = func() []string { return []string{fs} }
			a.mu.Lock()
			a.usarFS(fs)
			a.mu.Unlock()
			t.Cleanup(func() { _ = syscall.Unmount(a.dir, syscall.MNT_DETACH) })
			src := filepath.Join(root, "dorado.ext4")
			if err := os.WriteFile(src, []byte("disco de verdad"), 0o600); err != nil {
				t.Fatal(err)
			}
			ruta, err := a.clonarInstancia(context.Background(), "d", src, "id1", 1)
			if err != nil {
				t.Fatal(err)
			}
			antes := a.info()
			if err := a.crecer(context.Background(), 0, 512<<20); err != nil {
				t.Fatal(err)
			}
			despues := a.info()
			if despues == nil || despues.SizeMiB < antes.SizeMiB+400 {
				t.Errorf("no creció: %+v -> %+v", antes, despues)
			}
			var st syscall.Stat_t
			if err := syscall.Stat(a.img, &st); err != nil {
				t.Fatal(err)
			}
			if ocupa := int64(st.Blocks) * 512; ocupa < st.Size || st.Size != 1536<<20 {
				t.Errorf("la imagen mide %d MiB y ocupa %d: no está reservada entera", st.Size>>20, ocupa>>20)
			}
			if b, err := os.ReadFile(ruta); err != nil || string(b) != "disco de verdad" {
				t.Errorf("la instancia lee %q %v", b, err)
			}
			// Repetir con el mismo tamaño no falla (completa uno a medias).
			if err := a.crecer(context.Background(), 1536<<20, 0); err != nil {
				t.Errorf("mismo tamaño: %v", err)
			}
		})
	}
}
