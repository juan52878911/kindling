package scheduler

import (
	"context"
	"net"
	"testing"
	"time"
)

// Una instancia con reenvíos (macOS) se alcanza por ellos, no por su IP, que
// allí es la misma para todos los invitados.
func TestInstanceAddrUsaReenvios(t *testing.T) {
	e := &entry{ip: "172.16.0.2", fwd: map[string]string{"8080": "127.0.0.1:61234"}}
	if got := e.Addr(GuestPort); got != "127.0.0.1:61234" {
		t.Fatalf("Addr = %q", got)
	}
	if e.IP() != "172.16.0.2" {
		t.Fatal("IP() se conserva para no romper a kindling-mcp")
	}
	rt := &sessionRoute{ip: e.ip, fwd: e.fwd}
	w := &warmVM{ip: e.ip, fwd: e.fwd}
	if rt.Addr(GuestPort) != e.Addr(GuestPort) || w.Addr(GuestPort) != e.Addr(GuestPort) {
		t.Fatal("ruta y precalentada resuelven igual que la instancia")
	}
	sinFwd := &entry{ip: "172.30.0.2"}
	if got := sinFwd.Addr(GuestPort); got != "172.30.0.2:8080" {
		t.Fatalf("Linux: Addr = %q", got)
	}
}

func TestWaitReadyAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := WaitReadyAddr(context.Background(), ln.Addr().String(), time.Second); err != nil {
		t.Fatalf("WaitReadyAddr: %v", err)
	}
	if !AliveAddr(ln.Addr().String()) {
		t.Fatal("AliveAddr sobre un puerto abierto")
	}
}
