package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestDaemonBackend(t *testing.T) {
	nativo := "firecracker"
	if runtime.GOOS == "darwin" {
		nativo = "vz"
	}
	if runtime.GOOS == "darwin" && runtime.GOARCH != "arm64" {
		t.Skip("vz solo en Apple Silicon")
	}
	if b, err := daemonBackend("", "", runtime.GOOS, runtime.GOARCH); err != nil || b != nativo {
		t.Fatalf("sin ajuste = %q, %v", b, err)
	}
	// KLING_VMM con una ruta no cambia el backend, solo el binario.
	if b, err := daemonBackend("", "/tmp/otro-vmm", runtime.GOOS, runtime.GOARCH); err != nil || b != nativo {
		t.Fatalf("KLING_VMM ruta = %q, %v", b, err)
	}
	otro := "vz"
	if nativo == "vz" {
		otro = "firecracker"
	}
	if _, err := daemonBackend(otro, "", runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatalf("%s no puede correr aquí", otro)
	}
	// KLING_VMM con un nombre de backend gana al fichero.
	if _, err := daemonBackend(nativo, otro, runtime.GOOS, runtime.GOARCH); err == nil {
		t.Fatalf("KLING_VMM=%s gana al fichero y no vale aquí", otro)
	}
	if _, err := daemonBackend("qemu", "", runtime.GOOS, runtime.GOARCH); err == nil || !strings.Contains(err.Error(), "daemon.vmm") {
		t.Fatalf("backend desconocido: %v", err)
	}
}
