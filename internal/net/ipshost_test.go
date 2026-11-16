package net

import (
	"errors"
	stdnet "net"
	"os"
	"slices"
	"strings"
	"sync/atomic"
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

// Un reload del cortafuegos que vacía INPUT, OUTPUT o FORWARD tras SetupHost:
// el siguiente Setup repone lo que falta, en orden. Con las reglas en su
// sitio, cinco -C (un centinela por juego) y nada más.
func TestAsegurarHostInputReponeTrasUnReload(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	var comprobaciones atomic.Int32
	comprobarEstado = func(args []string) (bool, error) { comprobaciones.Add(1); return f.estado(args) }

	acceso := AccesoAgente{UIDs: []int{1000}, GIDs: []int{1000}}
	if err := SetupHost(acceso); err != nil {
		t.Fatal(err)
	}
	accesoAntes := slices.Clone(f.otras[cadenaAcceso])
	forwardAntes := slices.Clone(f.otras["FORWARD"])
	var quiere []string
	for _, r := range HostInputRules() {
		quiere = append(quiere, strings.Join(r[4:], " "))
	}

	comprobaciones.Store(0)
	if err := asegurarHostInput(); err != nil || comprobaciones.Load() != 5 {
		t.Fatalf("con las reglas puestas: err %v, %d -C, quería 5", err, comprobaciones.Load())
	}

	f.input = []string{"-p tcp --dport 22 -j ACCEPT"} // lo que deja el reload
	if err := asegurarHostInput(); err != nil {
		t.Fatal(err)
	}
	quiere = append(quiere, "-p tcp --dport 22 -j ACCEPT")
	if !slices.Equal(f.input, quiere) {
		t.Fatalf("INPUT tras reponer =\n%s\nquería\n%s", strings.Join(f.input, "\n"), strings.Join(quiere, "\n"))
	}

	// Un firewalld --reload vacía todo: OUTPUT, FORWARD, mangle y la cadena
	// propia también.
	f.otras = nil
	if err := asegurarHostInput(); err != nil {
		t.Fatal(err)
	}
	_, _, salto := reglasAcceso(acceso, os.Geteuid(), os.Getegid())
	if !f.comprobar(sinPosicion(salto, "-C")) || !slices.Equal(f.otras[cadenaAcceso], accesoAntes) {
		t.Fatalf("OUTPUT tras reponer = %v, %s = %v; quería el salto y %v",
			f.otras["OUTPUT"], cadenaAcceso, f.otras[cadenaAcceso], accesoAntes)
	}
	if !slices.Equal(f.otras["FORWARD"], forwardAntes) || !f.comprobar(paraComprobar(reglaAgenteDesdeFuera())) {
		t.Fatalf("FORWARD tras reponer = %v (quería %v), mangle = %v", f.otras["FORWARD"], forwardAntes, f.otras["mangle/PREROUTING"])
	}

	// Solo los REJECT de la cadena borrados (alguien la vació a mano): sin
	// ellos, el salto sigue y la cadena no cierra nada.
	f.otras[cadenaAcceso] = nil
	if err := asegurarHostInput(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.otras[cadenaAcceso], accesoAntes) {
		t.Fatalf("%s tras reponer = %v, quería %v", cadenaAcceso, f.otras[cadenaAcceso], accesoAntes)
	}
}

// S13: un -C que falla por otra cosa que «no está» (el cerrojo de xtables, por
// ejemplo) no se lee como que falta: Setup falla y no se reinstala nada.
func TestAsegurarHostInputNoConfundeUnFalloConUnaFalta(t *testing.T) {
	f := &iptablesFalso{}
	usarIptablesFalso(t, f)
	if err := SetupHost(AccesoAgente{}); err != nil {
		t.Fatal(err)
	}
	antes := slices.Clone(f.input)
	comprobarEstado = func([]string) (bool, error) { return false, errors.New("xtables lock") }
	var ejecutados [][]string
	ejecutar = func(args ...string) error { ejecutados = append(ejecutados, args); return nil }
	if err := asegurarHostInput(); err == nil {
		t.Fatal("asegurarHostInput = nil con los -C fallando")
	}
	if len(ejecutados) != 0 || !slices.Equal(f.input, antes) {
		t.Fatalf("reinstaló con los -C fallando: %v", ejecutados)
	}
}
