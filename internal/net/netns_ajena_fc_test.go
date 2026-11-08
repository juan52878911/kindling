//go:build !darwin

package net

import (
	"errors"
	"testing"
)

// Un netns kl-<id> que ya existe con el veth en otra /30 es de una máquina
// viva de otro daemon (mismo prefijo de id). Setup no lo derriba: falla antes
// de tocar nada.
func TestSetupNoDerribaLaRedDeOtroDaemon(t *testing.T) {
	oldE, oldI, oldX := existeRed, ipsVeth, ejecutar
	t.Cleanup(func() { existeRed, ipsVeth, ejecutar = oldE, oldI, oldX })
	existeRed = func(*Net) bool { return true }
	ipsVeth = func(string) []string { return []string{"172.30.3.41"} }
	var cmds [][]string
	ejecutar = func(args ...string) error { cmds = append(cmds, args); return nil }

	n := Plan(5, "deadbeef00000001")
	err := n.Setup(EgressNone, nil, 0)
	if !errors.Is(err, ErrRedAjena) {
		t.Fatalf("Setup = %v, quería ErrRedAjena", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("ejecutó %v sobre una red ajena", cmds)
	}
}
