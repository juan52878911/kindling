package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// `kling mcp import a.b` se niega antes de tocar el daemon: el punto separa
// servicio y herramienta en /mcp/_all (ver mcp.ValidServiceName).
func TestImportRechazaServicioConPunto(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "no-daemon.sock")
	err := mcpImport([]string{"-H", sock, "a.b"})
	if err == nil || !strings.Contains(err.Error(), "invalid service name") {
		t.Fatalf("mcpImport(a.b) = %v", err)
	}
}
