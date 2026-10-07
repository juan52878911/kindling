package main

import (
	"strings"
	"testing"
)

// Los argumentos que puso quien importa no se repiten en la salida.
func TestRunsLine(t *testing.T) {
	argv := []string{"postgres", "-c", "password=secreto"}
	if got := runsLine(argv, false); got != "postgres -c password=secreto" {
		t.Fatalf("de la imagen: %q", got)
	}
	got := runsLine(argv, true)
	if strings.Contains(got, "secreto") || !strings.HasPrefix(got, "postgres") || !strings.Contains(got, "2 arguments") {
		t.Fatalf("de la línea de órdenes: %q", got)
	}
}

// -restart se comprueba antes de hablar con el daemon (no hay ninguno).
func TestImagesImportRestartFlag(t *testing.T) {
	err := imagesImport([]string{"-H", "/nonexistent/kling.sock", "-restart", "sometimes", "redis:7"})
	if err == nil || !strings.Contains(err.Error(), "-restart must be") {
		t.Fatalf("err = %v", err)
	}
}
