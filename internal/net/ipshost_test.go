package net

import (
	stdnet "net"
	"slices"
	"strings"
	"testing"
)

// Las reglas no dependen de las direcciones de la máquina que corre los
// tests; quien quiera unas concretas las pone con conIPsHost.
func init() { ipsDelHost = func() []stdnet.IP { return nil } }

func conIPsHost(t *testing.T, ips ...string) {
	t.Helper()
	viejo := ipsDelHost
	t.Cleanup(func() { ipsDelHost = viejo })
	ipsDelHost = func() []stdnet.IP {
		var out []stdnet.IP
		for _, ip := range ips {
			out = append(out, stdnet.ParseIP(ip))
		}
		return out
	}
}

// Un invitado no alcanza ninguna IPv4 del host por el FORWARD de su netns, en
// ningún modo, aunque la IP esté en el ipset (un dominio permitido que
// resuelve a la IP pública del host). La HostIP propia (DNS y proxies) sigue
// abierta, y no hay reglas para el loopback ni para las HostIP de otras.
func TestInvitadoNoAlcanzaLasIPsDelHost(t *testing.T) {
	n := Plan(1, "abcdef0123")
	conIPsHost(t, "203.0.113.7", "127.0.0.1", n.HostIP, "172.30.0.5", "203.0.113.7", "198.51.100.2")
	var drops []string
	for _, r := range n.reglasIPsHost() {
		drops = append(drops, strings.Join(r, " "))
	}
	if want := []string{
		"-A FORWARD -i tap0 -d 198.51.100.2 -j DROP",
		"-A FORWARD -i tap0 -d 203.0.113.7 -j DROP",
	}; !slices.Equal(drops, want) {
		t.Fatalf("DROP a las IP del host =\n%s\nquería\n%s", strings.Join(drops, "\n"), strings.Join(want, "\n"))
	}
	alHost := paquete{in: TapName, out: n.NSIf, src: GuestIP, dst: "203.0.113.7", proto: "tcp", dport: 22, estado: "NEW", enSet: true}
	for nombre, reglas := range map[string][][]string{
		"internet":  n.egressRules(EgressInternet),
		"allowlist": n.reglasAllowlist(),
		"none":      n.egressRules(EgressNone),
	} {
		if got := veredicto(t, reglas, alHost); got != "DROP" {
			t.Errorf("%s: SYN a la IP pública del host = %s, quería DROP", nombre, got)
		}
	}
	dns := paquete{in: TapName, out: n.NSIf, src: GuestIP, dst: n.HostIP, proto: "udp", dport: dnsPort, estado: "NEW"}
	if got := veredicto(t, n.reglasAllowlist(), dns); got != "ACCEPT" {
		t.Errorf("allowlist: el DNS de la HostIP propia = %s, quería ACCEPT", got)
	}
	resp := alHost
	resp.estado = "ESTABLISHED"
	if got := veredicto(t, n.egressRules(EgressInternet), resp); got != "ACCEPT" {
		t.Errorf("las respuestas a lo que abre el host = %s, quería ACCEPT", got)
	}
}

// Un reload del cortafuegos que vacía INPUT tras SetupHost: el siguiente
// Setup las repone en orden. Con las reglas en su sitio, un solo -C y nada más.
func TestAsegurarHostInputReponeTrasUnReload(t *testing.T) {
	f := &iptablesFalso{}
	oldC, oldE := comprobarRegla, ejecutar
	t.Cleanup(func() { comprobarRegla, ejecutar = oldC, oldE; inputMontado.Store(false) })
	comprobaciones := 0
	comprobarRegla = func(args []string) bool { comprobaciones++; return f.comprobar(args) }
	ejecutar = f.ejecutar

	inputMontado.Store(false)
	if err := asegurarHostInput(); err != nil || len(f.input) != 0 || comprobaciones != 0 {
		t.Fatalf("sin SetupHost no debe tocar nada: err %v, INPUT %v, %d -C", err, f.input, comprobaciones)
	}
	if err := setupHostInput(); err != nil {
		t.Fatal(err)
	}
	inputMontado.Store(true)
	var quiere []string
	for _, r := range HostInputRules() {
		quiere = append(quiere, strings.Join(r[4:], " "))
	}

	comprobaciones = 0
	if err := asegurarHostInput(); err != nil || comprobaciones != 1 {
		t.Fatalf("con las reglas puestas: err %v, %d -C, quería 1", err, comprobaciones)
	}

	f.input = []string{"-p tcp --dport 22 -j ACCEPT"} // lo que deja el reload
	if err := asegurarHostInput(); err != nil {
		t.Fatal(err)
	}
	quiere = append(quiere, "-p tcp --dport 22 -j ACCEPT")
	if !slices.Equal(f.input, quiere) {
		t.Fatalf("INPUT tras reponer =\n%s\nquería\n%s", strings.Join(f.input, "\n"), strings.Join(quiere, "\n"))
	}
}
