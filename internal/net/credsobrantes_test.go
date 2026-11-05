package net

import (
	"fmt"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Cuando un nodo pierde su última arista credential, SetGraph quita los DNAT y
// ACCEPT de los proxies de credenciales; los de DNS y enlaces no se tocan.
func TestReglasCredSobrantes(t *testing.T) {
	n := Plan(5, "0123456789abcdef")
	h := n.HostIP
	// Las líneas tal como las devuelve `iptables -S` tras reglasGrafo(_, true).
	credNAT := []string{
		fmt.Sprintf("-A PREROUTING -d %s/32 -i %s -p tcp -m tcp --dport 80 -j DNAT --to-destination %s:%d", h, TapName, h, credPort),
		fmt.Sprintf("-A PREROUTING -d %s/32 -i %s -p tcp -m tcp --dport 443 -j DNAT --to-destination %s:%d", h, TapName, h, credPort),
		fmt.Sprintf("-A PREROUTING -d %s/32 -i %s -p tcp -m tcp --dport %d -j DNAT --to-destination %s:%d", h, TapName, credproxy.MySQLDefaultPort, h, myPort),
		fmt.Sprintf("-A PREROUTING -d %s/32 -i %s -p tcp -j DNAT --to-destination %s:%d", h, TapName, h, pgPort),
	}
	credFwd := []string{}
	for _, p := range []int{credPort, pgPort, myPort} {
		credFwd = append(credFwd, fmt.Sprintf("-A FORWARD -d %s/32 -i %s -o %s -p tcp -m tcp --dport %d -m conntrack ! --ctstate INVALID -j ACCEPT", h, TapName, n.NSIf, p))
	}
	otrasNAT := []string{
		"-P PREROUTING ACCEPT",
		fmt.Sprintf("-A PREROUTING -i %s -p udp -m udp --dport 53 -j DNAT --to-destination %s:%d", TapName, h, dnsPort),
		fmt.Sprintf("-A PREROUTING -d %s/32 -i %s -p tcp -m tcp --dport 5432 -j DNAT --to-destination %s:%d", h, TapName, h, linkBasePortRango),
		// Un DNAT a pgPort de OTRA dirección no es de este netns.
		fmt.Sprintf("-A PREROUTING -i %s -p tcp -j DNAT --to-destination 10.9.9.9:%d", TapName, pgPort),
		"",
	}
	otrasFwd := []string{
		"-P FORWARD DROP",
		fmt.Sprintf("-A FORWARD -d %s/32 -i %s -o %s -p udp -m udp --dport %d -j ACCEPT", h, TapName, n.NSIf, dnsPort),
		fmt.Sprintf("-A FORWARD -d %s/32 -i %s -o %s -p tcp -m tcp --dport %d -m conntrack ! --ctstate INVALID -j ACCEPT", h, TapName, n.NSIf, linkBasePortRango),
		// Un DROP al puerto del proxy no es un ACCEPT nuestro.
		fmt.Sprintf("-A FORWARD -d %s/32 -p tcp -m tcp --dport %d -j DROP", h, credPort),
	}

	nat := append(append([]string{}, otrasNAT...), credNAT...)
	fwd := append(append([]string{}, otrasFwd...), credFwd...)
	got := map[string]bool{}
	for _, r := range n.reglasCredSobrantes(nat, fwd) {
		got[strings.Join(r, " ")] = true
	}
	want := map[string]bool{}
	for _, l := range credNAT {
		want["-t nat "+strings.Replace(l, "-A", "-D", 1)] = true
	}
	for _, l := range credFwd {
		want[strings.Replace(l, "-A", "-D", 1)] = true
	}
	if len(got) != len(want) {
		t.Errorf("sobran %d reglas, quería %d: %v", len(got), len(want), got)
	}
	for r := range want {
		if !got[r] {
			t.Errorf("no se borra: %s", r)
		}
	}

	// Sin reglas de credenciales no sobra nada.
	if r := n.reglasCredSobrantes(otrasNAT, otrasFwd); len(r) != 0 {
		t.Errorf("sin credenciales sobran %v", r)
	}
	// Las de enlace siguen siendo cosa de reglasEnlaceSobrantes: las de
	// credenciales no le cuentan.
	if r := n.reglasEnlaceSobrantes(credNAT, credFwd, nil); len(r) != 0 {
		t.Errorf("reglasEnlaceSobrantes toca las de credenciales: %v", r)
	}
}
