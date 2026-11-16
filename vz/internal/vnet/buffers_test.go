package vnet

import (
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
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
