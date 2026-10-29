package machine

import (
	"os"
	"path/filepath"
	"testing"
)

// Cuenta las congeladas (sello volcado.ok) y los dorados (meta.json) de ese
// VMM, y no los de otro.
func TestVolcadosDeVMM(t *testing.T) {
	root := t.TempDir()
	escribe := func(rel, datos string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(datos), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	escribe("machines/a/"+marcaOK, `{"vmm":"kling-vz 0.18.0"}`)
	escribe("machines/b/"+marcaOK, `{"vmm":"kling-hv 0.19.0"}`)
	escribe("machines/c/overlay.ext4", "")
	escribe("snapshots/d/meta.json", `{"vmm":"kling-vz 0.18.0"}`)
	escribe("snapshots/e/meta.json", `{"vmm":"firecracker 1.12.0"}`)
	// La papelera, una tumba o un temporal no cuentan (B5).
	escribe("machines/.trash-x/"+marcaOK, `{"vmm":"kling-vz 0.18.0"}`)
	escribe("snapshots/.tmp-f/meta.json", `{"vmm":"kling-vz 0.18.0"}`)
	if n := VolcadosDeVMM(root, "kling-vz"); n != 2 {
		t.Fatalf("kling-vz = %d, want 2", n)
	}
	if n := VolcadosDeVMM(root, "kling-hv"); n != 1 {
		t.Fatalf("kling-hv = %d, want 1", n)
	}
	if n := VolcadosDeVMM(filepath.Join(root, "no"), "kling-vz"); n != 0 {
		t.Fatalf("raíz vacía = %d", n)
	}
}
