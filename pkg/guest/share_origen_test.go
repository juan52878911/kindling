package guest

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
)

// hexRuta escribe una dirección como en /proc/net/route: el uint32 cuyos
// bytes en memoria son los de la dirección.
func hexRuta(ip string) string {
	return fmt.Sprintf("%08X", binary.NativeEndian.Uint32(net.ParseIP(ip).To4()))
}

// tablaInvitado es la tabla de rutas de un invitado de kindling con Docker
// dentro: eth0 hacia el host, la ruta por defecto por la pasarela y la red
// puente de los contenedores.
func tablaInvitado() []byte {
	var b strings.Builder
	b.WriteString("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n")
	linea := func(dev, dst, gw, flags, mask string) {
		fmt.Fprintf(&b, "%s\t%s\t%s\t%s\t0\t0\t0\t%s\t0\t0\t0\n", dev, hexRuta(dst), hexRuta(gw), flags, hexRuta(mask))
	}
	linea("eth0", "0.0.0.0", "172.16.0.1", "0003", "0.0.0.0")
	linea("eth0", "172.16.0.0", "0.0.0.0", "0001", "255.255.255.252")
	linea("docker0", "172.17.0.0", "0.0.0.0", "0001", "255.255.0.0")
	return []byte(b.String())
}

// En las pruebas el agente ve siempre la tabla de un invitado, no la de la
// máquina que las corre: httptest.NewRequest llega desde 192.0.2.1, que en
// esa tabla es el host (por la ruta por defecto).
func init() {
	rutasInvitado = func() ([]rutaIPv4, error) { return parseRutas(tablaInvitado()), nil }
}

// conRutas sustituye la tabla de rutas del invitado durante la prueba.
func conRutas(t *testing.T, tabla []byte, err error) {
	t.Helper()
	viejas := rutasInvitado
	rutasInvitado = func() ([]rutaIPv4, error) {
		if err != nil {
			return nil, err
		}
		return parseRutas(tabla), nil
	}
	t.Cleanup(func() { rutasInvitado = viejas })
}

// Quién cuenta como el host: por TCP, solo la pasarela o quien llega por la
// ruta por defecto (el veth del host en Firecracker); por vsock, solo el
// CID 2. Lo demás —un contenedor con red puente, la red de eth0, loopback,
// la propia IP, IPv6— es de dentro.
func TestRequestFromGuest(t *testing.T) {
	conRutas(t, tablaInvitado(), nil)
	pet := func(remota string, local net.Addr) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://x/share/attach", nil)
		r.RemoteAddr = remota
		return r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, local))
	}
	tcp := &net.TCPAddr{IP: net.ParseIP("172.16.0.2"), Port: 8080}
	vs := vsockAddr{cid: 0xFFFFFFFF, port: 8080}
	for _, c := range []struct {
		nombre string
		r      *http.Request
		dentro bool
	}{
		{"tcp de la pasarela (kling-vz, kling-hv)", pet("172.16.0.1:5000", tcp), false},
		{"tcp del veth del host (Firecracker)", pet("172.30.0.1:5000", tcp), false},
		{"tcp de un contenedor con red puente", pet("172.17.0.2:5000", tcp), true},
		{"tcp de la red de eth0 que no es la pasarela", pet("172.16.0.3:5000", tcp), true},
		{"tcp loopback", pet("127.0.0.1:5000", tcp), true},
		{"tcp propia IP", pet("172.16.0.2:5000", tcp), true},
		{"tcp IPv6", pet("[fd00::1]:5000", tcp), true},
		{"vsock del host", pet(vsockAddr{cid: 2}.String(), vs), false},
		{"vsock loopback", pet(vsockAddr{cid: 1, port: 9}.String(), vs), true},
		{"vsock sin peer", pet(vsockAddr{}.String(), vs), true},
	} {
		if got := requestFromGuest(c.r); got != c.dentro {
			t.Errorf("%s: fromGuest = %v, want %v", c.nombre, got, c.dentro)
		}
	}
}

// Sin tabla de rutas (o sin ruta por defecto) nadie cuenta como el host
// por TCP: falla cerrado.
func TestRequestFromGuestSinRutas(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPost, "http://x/share/attach", nil)
	r.RemoteAddr = "172.16.0.1:5000"
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
		net.Addr(&net.TCPAddr{IP: net.ParseIP("172.16.0.2"), Port: 8080})))

	conRutas(t, nil, errors.New("no /proc"))
	if !requestFromGuest(r) {
		t.Error("sin tabla de rutas, la pasarela cuenta como host")
	}
	sinDefecto := []byte("Iface\tDestination\tGateway\tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t" + hexRuta("172.16.0.0") + "\t00000000\t0001\t0\t0\t0\t" + hexRuta("255.255.255.252") + "\n")
	conRutas(t, sinDefecto, nil)
	if !requestFromGuest(r) {
		t.Error("sin ruta por defecto, 172.16.0.1 cuenta como host")
	}
}
