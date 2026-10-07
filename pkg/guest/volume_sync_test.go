package guest

import "testing"

// /volume/sync vacía también la raíz: una máquina sin volúmenes conserva su
// overlay al pararla, y lo que quedase en caché se perdía con el SIGKILL.
func TestSyncVaciaSinVolumenes(t *testing.T) {
	orig := syncDiscos
	t.Cleanup(func() { syncDiscos = orig })
	n := 0
	syncDiscos = func() { n++ }
	var v Volumes
	v.Sync()
	if n != 1 {
		t.Fatalf("Sync sin volúmenes vació %d veces; quería 1", n)
	}
}
