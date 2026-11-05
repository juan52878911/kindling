package vnet

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/juan52878911/kindling/vz/internal/egress"
)

// El loopback del Mac no se alcanza desde el invitado: ni por 127.0.0.1 (es
// el suyo), ni por la pasarela, en ningún modo de egress, aunque el Dial del
// host llegara a él. Con un puente (kling run -host-port, que vz/internal/grafo
// atiende como un puerto más de Graph) se alcanza ESE puerto de la pasarela,
// también con egress none, y ningún otro.
func TestLoopbackDelHostSoloPorPuente(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	_, sp, _ := net.SplitHostPort(ln.Addr().String())
	// Todo lo que el rig deje salir acaba en el servicio del loopback.
	r := newRig(t, ln.Addr().String())
	eco := func(addr string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := r.g.dialTCP(ctx, addr)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("ollama")); err != nil {
			return err
		}
		b := make([]byte, 6)
		_, err = io.ReadFull(c, b)
		return err
	}
	for _, modo := range []egress.Mode{egress.None, egress.Internet} {
		r.policy.Set(modo, nil)
		for _, addr := range []string{"127.0.0.1:" + sp, GatewayIP.String() + ":" + sp, GatewayIP.String() + ":11434"} {
			if err := eco(addr); err == nil {
				t.Fatalf("egress %s: sin puente el invitado llegó al loopback del host por %s", modo, addr)
			}
		}
	}
	r.mu.Lock()
	if len(r.dialed) != 0 {
		t.Fatalf("el host marcó %v sin puente", r.dialed)
	}
	r.mu.Unlock()

	// Con el puente 11434 de la pasarela -> el servicio, en egress none.
	r.policy.Set(egress.None, nil)
	r.n.cfg.Graph = &enlacesFalsos{destino: map[uint16]string{11434: ln.Addr().String()}}
	if err := eco(GatewayIP.String() + ":11434"); err != nil {
		t.Fatalf("por el puente: %v", err)
	}
	for _, addr := range []string{GatewayIP.String() + ":" + sp, "93.184.215.14:80"} {
		if err := eco(addr); err == nil {
			t.Fatalf("el puente abrió además %s", addr)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.dialed) != 0 {
		t.Fatalf("el puente salió por el Dial del host (debe ir por el broker): %v", r.dialed)
	}
}
