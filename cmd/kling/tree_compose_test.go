package main

import (
	"strings"
	"testing"
)

// El árbol (ayuda y completado) nombra exactamente los subcomandos que
// cmdCompose sirve: antes decía solo up y down.
func TestArbolCompose(t *testing.T) {
	hermetic(t)
	t.Setenv("KLING_HOST", "unix:///nonexistent/kling.sock")
	c := coreCommand("compose")
	if c == nil {
		t.Fatal("compose no está en el árbol")
	}
	for _, sub := range c.Subcommands {
		err := cmdCompose([]string{"-f", "/nonexistent/compose.yaml", sub})
		if err != nil && strings.Contains(err.Error(), "unknown subcommand") {
			t.Errorf("el árbol dice compose %s y cmdCompose no lo conoce", sub)
		}
		if b := subBlock(c, sub); b == "" {
			t.Errorf("compose %s sin línea de uso en el árbol", sub)
		}
	}
	if err := cmdCompose([]string{"-f", "/nonexistent/compose.yaml", "bogus"}); err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Errorf("bogus: %v", err)
	}
}
