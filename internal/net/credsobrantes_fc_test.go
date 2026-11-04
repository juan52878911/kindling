//go:build !darwin

package net

import (
	"strings"
	"testing"
)

// reglasGrafo con creds produce exactamente lo que reglasCredSobrantes sabe
// reconocer: mismos destinos y puertos (en formato -S, -d lleva /32).
func TestReglasGrafoCredsSeReconocen(t *testing.T) {
	n := Plan(7, "fedcba9876543210")
	var nat, fwd []string
	for _, r := range n.reglasGrafo(EgressNone, true) {
		l := strings.Join(r, " ")
		l = strings.Replace(l, "-I FORWARD 1", "-A FORWARD", 1)
		l = strings.TrimPrefix(l, "-t nat ")
		if strings.HasPrefix(l, "-A PREROUTING") {
			nat = append(nat, l)
		} else if strings.HasPrefix(l, "-A FORWARD") {
			fwd = append(fwd, l)
		}
	}
	// 4 DNAT y 3 ACCEPT de credenciales; los del DNS no cuentan.
	if r := n.reglasCredSobrantes(nat, fwd); len(r) != 7 {
		t.Fatalf("se reconocen %d reglas de credenciales, quería 7: %v", len(r), r)
	}
}
