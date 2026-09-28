//go:build darwin

package machine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// En APFS se clona; fuera, copia completa pasando por la comprobación de sitio.
func TestClonarDiscoEnMac(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("contenido"), 0o644); err != nil {
		t.Fatal(err)
	}
	llamada := false
	modo, err := clonarDisco(context.Background(), src, dst, func() error { llamada = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	want := "copy"
	if esAPFS(dir) {
		want = "clone"
	}
	if modo != want || llamada != (want == "copy") {
		t.Errorf("modo=%q (quería %q), comprobación de sitio=%v", modo, want, llamada)
	}
	if b, _ := os.ReadFile(dst); string(b) != "contenido" {
		t.Errorf("copia mal: %q", b)
	}
}
