package vnet

import (
	"context"
	"io"
	"net"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/juan52878911/kindling/vz/internal/egress"
)

// R7: los buffers TCP de la pila viven en este proceso, fuera de la memoria
// de la VM. Con pocas conexiones cada una tiene los máximos de siempre; con
// MaxTCPFlows vivas, entre todas no pueden pasar de tcpBufPresupuesto (antes
// eran MaxTCPFlows × (tcpRXBufMax + tcpTXBufMax), 7 GiB).
func TestBuffersTCPConPresupuesto(t *testing.T) {
	n, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	maximos := func() (int, int) {
		var rx tcpip.TCPReceiveBufferSizeRangeOption
		var tx tcpip.TCPSendBufferSizeRangeOption
		if e := n.stack.TransportProtocolOption(tcp.ProtocolNumber, &rx); e != nil {
			t.Fatal(e)
		}
		if e := n.stack.TransportProtocolOption(tcp.ProtocolNumber, &tx); e != nil {
			t.Fatal(e)
		}
		return rx.Max, tx.Max
	}
	if rx, tx := maximos(); rx != tcpRXBufMax || tx != tcpTXBufMax {
		t.Fatalf("with no connections the maximums are %d/%d, want %d/%d", rx, tx, tcpRXBufMax, tcpTXBufMax)
	}
	for i := 0; i < MaxTCPFlows; i++ {
		if !n.tomarTCP() {
			t.Fatalf("connection %d found no slot", i+1)
		}
	}
	rx, tx := maximos()
	total := int64(MaxTCPFlows) * int64(rx+tx)
	t.Logf("with %d connections: %d KiB + %d KiB each, %d MiB in all (was %d MiB)",
		MaxTCPFlows, rx>>10, tx>>10, total>>20, int64(MaxTCPFlows)*(tcpRXBufMax+tcpTXBufMax)>>20)
	if total > tcpBufPresupuesto {
		t.Fatalf("with %d connections the buffers can add up to %d MiB, budget %d MiB", MaxTCPFlows, total>>20, tcpBufPresupuesto>>20)
	}
	for i := 0; i < MaxTCPFlows; i++ {
		n.soltarTCP()
	}
	if rx, tx := maximos(); rx != tcpRXBufMax || tx != tcpTXBufMax {
		t.Fatalf("after closing them the maximums are %d/%d, want %d/%d back", rx, tx, tcpRXBufMax, tcpTXBufMax)
	}
}

// R3: con MaxTCPFlows conexiones DE VERDAD, lo que suman los buffers de sus
// endpoints no pasa de tcpBufPresupuesto. Leer solo las opciones de la pila no
// bastaba: el forwarder daba a cada conexión 1 MiB de recepción sin mirarlas,
// y la autoescala no encoge lo ya crecido (unos 1,1 GiB en el peor caso).
// La primera conexión se abre sola y con datos, para que crezca antes de que
// lleguen las demás.
func TestBuffersTCPDeLasConexionesVivas(t *testing.T) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err == nil && lim.Cur < 4096 {
		lim.Cur = min(4096, lim.Max)
		_ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
	}
	up, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	r := newRig(t, up.Addr().String())
	r.policy.Set(egress.Internet, nil)

	abrir := func() net.Conn {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := r.g.dialTCP(ctx, "93.184.215.14:80")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	primera := abrir()
	const enviar = 2 << 20
	_ = primera.SetDeadline(time.Now().Add(20 * time.Second))
	go func() { _, _ = primera.Write(make([]byte, enviar)) }()
	if _, err := io.ReadFull(primera, make([]byte, enviar)); err != nil {
		t.Fatal(err)
	}
	_ = primera.SetDeadline(time.Time{})
	for i := 1; i < MaxTCPFlows; i++ {
		abrir()
	}
	limite := time.Now().Add(10 * time.Second)
	for len(r.n.tcpFlows) != MaxTCPFlows {
		if time.Now().After(limite) {
			t.Fatalf("%d connections up, want %d", len(r.n.tcpFlows), MaxTCPFlows)
		}
		time.Sleep(10 * time.Millisecond)
	}

	var total int64
	conexiones := 0
	for _, ep := range r.n.stack.RegisteredEndpoints() {
		te, ok := ep.(*tcp.Endpoint)
		// Fuera: los listeners propios de la pasarela (MMDS) y el DNS.
		if !ok || te.EndpointState() == tcp.StateListen || te.TransportEndpointInfo.ID.LocalPort == 53 {
			continue
		}
		so := te.SocketOptions()
		total += so.GetReceiveBufferSize() + so.GetSendBufferSize()
		conexiones++
	}
	t.Logf("%d connections: %d MiB of TCP buffers in all (budget %d MiB)", conexiones, total>>20, tcpBufPresupuesto>>20)
	if total > tcpBufPresupuesto {
		t.Fatalf("the TCP buffers of %d live connections add up to %d MiB, budget %d MiB", conexiones, total>>20, tcpBufPresupuesto>>20)
	}
	if conexiones < MaxTCPFlows {
		t.Fatalf("counted %d endpoints, want %d", conexiones, MaxTCPFlows)
	}
}
