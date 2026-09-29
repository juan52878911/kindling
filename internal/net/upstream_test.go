package net

import (
	"net/netip"
	"testing"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// Un upstream de Postgres fijado por el operador puede ser privado o el
// loopback, pero nunca la red interna de kindling: pkg/credproxy lleva su
// propia lista (vz/ no puede importar este paquete) y aquí se comprueba que
// cubre el enlace del invitado y todos los veth del host, que en Linux están
// en el netns del host, el mismo desde el que marca el proxy.
func TestUpstreamProhibidoCubreLaRedDeKindling(t *testing.T) {
	for _, ip := range []string{GuestIP, GuestGW, "172.16.0.0", "172.16.0.3"} {
		if !credproxy.UpstreamIPProhibida(netip.MustParseAddr(ip)) {
			t.Errorf("%s (enlace del invitado) no está prohibido", ip)
		}
	}
	host := netip.MustParsePrefix(HostSubnet)
	for _, idx := range []int{0, 1, 63, 64, 16383} {
		n := Plan(idx, "abcdef0123")
		ip := netip.MustParseAddr(n.HostIP)
		if !host.Contains(ip) {
			t.Fatalf("HostIP %s fuera de %s", ip, host)
		}
		if !credproxy.UpstreamIPProhibida(ip) {
			t.Errorf("HostIP %s (veth del host) no está prohibido", ip)
		}
	}
	if !credproxy.UpstreamIPProhibida(host.Masked().Addr()) ||
		!credproxy.UpstreamIPProhibida(netip.MustParseAddr("172.30.255.255")) {
		t.Errorf("%s no está prohibido entero", HostSubnet)
	}
	// Lo que sí debe poder fijarse: el loopback (Docker) y la LAN, incluida la
	// red por defecto de Docker (172.17/16), que no es de kindling.
	for _, ip := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.10", "172.17.0.2", "100.64.0.1"} {
		if credproxy.UpstreamIPProhibida(netip.MustParseAddr(ip)) {
			t.Errorf("%s está prohibido y no debería", ip)
		}
	}
}
