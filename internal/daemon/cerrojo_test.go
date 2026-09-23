package daemon

import (
	"strings"
	"testing"
)

// Un segundo daemon sobre la misma raíz tiene que negarse antes de tocar nada:
// si arranca, mata los VMM del primero como huérfanos.
func TestBloquearRaizExcluyeSegundoDaemon(t *testing.T) {
	root := t.TempDir()
	a, err := bloquearRaiz(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bloquearRaiz(root); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("segundo cerrojo: err = %v; quería 'already running'", err)
	}
	// Al soltarlo (el proceso muere), el siguiente entra.
	a.Close()
	b, err := bloquearRaiz(root)
	if err != nil {
		t.Fatalf("tras soltarlo: %v", err)
	}
	b.Close()
}
