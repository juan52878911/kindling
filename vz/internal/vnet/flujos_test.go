package vnet

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"github.com/juan52878911/kindling/vz/internal/egress"
)

// Con MaxTCPFlows conexiones vivas, la siguiente recibe un RST sin que el
// host marque nada; al cerrarse una, vuelve a haber sitio.
func TestTopeDeFlujosTCP(t *testing.T) {
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
	eco := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := r.g.dialTCP(ctx, "93.184.215.14:80")
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("x")); err != nil {
			return err
		}
		_, err = io.ReadFull(c, make([]byte, 1))
		return err
	}
	marcadas := func() int {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.dialed)
	}
	// Las vivas se simulan llenando el semáforo: abrir 512 conexiones de
	// verdad en un test es lento y no prueba nada más.
	for i := 0; i < MaxTCPFlows; i++ {
		r.n.tcpFlows <- struct{}{}
	}
	antes := marcadas()
	if err := eco(); err == nil {
		t.Fatal("a connection above MaxTCPFlows went through")
	}
	if marcadas() != antes {
		t.Fatal("the host dialed out for a connection above MaxTCPFlows")
	}
	<-r.n.tcpFlows
	if err := eco(); err != nil {
		t.Fatalf("with room again: %v", err)
	}
	// La que acaba devuelve su plaza.
	limite := time.Now().Add(3 * time.Second)
	for len(r.n.tcpFlows) != MaxTCPFlows-1 {
		if time.Now().After(limite) {
			t.Fatalf("%d plazas ocupadas tras cerrar, quería %d", len(r.n.tcpFlows), MaxTCPFlows-1)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// La pila sale con los buffers TCP agrandados, SACK y autoescala.
func TestOpcionesTCP(t *testing.T) {
	r := newRig(t, "")
	var rx tcpip.TCPReceiveBufferSizeRangeOption
	if e := r.n.stack.TransportProtocolOption(tcp.ProtocolNumber, &rx); e != nil || rx.Max != tcpRXBufMax {
		t.Fatalf("receive buffer max = %d (%v), quería %d", rx.Max, e, tcpRXBufMax)
	}
	var tx tcpip.TCPSendBufferSizeRangeOption
	if e := r.n.stack.TransportProtocolOption(tcp.ProtocolNumber, &tx); e != nil || tx.Max != tcpTXBufMax {
		t.Fatalf("send buffer max = %d (%v), quería %d", tx.Max, e, tcpTXBufMax)
	}
	var sack tcpip.TCPSACKEnabled
	if e := r.n.stack.TransportProtocolOption(tcp.ProtocolNumber, &sack); e != nil || !bool(sack) {
		t.Fatalf("SACK = %v (%v)", sack, e)
	}
}
