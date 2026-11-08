package net

import (
	"errors"
	"fmt"
	stdnet "net"
	"os/exec"
	"slices"
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
	// entre host y microVMs. Descartarlo entero aqui cortaria el trafico
	// legitimo, y por eso va partido sin HostSubnet (ver
	// TestHostEgressRulesCierranVethAVethYOrigenFalso).
	if strings.Contains(junto, "-d 172.16.0.0/12") || strings.Contains(junto, "-d "+HostSubnet+" -j DROP") {
		t.Errorf("el host descarta HostSubnet y eso corta host<->microVM:\n%s", junto)
	}

	if !strings.Contains(junto, "MASQUERADE") {
		t.Errorf("falta el MASQUERADE: sin el, ninguna microVM sale a internet:\n%s", junto)
	}
	// Todas las reglas salen del rango de kindling o entran por sus veth, no
	// de cualquier origen.
	for _, r := range reglas {
		if !strings.Contains(linea(r), "-s "+HostSubnet) && !strings.Contains(linea(r), "-i vh-+") {
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

// SetGraph no solo pone reglas de enlace: quita las de aristas que ya no están
// o que cambiaron de slot. Si no, el DNAT viejo P -> slot llevaría el puerto P
// a la arista nueva que ocupe ese slot. Lo demás del netns no se toca.
func TestReglasEnlaceSobrantes(t *testing.T) {
	n := Plan(5, "0123456789abcdef")
	h := n.HostIP
	dnat := func(dport, slot int) string {
		return fmt.Sprintf("-A PREROUTING -d %s/32 -i %s -p tcp -m tcp --dport %d -j DNAT --to-destination %s:%d",
			h, TapName, dport, h, linkBasePortRango+slot)
	}
	accept := func(slot int) string {
		return fmt.Sprintf("-A FORWARD -d %s/32 -i %s -o %s -p tcp -m tcp --dport %d -m conntrack ! --ctstate INVALID -j ACCEPT",
			h, TapName, n.NSIf, linkBasePortRango+slot)
	}
	// Antes: api:8080 (slot 0), db:5432 (slot 1), web:9000 (slot 2).
	nat := []string{
		"-P PREROUTING ACCEPT",
		dnat(8080, 0), dnat(5432, 1), dnat(9000, 2),
		fmt.Sprintf("-A PREROUTING -i %s -p udp -m udp --dport 53 -j DNAT --to-destination %s:%d", TapName, h, dnsPort),
		fmt.Sprintf("-A PREROUTING -d %s/32 -i %s -p tcp -m tcp --dport 5432 -j DNAT --to-destination %s:%d", h, TapName, h, pgPort),
		fmt.Sprintf("-A PREROUTING -i %s -p tcp -m tcp --dport 8080 -j DNAT --to-destination 10.9.9.9:%d", TapName, linkBasePortRango),
		"",
	}
	forward := []string{
		"-P FORWARD DROP",
		accept(0), accept(1), accept(2),
		fmt.Sprintf("-A FORWARD -d %s/32 -i %s -o %s -p udp -m udp --dport %d -j ACCEPT", h, TapName, n.NSIf, dnsPort),
		fmt.Sprintf("-A FORWARD -d %s/32 -p tcp -m tcp --dport %d:%d -j ACCEPT", h, linkBasePortRango, linkBasePortRango+linkMaxPortsRango-1),
	}
	// Después: db:5432 (slot 0) y zz:7000 (slot 1).
	got := map[string]bool{}
	for _, r := range n.reglasEnlaceSobrantes(nat, forward, []int{5432, 7000}) {
		got[strings.Join(r, " ")] = true
	}
	want := map[string]bool{
		"-t nat " + strings.Replace(dnat(8080, 0), "-A", "-D", 1): true,
		"-t nat " + strings.Replace(dnat(5432, 1), "-A", "-D", 1): true,
		"-t nat " + strings.Replace(dnat(9000, 2), "-A", "-D", 1): true,
		strings.Replace(accept(2), "-A", "-D", 1):                 true,
	}
	if len(got) != len(want) {
		t.Errorf("sobran %d reglas, quería %d: %v", len(got), len(want), got)
	}
	for r := range want {
		if !got[r] {
			t.Errorf("no se borra: %s", r)
		}
	}

	// Idempotente: con las reglas ya al día no sobra nada.
	nat = []string{dnat(5432, 0), dnat(7000, 1)}
	forward = []string{accept(0), accept(1)}
	if r := n.reglasEnlaceSobrantes(nat, forward, []int{5432, 7000}); len(r) != 0 {
		t.Errorf("con las reglas al día sobran %v", r)
	}
	// Sin aristas se va todo lo de enlace.
	if r := n.reglasEnlaceSobrantes(nat, forward, nil); len(r) != 4 {
		t.Errorf("sin aristas sobran %d reglas, quería 4: %v", len(r), r)
	}
}

// iptablesFalso simula la cadena INPUT para setupHostInput: -C, -D e -I 1.
type iptablesFalso struct{ input []string }

func (f *iptablesFalso) clave(args []string) string {
	return strings.Join(args[3:], " ") // tras "iptables -X INPUT"
}

func (f *iptablesFalso) comprobar(args []string) bool {
	return slices.Contains(f.input, f.clave(args))
}

func (f *iptablesFalso) ejecutar(args ...string) error {
	switch args[1] {
	case "-D":
		if i := slices.Index(f.input, f.clave(args)); i >= 0 {
			f.input = slices.Delete(f.input, i, i+1)
			return nil
		}
		return errors.New("no existe")
	case "-I":
		f.input = slices.Insert(f.input, 0, strings.Join(args[4:], " "))
		return nil
	}
	return fmt.Errorf("inesperado: %v", args)
}

// La HostIP de una máquina solo se alcanza por su propio veth: el ACCEPT de
// cada servicio del host (DNS, credenciales, Postgres, MySQL, aristas) exige
// que el destino sea del interfaz de entrada. Medido en el lab: con la regla
// vieja, desde el netns A `nc -z <HostIP de B> 5380` conectaba.
func TestHostInputSoloAlcanzaSuPropioVeth(t *testing.T) {
	for _, r := range HostInputRules() {
		l := strings.Join(r, " ")
		if strings.Contains(l, "--dport") && !strings.Contains(l, "-m addrtype --dst-type LOCAL --limit-iface-in") {
			t.Errorf("servicio abierto a la HostIP de cualquier máquina: %s", l)
		}
	}
}

// Al arrancar sobre un host con las reglas de una versión anterior, sus
// ACCEPT sin addrtype se quitan: si se quedaran delante del DROP, seguirían
// dejando pasar a todos.
func TestSetupHostInputQuitaLasReglasViejas(t *testing.T) {
	f := &iptablesFalso{}
	viejas := append(append([][]string{HostInputRules()[0]}, reglasInputViejas()...), HostInputRules()[len(HostInputRules())-1])
	for i := len(viejas) - 1; i >= 0; i-- {
		_ = f.ejecutar(viejas[i]...)
	}
	f.input = append(f.input, "-p tcp --dport 22 -j ACCEPT") // algo del administrador
	oldC, oldE := comprobarRegla, ejecutar
	t.Cleanup(func() { comprobarRegla, ejecutar = oldC, oldE })
	comprobarRegla, ejecutar = f.comprobar, f.ejecutar

	if err := setupHostInput(); err != nil {
		t.Fatal(err)
	}
	var quiere []string
	for _, r := range HostInputRules() {
		quiere = append(quiere, strings.Join(r[4:], " "))
	}
	quiere = append(quiere, "-p tcp --dport 22 -j ACCEPT")
	if !slices.Equal(f.input, quiere) {
		t.Fatalf("INPUT =\n%s\nquería\n%s", strings.Join(f.input, "\n"), strings.Join(quiere, "\n"))
	}
}

// Un daemon de la versión anterior que arranca después del nuestro borra el
// DROP y la regla de respuestas (son las mismas en las dos versiones) y mete
// su juego completo arriba: nuestros ACCEPT quedan detrás de su DROP. Al
// volver a arrancar el nuestro, quitar las viejas no basta: hay que volver a
// ordenar (visto en el lab: el DNS del invitado se quedó sin respuesta).
func TestSetupHostInputReordenaTrasOtraVersion(t *testing.T) {
	f := &iptablesFalso{}
	oldC, oldE := comprobarRegla, ejecutar
	t.Cleanup(func() { comprobarRegla, ejecutar = oldC, oldE })
	comprobarRegla, ejecutar = f.comprobar, f.ejecutar
	if err := setupHostInput(); err != nil {
		t.Fatal(err)
	}
	// Lo que hace el setupHostInput de la versión anterior.
	nuevas := HostInputRules()
	viejas := append(append([][]string{nuevas[0]}, reglasInputViejas()...), nuevas[len(nuevas)-1])
	for _, r := range viejas {
		for f.comprobar(sinPosicion(r, "-C")) {
			_ = f.ejecutar(sinPosicion(r, "-D")...)
		}
	}
	for i := len(viejas) - 1; i >= 0; i-- {
		_ = f.ejecutar(viejas[i]...)
	}

	if err := setupHostInput(); err != nil {
		t.Fatal(err)
	}
	var quiere []string
	for _, r := range nuevas {
		quiere = append(quiere, strings.Join(r[4:], " "))
	}
	if !slices.Equal(f.input, quiere) {
		t.Fatalf("INPUT =\n%s\nquería\n%s", strings.Join(f.input, "\n"), strings.Join(quiere, "\n"))
	}
}
