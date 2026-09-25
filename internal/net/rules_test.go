package net

import (
	"strings"
	"testing"
)

// Las reglas en lote tienen que ser las mismas y en el mismo orden que las
// que ponía applyEgress una a una: la primera que casa, gana.
func TestEgressRulesOrden(t *testing.T) {
	n := Plan(1, "abcdef0123")
	none := n.egressRules(EgressNone)
	if len(none) != 2 || !strings.Contains(strings.Join(none[0], " "), "ESTABLISHED,RELATED -j ACCEPT") ||
		!strings.Contains(strings.Join(none[1], " "), "-o vg-abcdef01 -m conntrack --ctstate NEW -j DROP") {
		t.Fatalf("none = %v", none)
	}
	inet := n.egressRules(EgressInternet)
	if len(inet) != 2+len(blocked) {
		t.Fatalf("internet: %d reglas", len(inet))
	}
	if strings.Join(inet[len(inet)-1], " ") != "-t nat -A POSTROUTING -o vg-abcdef01 -j MASQUERADE" {
		t.Errorf("la última de internet debe ser el MASQUERADE: %v", inet[len(inet)-1])
	}
	for i, cidr := range blocked {
		if got := strings.Join(inet[1+i], " "); got != "-A FORWARD -i tap0 -d "+cidr+" -j DROP" {
			t.Errorf("regla %d = %q", i, got)
		}
	}
}
