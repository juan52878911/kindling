package net

import (
	stdnet "net"
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
