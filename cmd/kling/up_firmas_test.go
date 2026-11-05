package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// tieneEntitlement pregunta a codesign una vez por fichero; si el fichero
// cambia (otra fecha), vuelve a preguntar.
func TestTieneEntitlementRecuerda(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("codesign es de macOS")
	}
	if _, err := exec.LookPath("codesign"); err != nil {
		t.Skip("sin codesign")
	}
	bin := filepath.Join(t.TempDir(), "ls")
	b, err := os.ReadFile("/bin/ls")
	if err != nil {
		t.Skip(err)
	}
	if err := os.WriteFile(bin, b, 0o755); err != nil {
		t.Fatal(err)
	}
	cuantas := func() int {
		firmasMu.Lock()
		defer firmasMu.Unlock()
		n := 0
		for k := range firmas {
			if len(k) > len(bin) && k[:len(bin)] == bin {
				n++
			}
		}
		return n
	}
	if tieneEntitlement(bin, "com.apple.security.hypervisor") {
		t.Fatal("/bin/ls no lleva el permiso del hipervisor")
	}
	_ = tieneEntitlement(bin, "com.apple.security.virtualization")
	if n := cuantas(); n != 1 {
		t.Fatalf("%d entradas para un fichero, want 1", n)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(bin, later, later); err != nil {
		t.Fatal(err)
	}
	_ = tieneEntitlement(bin, "com.apple.security.hypervisor")
	if n := cuantas(); n != 2 {
		t.Fatalf("%d entradas tras cambiar la fecha, want 2", n)
	}
}
