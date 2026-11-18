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
	oldE, oldI, oldX, oldC := existeRed, ipsVeth, ejecutar, comprobarEstado
	t.Cleanup(func() {
		existeRed, ipsVeth, ejecutar, comprobarEstado = oldE, oldI, oldX, oldC
		inputMontado.Store(false)
	})
	existeRed = func(*Net) bool { return true }
	ipsVeth = func(string) []string { return []string{"172.30.3.41"} }
	var cmds [][]string
	ejecutar = func(args ...string) error { cmds = append(cmds, args); return nil }
	// Las reglas del host, en su sitio.
	comprobarEstado = func([]string) (bool, error) { return true, nil }
	inputMontado.Store(true)

	n := Plan(5, "deadbeef00000001")
	err := n.Setup(EgressNone, nil, 0)
	if !errors.Is(err, ErrRedAjena) {
		t.Fatalf("Setup = %v, quería ErrRedAjena", err)
	}
	if len(cmds) != 0 {
		t.Fatalf("ejecutó %v sobre una red ajena", cmds)
	}
}

// S2: sin las reglas del host (SetupHost falló o no corrió), Setup no monta
// nada: la máquina no arranca con el host abierto.
func TestSetupSinBarrerasDelHostNoMontaNada(t *testing.T) {
	oldE, oldX := existeRed, ejecutar
	t.Cleanup(func() { existeRed, ejecutar = oldE, oldX })
	existeRed = func(*Net) bool {
		t.Fatal("Setup siguió sin las reglas del host")
		return false
	}
	ejecutar = func(args ...string) error { t.Fatalf("Setup ejecutó %v sin las reglas del host", args); return nil }
	inputMontado.Store(false)
	accesoPedido.Store(nil)

	err := Plan(6, "deadbeef00000002").Setup(EgressInternet, nil, 0)
	if !errors.Is(err, ErrHostSinBarreras) {
		t.Fatalf("Setup = %v, quería ErrHostSinBarreras", err)
	}
}
