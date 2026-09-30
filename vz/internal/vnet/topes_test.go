package vnet

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/juan52878911/kindling/vz/internal/egress"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// Un invitado que abre miles de flujos UDP al 53 (una consulta desde cada
// puerto de origen y nada más) no deja una goroutine por flujo en kling-vz,
// también en egress none: por encima de MaxDNSUDPFlows se descartan. Sin el
// tope, 3000 flujos dejaban ~2800 goroutines vivas durante dnsIdle.
func TestFlujosDNSAcotados(t *testing.T) {
	r := newRig(t, "")
	if rc := dnsQuery(t, r.g, "172.16.0.1", "example.com"); rc != 5 {
		t.Fatalf("none: rcode %d, want REFUSED", rc)
	}
	time.Sleep(200 * time.Millisecond)
	antes := runtime.NumGoroutine()
	q := egress.BuildQuery("example.com", 1)
	var conns []*gonet.UDPConn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 3000; i++ {
		c, err := gonet.DialUDP(r.g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(GatewayIP.As4()), Port: 53}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		_, _ = c.Write(q)
		if i%100 == 99 {
			time.Sleep(10 * time.Millisecond) // que la cola del enlace no descarte
		}
	}
	time.Sleep(500 * time.Millisecond)
	despues := runtime.NumGoroutine()
	t.Logf("goroutines: %d antes, %d con 3000 flujos", antes, despues)
	if despues-antes > MaxDNSUDPFlows+50 {
		t.Fatalf("3000 flujos al 53 dejaron %d goroutines más (tope %d)", despues-antes, MaxDNSUDPFlows)
	}
}

// Lo mismo con conexiones TCP al 53 que no mandan nada: por encima de
// MaxDNSTCPConns, RST en vez de una goroutine esperando dnsIdle.
func TestConexionesDNSTCPAcotadas(t *testing.T) {
	r := newRig(t, "")
	time.Sleep(200 * time.Millisecond)
	antes := runtime.NumGoroutine()
	var abiertas, rechazadas int
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 300; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := r.g.dialTCP(ctx, "172.16.0.1:53")
		cancel()
		if err != nil {
			rechazadas++
			continue
		}
		abiertas++
		conns = append(conns, c)
	}
	time.Sleep(300 * time.Millisecond)
	despues := runtime.NumGoroutine()
	t.Logf("TCP al 53: %d abiertas, %d rechazadas; goroutines %d -> %d", abiertas, rechazadas, antes, despues)
	if abiertas > MaxDNSTCPConns {
		t.Fatalf("%d conexiones TCP al 53 abiertas a la vez (tope %d)", abiertas, MaxDNSTCPConns)
	}
	if despues-antes > 3*MaxDNSTCPConns+50 {
		t.Fatalf("300 conexiones al 53 dejaron %d goroutines más", despues-antes)
	}
}
