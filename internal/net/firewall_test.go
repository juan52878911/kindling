package net

import (
	"fmt"
	stdnet "net"
	"os/exec"
	"strings"
	"testing"
)

// isBlockedIP es lo que impide que un servidor MCP comprometido pivote desde su
// microVM hacia la red de casa. Se ejecuta sobre lo que devuelve un resolver, que
// en modo allowlist es justo la parte que un invitado hostil puede influir: si
// consigue que un dominio "permitido" resuelva a 192.168.2.64, y esto dijera que
// no pasa nada, esa IP entraria en el ipset y el invitado tendria via libre a la
// LAN.
func TestIsBlockedIPTapaLasRedesQueUnInvitadoNoDebeAlcanzarJamas(t *testing.T) {
	prohibidas := []struct{ ip, porque string }{
		{"192.168.2.64", "el CT de UltraMemory, en la LAN de casa"},
		{"192.168.2.61", "el propio anfitrion de las microVM"},
		{"192.168.1.1", "el router"},
		{"10.0.0.5", "privada clase A"},
		{"10.255.255.255", "el borde alto de 10/8"},
		{"172.16.0.1", "el rango de enlaces de kindling"},
		{"172.30.0.1", "otro trozo de 172.16/12"},
		{"172.31.255.255", "el borde alto de 172.16/12"},
		{"169.254.169.254", "los metadatos del cloud, el objetivo clasico"},
		{"169.254.0.1", "link-local"},
		{"127.0.0.1", "el loopback del anfitrion"},
		{"127.255.255.254", "el borde alto de 127/8"},
		{"100.64.0.1", "CGNAT"},
		{"0.0.0.0", "hacia 0.0.0.0 el host se llama a sí mismo"},
		{"0.1.2.3", "el resto de 0/8"},
		{"224.0.0.251", "multicast (mDNS)"},
		{"239.255.255.250", "multicast (SSDP)"},
		{"240.0.0.1", "reservada"},
		{"255.255.255.255", "broadcast"},
	}
	for _, c := range prohibidas {
		ip := stdnet.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("%s no es una IP", c.ip)
		}
		if !isBlockedIP(ip) {
			t.Errorf("%s NO esta bloqueada y deberia: %s", c.ip, c.porque)
		}
	}

	// El contrapunto, y no es menor: si esto bloqueara de mas, el modo allowlist
	// no dejaria salir a ningun sitio y el servicio quedaria inutil sin decir
	// por que.
	publicas := []struct{ ip, quien string }{
		{"1.1.1.1", "el resolver al que se fuerza el DNS del invitado"},
		{"8.8.8.8", "DNS publico"},
		{"140.82.121.4", "github.com"},
		{"172.15.255.255", "justo por debajo de 172.16/12"},
		{"172.32.0.1", "justo por encima de 172.16/12"},
		{"100.63.255.255", "justo por debajo del CGNAT"},
		{"100.128.0.1", "justo por encima del CGNAT"},
		{"9.255.255.255", "justo por debajo de 10/8"},
		{"11.0.0.1", "justo por encima de 10/8"},
	}
	for _, c := range publicas {
		ip := stdnet.ParseIP(c.ip)
		if ip == nil {
			t.Fatalf("%s no es una IP", c.ip)
		}
		if isBlockedIP(ip) {
			t.Errorf("%s SI esta bloqueada y no deberia (%s): el allowlist no dejaria salir", c.ip, c.quien)
		}
	}
}

// Una politica desconocida tiene que FALLAR, no caer en la mas permisiva. Un
// typo en `-egress internte` no puede acabar dando internet.
func TestParseEgressFallaEnVezDeAbrir(t *testing.T) {
	for _, c := range []struct {
		entrada string
		quiero  Egress
	}{
		{"none", EgressNone},
		{"internet", EgressInternet},
		{"allowlist", EgressAllowlist},
		{"", EgressNone}, // sin decir nada, sin salida
	} {
		got, err := ParseEgress(c.entrada)
		if err != nil {
			t.Errorf("ParseEgress(%q) fallo: %v", c.entrada, err)
			continue
		}
		if got != c.quiero {
			t.Errorf("ParseEgress(%q) = %q, esperaba %q", c.entrada, got, c.quiero)
		}
	}

	for _, malo := range []string{"internte", "INTERNET", "all", "yes", "true", "*", "none,internet"} {
		got, err := ParseEgress(malo)
		if err == nil {
			t.Errorf("ParseEgress(%q) no fallo; devolvio %q", malo, got)
		}
		// Y lo que importa de verdad: si un llamador se comiera el error, lo que
		// se queda no puede ser una politica con salida.
		if got == EgressInternet || got == EgressAllowlist {
			t.Errorf("ParseEgress(%q) fallo pero devolvio %q: ignorar el error daria salida", malo, got)
		}
	}
}

// El host es la SEGUNDA barrera: aunque alguien manipulara las reglas de un
// namespace, aqui se sigue negando el salto a la red privada.
func TestHostEgressRulesNieganLaRedPrivadaMenosElRangoPropio(t *testing.T) {
	reglas := HostEgressRules("kbr0")

	linea := func(r []string) string { return strings.Join(r, " ") }
	var todo []string
	for _, r := range reglas {
		todo = append(todo, linea(r))
	}
	junto := strings.Join(todo, "\n")

	for _, cidr := range []string{"10.0.0.0/8", "192.168.0.0/16", "169.254.0.0/16", "127.0.0.0/8", "100.64.0.0/10"} {
		if !strings.Contains(junto, "-d "+cidr+" -j DROP") {
			t.Errorf("el host no descarta el trafico hacia %s:\n%s", cidr, junto)
		}
	}

	// La excepcion deliberada: 172.16/12 contiene el propio rango de enlaces
	// entre host y microVMs. Descartarlo aqui cortaria el trafico legitimo, y por
	// eso ese rango lo cubren SOLO las reglas del namespace.
	if strings.Contains(junto, "-d 172.16.0.0/12") {
		t.Errorf("el host descarta 172.16/12 y eso corta host<->microVM:\n%s", junto)
	}

	if !strings.Contains(junto, "MASQUERADE") {
		t.Errorf("falta el MASQUERADE: sin el, ninguna microVM sale a internet:\n%s", junto)
	}
	// Todas las reglas salen del rango de kindling, no de cualquier origen.
	for _, r := range reglas {
		if !strings.Contains(linea(r), "-s "+HostSubnet) {
			t.Errorf("regla sin acotar al rango de kindling: %s", linea(r))
		}
	}
}

// Las reglas de INPUT: solo para lo que entra por los veth de kindling y desde
// su rango, con los ACCEPT (respuestas, DNS del allowlist y proxy de
// credenciales) delante del DROP.
// Sin ellas, un invitado con salida llegaba a los servicios del host por su IP
// pública.
func TestHostInputRulesAceptanLoLegitimoYDescartanElResto(t *testing.T) {
	reglas := HostInputRules()
	if len(reglas) == 0 {
		t.Fatal("sin reglas de INPUT")
	}
	for _, r := range reglas {
		l := strings.Join(r, " ")
		if !strings.Contains(l, "-I INPUT 1 -i vh-+ -s "+HostSubnet) {
			t.Errorf("regla sin acotar a los veth y al rango de kindling: %s", l)
		}
	}
	ultima := strings.Join(reglas[len(reglas)-1], " ")
	if !strings.HasSuffix(ultima, "-j DROP") {
		t.Errorf("la última regla tiene que ser el DROP: %s", ultima)
	}
	junto := ""
	for _, r := range reglas[:len(reglas)-1] {
		junto += strings.Join(r, " ") + "\n"
	}
	for _, want := range []string{"ESTABLISHED,RELATED -j ACCEPT", "-p udp --dport 5333 -j ACCEPT", "-p tcp --dport 5333 -j ACCEPT",
		"-p tcp --dport 5380 -j ACCEPT", "-p tcp --dport 5381 -j ACCEPT", "-p tcp --dport 5400:5463 -j ACCEPT"} {
		if !strings.Contains(junto, want) {
			t.Errorf("falta %q antes del DROP:\n%s", want, junto)
		}
	}
}

func TestSinPosicionQuitaElNumeroDeLinea(t *testing.T) {
	r := []string{"iptables", "-I", "INPUT", "1", "-i", "vh-+", "-j", "DROP"}
	if got := strings.Join(sinPosicion(r, "-C"), " "); got != "iptables -C INPUT -i vh-+ -j DROP" {
		t.Errorf("sinPosicion(-C) = %q", got)
	}
	if got := strings.Join(sinPosicion(r, "-D"), " "); got != "iptables -D INPUT -i vh-+ -j DROP" {
		t.Errorf("sinPosicion(-D) = %q", got)
	}
}

// applyIPv6Barrier es la defensa en profundidad de B1: el filtrado de este
// fichero (ipset, iptables) es solo IPv4, así que sin ella un invitado con
// salida v6 real se saltaría el allowlist entero, y en modo internet o none
// tendría una salida que ningún DROP de aquí cubre. Se comprueba con un `ns`
// de mentira: no hay netns real en un test.
func TestApplyIPv6BarrierApagaSysctlEnLasInterfacesQueImportan(t *testing.T) {
	n := Plan(1, "abcdef0123")
	var llamadas [][]string
	ns := func(args ...string) error {
		llamadas = append(llamadas, append([]string(nil), args...))
		return nil
	}
	if err := n.applyIPv6Barrier(ns); err != nil {
		t.Fatalf("applyIPv6Barrier = %v", err)
	}

	junto := ""
	for _, l := range llamadas {
		junto += strings.Join(l, " ") + "\n"
	}
	// Las cuatro claves de sysctl que cierran IPv6 dentro del namespace, sin
	// depender de que exista ip6tables.
	for _, quiere := range []string{
		"sysctl -w net.ipv6.conf.all.disable_ipv6=1",
		"sysctl -w net.ipv6.conf.default.disable_ipv6=1",
		"sysctl -w net.ipv6.conf." + TapName + ".disable_ipv6=1",
		"sysctl -w net.ipv6.conf." + n.NSIf + ".disable_ipv6=1",
	} {
		if !strings.Contains(junto, quiere) {
			t.Errorf("falta %q:\n%s", quiere, junto)
		}
	}
}

// Sin ip6tables instalado (el caso de este entorno de test, y de muchos hosts
// reales que no lo llevan por defecto), la función no debe fallar: la capa de
// sysctl ya es la barrera real, y exigir ip6tables tumbaría el arranque de
// toda microVM en un host que no lo tiene.
func TestApplyIPv6BarrierNoFallaSinIp6tables(t *testing.T) {
	if _, err := exec.LookPath("ip6tables"); err == nil {
		t.Skip("este host SÍ tiene ip6tables; el caso que prueba este test no se da aquí")
	}
	n := Plan(2, "fedcba9876")
	if err := n.applyIPv6Barrier(func(args ...string) error { return nil }); err != nil {
		t.Fatalf("sin ip6tables, applyIPv6Barrier tiene que avisar y seguir, no fallar: %v", err)
	}
}

// Un fallo de sysctl (namespace sin /proc/sys/net/ipv6, por ejemplo) tampoco
// es fatal: la capa 2 (ip6tables) sigue en pie, y negarse a montar la red por
// esto dejaría a la microVM sin arrancar por algo que no es su culpa.
func TestApplyIPv6BarrierSiguePeseAUnSysctlQueFalla(t *testing.T) {
	n := Plan(3, "0011223344")
	ns := func(args ...string) error {
		if len(args) > 0 && args[0] == "sysctl" {
			return fmt.Errorf("sysctl: no such file or directory")
		}
		return nil
	}
	if err := n.applyIPv6Barrier(ns); err != nil {
		t.Fatalf("un sysctl que falla no debe tumbar applyIPv6Barrier: %v", err)
	}
}
