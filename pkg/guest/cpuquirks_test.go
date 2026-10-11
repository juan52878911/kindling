package guest

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestCPUQuirkSVE2SinSVE(t *testing.T) {
	dir := t.TempDir()
	viejoInfo, viejoArch := cpuinfoPath, goarch
	t.Cleanup(func() { cpuinfoPath, goarch = viejoInfo, viejoArch })
	cpuinfoPath = filepath.Join(dir, "cpuinfo")
	escribir := func(features string) {
		os.WriteFile(cpuinfoPath, []byte("processor\t: 0\nBogoMIPS\t: 48.00\nFeatures\t: "+features+"\nCPU implementer\t: 0x61\n"), 0o644)
	}
	base := []string{"PATH=/bin"}

	goarch = "arm64"
	// El de un M4 bajo vz (medido): sve2 sin sve.
	escribir("fp asimd aes pmull sha1 sha2 sha512 sve2 flagm2 frint svei8mm svebf16 i8mm bf16 bti")
	if got := cpuQuirkEnv(base); !slices.Contains(got, "OPENSSL_armcap=0x7d") {
		t.Fatalf("M4: %v, quiero OPENSSL_armcap", got)
	}
	// La imagen ya trae uno: se respeta.
	if got := cpuQuirkEnv([]string{"OPENSSL_armcap=0x1"}); len(got) != 1 {
		t.Fatalf("pisó el de la imagen: %v", got)
	}
	// Con SVE de verdad (Graviton 3), o sin ninguno, nada.
	for _, f := range []string{"fp asimd sve sve2 svei8mm", "fp asimd aes pmull"} {
		escribir(f)
		if got := cpuQuirkEnv(base); len(got) != 1 {
			t.Fatalf("%q: %v, no quiero nada", f, got)
		}
	}
	// En amd64 no se mira.
	goarch = "amd64"
	escribir("fp asimd sve2")
	if got := cpuQuirkEnv(base); len(got) != 1 {
		t.Fatalf("amd64: %v", got)
	}
}
