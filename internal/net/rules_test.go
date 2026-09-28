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

// Los DNAT del proxy de credenciales: 80 y 443 al HTTP, y DESPUÉS el resto de
// puertos TCP de la IP del proxy al de Postgres (el orden deja fuera el 80 y
// el 443; el 53 lo toma antes el DNAT del DNS). El FORWARD deja pasar los dos.
func TestCredNATRulesOrden(t *testing.T) {
	n := Plan(1, "abcdef0123")
	nat := n.credNATRules()
	want := []string{
		"iptables -t nat -A PREROUTING -i tap0 -p tcp -d " + n.HostIP + " --dport 80 -j DNAT --to-destination " + n.HostIP + ":5380",
		"iptables -t nat -A PREROUTING -i tap0 -p tcp -d " + n.HostIP + " --dport 443 -j DNAT --to-destination " + n.HostIP + ":5380",
		"iptables -t nat -A PREROUTING -i tap0 -p tcp -d " + n.HostIP + " -j DNAT --to-destination " + n.HostIP + ":5381",
	}
	if len(nat) != len(want) {
		t.Fatalf("nat = %v", nat)
	}
	for i := range want {
		if got := strings.Join(nat[i], " "); got != want[i] {
			t.Errorf("regla %d = %q\n quería %q", i, got, want[i])
		}
	}
	fwd := n.credForwardRules()
	if len(fwd) != 2 {
		t.Fatalf("forward = %v", fwd)
	}
	for i, puerto := range []string{"5380", "5381"} {
		want := "iptables -A FORWARD -i tap0 -o " + n.NSIf + " -p tcp -d " + n.HostIP + " --dport " + puerto + " -j ACCEPT"
		if got := strings.Join(fwd[i], " "); got != want {
			t.Errorf("forward %d = %q", i, got)
		}
	}
}
