package net

import (
	stdnet "net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Las reglas en lote tienen que ser las mismas y en el mismo orden que las
// que pone applyEgress una a una: la primera que casa, gana.
func TestEgressRulesOrden(t *testing.T) {
	n := Plan(1, "abcdef0123")
	none := n.egressRules(EgressNone)
	if len(none) != 3 || strings.Join(none[0], " ") != "-A FORWARD -i tap0 -m conntrack --ctstate INVALID -j DROP" ||
		!strings.Contains(strings.Join(none[1], " "), "ESTABLISHED,RELATED -j ACCEPT") ||
		strings.Join(none[2], " ") != "-A FORWARD -i tap0 -j DROP" {
		t.Fatalf("none = %v", none)
	}
	inet := n.egressRules(EgressInternet)
	if len(inet) != 3+len(blocked) {
		t.Fatalf("internet: %d reglas", len(inet))
	}
	if strings.Join(inet[len(inet)-1], " ") != "-t nat -A POSTROUTING -o vg-abcdef01 -j MASQUERADE" {
		t.Errorf("la última de internet debe ser el MASQUERADE: %v", inet[len(inet)-1])
	}
	for i, cidr := range blocked {
		if got := strings.Join(inet[2+i], " "); got != "-A FORWARD -i tap0 -d "+cidr+" -j DROP" {
			t.Errorf("regla %d = %q", i, got)
		}
	}
}

// Los DNAT del proxy de credenciales: 80 y 443 al HTTP, el 3306 al listener de
// MySQL y DESPUÉS el resto de puertos TCP de la IP del proxy al de bases de
// datos (el orden deja fuera el 80, el 443 y el 3306; el 53 lo toma antes el
// DNAT del DNS). El FORWARD deja pasar los tres.
func TestCredNATRulesOrden(t *testing.T) {
	n := Plan(1, "abcdef0123")
	nat := n.credNATRules()
	want := []string{
		"iptables -t nat -A PREROUTING -i tap0 -p tcp -d " + n.HostIP + " --dport 80 -j DNAT --to-destination " + n.HostIP + ":5380",
		"iptables -t nat -A PREROUTING -i tap0 -p tcp -d " + n.HostIP + " --dport 443 -j DNAT --to-destination " + n.HostIP + ":5380",
		"iptables -t nat -A PREROUTING -i tap0 -p tcp -d " + n.HostIP + " --dport 3306 -j DNAT --to-destination " + n.HostIP + ":5382",
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
	if len(fwd) != 3 {
		t.Fatalf("forward = %v", fwd)
	}
	for i, puerto := range []string{"5380", "5381", "5382"} {
		want := "iptables -A FORWARD -i tap0 -o " + n.NSIf + " -p tcp -d " + n.HostIP + " --dport " + puerto + " -j ACCEPT"
		if got := strings.Join(fwd[i], " "); got != want {
			t.Errorf("forward %d = %q", i, got)
		}
	}
}

// paquete es lo que mira veredicto de un paquete que cruza FORWARD.
type paquete struct {
	in, out, src, dst, proto string
	dport                    int
	estado                   string // NEW, ESTABLISHED, INVALID, UNTRACKED...
	enSet                    bool   // la IP destino está en el ipset del allowlist
}

// veredicto recorre las reglas de FORWARD de la tabla filter (argumentos de
// iptables, con o sin "iptables" delante; el resto se ignora) y devuelve el
// target de la primera que casa, o ACCEPT, la política por defecto del
// FORWARD, si no casa ninguna. Entiende solo lo que usan las reglas de
// firewall.go: un argumento desconocido tumba el test en vez de casar a ciegas.
func veredicto(t *testing.T, reglas [][]string, p paquete) string {
	t.Helper()
	for _, r := range reglas {
		if len(r) > 0 && r[0] == "iptables" {
			r = r[1:]
		} else if len(r) == 0 || !strings.HasPrefix(r[0], "-") {
			continue // ipset, sysctl...
		}
		if objetivo, ok := casa(t, r, p); ok {
			return objetivo
		}
	}
	return "ACCEPT"
}

func casa(t *testing.T, r []string, p paquete) (string, bool) {
	t.Helper()
	cidr := func(c, ip string) bool {
		if !strings.Contains(c, "/") {
			c += "/32"
		}
		_, red, err := stdnet.ParseCIDR(c)
		if err != nil {
			t.Fatalf("CIDR %q en %v: %v", c, r, err)
		}
		return red.Contains(stdnet.ParseIP(ip))
	}
	iface := func(patron, nombre string) bool {
		if strings.HasSuffix(patron, "+") {
			return strings.HasPrefix(nombre, strings.TrimSuffix(patron, "+"))
		}
		return patron == nombre
	}
	objetivo, niega := "", false
	for i := 0; i < len(r); i++ {
		var m bool
		switch r[i] {
		case "-t":
			if r[i+1] != "filter" {
				return "", false
			}
			i++
			continue
		case "-A", "-I":
			if r[i+1] != "FORWARD" {
				return "", false
			}
			if r[i] == "-I" && i+2 < len(r) {
				if _, err := strconv.Atoi(r[i+2]); err == nil {
					i++
				}
			}
			i++
			continue
		case "!":
			niega = true
			continue
		case "-m":
			i++
			continue
		case "-j":
			objetivo = r[i+1]
			i++
			continue
		case "-i":
			m = iface(r[i+1], p.in)
		case "-o":
			m = iface(r[i+1], p.out)
		case "-s":
			m = cidr(r[i+1], p.src)
		case "-d":
			m = cidr(r[i+1], p.dst)
		case "-p":
			m = r[i+1] == p.proto
		case "--dport":
			m = r[i+1] == strconv.Itoa(p.dport)
		case "--ctstate":
			for _, e := range strings.Split(r[i+1], ",") {
				m = m || e == p.estado
			}
		case "--match-set":
			m = p.enSet
			i++ // el "dst"
		default:
			t.Fatalf("veredicto no entiende %q en %v", r[i], r)
		}
		i++
		if niega {
			m, niega = !m, false
		}
		if !m {
			return "", false
		}
	}
	return objetivo, objetivo != ""
}

// Lo que conntrack marca INVALID (un TCP con solo RST o FIN y carga) no lo
// casaban ni el ACCEPT de ESTABLISHED ni el DROP de NEW y caía en la política
// ACCEPT del FORWARD: en none, un canal de salida; falsificando el origen hacia
// el NSIP de otra microVM, uno entre microVMs.
func TestEgressNoDejaPasarLoInvalidoNiLoNoRastreado(t *testing.T) {
	n := Plan(1, "abcdef0123")
	otra := hostPrefix + ".0.10" // el NSIP de Plan(2, ...) en Linux; en macOS NSIP es GuestIP
	sale := func(dst, estado string) paquete {
		return paquete{in: TapName, out: n.NSIf, src: GuestIP, dst: dst, proto: "tcp", dport: 443, estado: estado}
	}
	casos := []struct {
		e      Egress
		p      paquete
		quiere string
	}{
		{EgressNone, sale("8.8.8.8", "INVALID"), "DROP"},
		{EgressNone, sale(otra, "INVALID"), "DROP"},
		{EgressNone, sale("8.8.8.8", "UNTRACKED"), "DROP"},
		{EgressNone, sale("8.8.8.8", "NEW"), "DROP"},
		{EgressNone, sale(n.HostIP, "ESTABLISHED"), "ACCEPT"},
		{EgressInternet, sale("8.8.8.8", "INVALID"), "DROP"},
		{EgressInternet, sale(otra, "INVALID"), "DROP"},
		{EgressInternet, sale(otra, "NEW"), "DROP"},
		{EgressInternet, sale("8.8.8.8", "NEW"), "ACCEPT"},
		{EgressInternet, sale(n.HostIP, "ESTABLISHED"), "ACCEPT"},
	}
	for _, c := range casos {
		if got := veredicto(t, n.egressRules(c.e), c.p); got != c.quiere {
			t.Errorf("%s: %+v = %s, quería %s", c.e, c.p, got, c.quiere)
		}
	}
}

// reglasAplicadas son las reglas que applyEgress manda al netns, sin el
// "ip netns exec <ns>" de delante, con ejecutar sustituido por un fake. En
// allowlist hace falta un ipset en el PATH (solo se busca, no se ejecuta); el
// resolver del final falla al no poder atarse a n.HostIP, y da igual: las
// reglas ya están puestas.
func reglasAplicadas(t *testing.T, n *Net, e Egress) [][]string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ipset"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var reglas [][]string
	viejo := ejecutar
	t.Cleanup(func() { ejecutar = viejo })
	ejecutar = func(args ...string) error {
		if len(args) < 4 || strings.Join(args[:4], " ") != "ip netns exec "+n.NS {
			t.Fatalf("applyEgress fuera del netns: %v", args)
		}
		reglas = append(reglas, append([]string(nil), args[4:]...))
		return nil
	}
	if err := n.applyEgress(e, nil); err == nil && e == EgressAllowlist {
		stopResolver(n.NS)
	}
	return reglas
}

// Las dos vías de Setup (iptables-restore de un golpe o applyEgress una a una)
// tienen que dejar las mismas reglas en none e internet.
func TestApplyEgressYRestorePonenLoMismo(t *testing.T) {
	n := Plan(1, "abcdef0123")
	for _, e := range []Egress{EgressNone, EgressInternet} {
		var quiere, got []string
		for _, r := range n.egressRules(e) {
			quiere = append(quiere, "iptables "+strings.Join(r, " "))
		}
		for _, r := range reglasAplicadas(t, n, e) {
			got = append(got, strings.Join(r, " "))
		}
		if strings.Join(got, "\n") != strings.Join(quiere, "\n") {
			t.Errorf("%s: applyEgress =\n%s\nrestore =\n%s", e, strings.Join(got, "\n"), strings.Join(quiere, "\n"))
		}
	}
	// Y en la entrada de iptables-restore, el filter de none abre con el DROP
	// de INVALID y cierra con el DROP sin ctstate.
	in := restoreInput(n.egressRules(EgressNone))
	if !strings.HasPrefix(in, "*filter\n-A FORWARD -i tap0 -m conntrack --ctstate INVALID -j DROP\n") ||
		!strings.HasSuffix(in, "\n-A FORWARD -i tap0 -j DROP\nCOMMIT\n") {
		t.Errorf("entrada de iptables-restore de none:\n%s", in)
	}
}

// El allowlist: el DNS y los proxies de n.HostIP y lo que esté en el ipset
// pasan si son NEW; lo INVALID o lo no rastreado no, vaya adonde vaya.
func TestAllowlistNoDejaPasarLoInvalido(t *testing.T) {
	n := Plan(1, "abcdef0123")
	reglas := reglasAplicadas(t, n, EgressAllowlist)
	sale := func(dst, proto string, dport int, estado string, enSet bool) paquete {
		return paquete{in: TapName, out: n.NSIf, src: GuestIP, dst: dst, proto: proto, dport: dport, estado: estado, enSet: enSet}
	}
	casos := []struct {
		p      paquete
		quiere string
	}{
		{sale("8.8.8.8", "tcp", 443, "INVALID", true), "DROP"},
		{sale("8.8.8.8", "tcp", 443, "INVALID", false), "DROP"},
		{sale("8.8.8.8", "tcp", 443, "UNTRACKED", false), "DROP"},
		{sale(n.HostIP, "udp", dnsPort, "INVALID", false), "DROP"},
		{sale(n.HostIP, "udp", dnsPort, "NEW", false), "ACCEPT"},
		{sale(n.HostIP, "tcp", credPort, "NEW", false), "ACCEPT"},
		{sale("8.8.8.8", "tcp", 443, "NEW", true), "ACCEPT"},
		{sale("8.8.8.8", "tcp", 443, "NEW", false), "DROP"},
		{sale("8.8.8.8", "tcp", 443, "ESTABLISHED", false), "ACCEPT"},
	}
	for _, c := range casos {
		if got := veredicto(t, reglas, c.p); got != c.quiere {
			t.Errorf("allowlist: %+v = %s, quería %s", c.p, got, c.quiere)
		}
	}
}

// La segunda barrera, en el host: nada de un veth de kindling a otro, nada con
// origen falsificado o INVALID, y la red privada 172.16/12 cerrada salvo el
// propio HostSubnet.
func TestHostEgressRulesCierranVethAVethYOrigenFalso(t *testing.T) {
	reglas := HostEgressRules("")
	nsif := "172.30.0.6" // el veth de un netns, tras su MASQUERADE
	casos := []struct {
		p      paquete
		quiere string
	}{
		{paquete{in: "vh-a", out: "vh-b", src: nsif, dst: "172.30.0.10", proto: "tcp", estado: "NEW"}, "DROP"},
		{paquete{in: "vh-a", out: "vh-b", src: nsif, dst: "172.30.0.10", proto: "tcp", estado: "ESTABLISHED"}, "DROP"},
		{paquete{in: "vh-a", out: "eth0", src: GuestIP, dst: "8.8.8.8", proto: "tcp", estado: "INVALID"}, "DROP"},
		{paquete{in: "vh-a", out: "eth0", src: "203.0.113.9", dst: "8.8.8.8", proto: "tcp", estado: "NEW"}, "DROP"},
		{paquete{in: "vh-a", out: "eth0", src: nsif, dst: "8.8.8.8", proto: "tcp", estado: "INVALID"}, "DROP"},
		{paquete{in: "vh-a", out: "eth0", src: nsif, dst: "8.8.8.8", proto: "tcp", estado: "NEW"}, "ACCEPT"},
		// Lo que entra por otro sitio hacia una microVM no lo toca.
		{paquete{in: "eth0", out: "vh-a", src: "8.8.8.8", dst: nsif, proto: "tcp", estado: "ESTABLISHED"}, "ACCEPT"},
	}
	for _, c := range casos {
		if got := veredicto(t, reglas, c.p); got != c.quiere {
			t.Errorf("host: %+v = %s, quería %s", c.p, got, c.quiere)
		}
	}

	// 172.16.0.0/12 entera cerrada menos HostSubnet: una LAN en 172.20 o 172.31
	// también tiene la segunda barrera.
	_, propia, _ := stdnet.ParseCIDR(HostSubnet)
	for b := 16; b <= 31; b++ {
		dst := "172." + strconv.Itoa(b) + ".1.1"
		quiere := "DROP"
		if propia.Contains(stdnet.ParseIP(dst)) {
			quiere = "ACCEPT"
		}
		p := paquete{in: "vh-a", out: "eth0", src: nsif, dst: dst, proto: "tcp", estado: "NEW"}
		if got := veredicto(t, reglas, p); got != quiere {
			t.Errorf("host hacia %s = %s, quería %s", dst, got, quiere)
		}
	}
}
