package vnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
)

// escucharEnInvitado sirve en el puerto port de la pila del invitado un eco
// de una línea con prefijo.
func escucharEnInvitado(t *testing.T, g *guest, port uint16, prefijo string) {
	t.Helper()
	l, err := gonet.ListenTCP(g.s, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(GuestIP.As4()), Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "%s %s", prefijo, line)
			}()
		}
	}()
}

func pedir(t *testing.T, addr, msg string) string {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprintf(c, "%s\n", msg)
	b, _ := io.ReadAll(c)
	return string(b)
}

// El puerto del agente con SetGuestDialer va por ese camino (el vsock) y no
// por la pila; si el camino falla (un agente anterior, que solo escucha TCP),
// la conexión sigue por TCP como siempre. La sonda, igual.
func TestGuestDialerConVueltaATCP(t *testing.T) {
	r := newRig(t, "")
	escucharEnInvitado(t, r.g, 8080, "tcp")
	var falla atomic.Bool
	var llamadas atomic.Int32
	r.n.SetGuestDialer(8080, func(ctx context.Context) (net.Conn, error) {
		llamadas.Add(1)
		if falla.Load() {
			return nil, errors.New("connection reset by peer")
		}
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			line, _ := bufio.NewReader(b).ReadString('\n')
			fmt.Fprintf(b, "vsock %s", line)
		}()
		return a, nil
	}, false)
	addr, err := r.n.Forward(8080)
	if err != nil {
		t.Fatal(err)
	}
	if got := pedir(t, addr, "hola"); got != "vsock hola\n" {
		t.Fatalf("con el camino propio: %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !r.n.Probe(ctx, 8080) {
		t.Fatal("la sonda del agente por vsock dice cerrado")
	}

	falla.Store(true)
	if got := pedir(t, addr, "hola"); got != "tcp hola\n" {
		t.Fatalf("sin vsock debe seguir por TCP: %q", got)
	}
	if !r.n.Probe(ctx, 8080) {
		t.Fatal("la sonda sin vsock no cae a TCP")
	}
	if llamadas.Load() < 4 {
		t.Fatalf("el camino propio se probó %d veces, quiero 4", llamadas.Load())
	}

	// Otro puerto no se ve afectado; y quitarlo deja solo TCP.
	r.n.SetGuestDialer(8080, nil, false)
	antes := llamadas.Load()
	if got := pedir(t, addr, "x"); got != "tcp x\n" || llamadas.Load() != antes {
		t.Fatalf("tras quitarlo: %q, llamadas %d -> %d", got, antes, llamadas.Load())
	}
}

// Un agente anterior no escucha por vsock: cuando falla vsock y TCP contesta,
// se deja de probar vsock (cada intento fallido cuesta ms). Mientras el
// invitado arranca y no contesta ninguno, no se decide nada.
func TestGuestDialerAgenteAnterior(t *testing.T) {
	r := newRig(t, "")
	var llamadas atomic.Int32
	r.n.SetGuestDialer(8080, func(ctx context.Context) (net.Conn, error) {
		llamadas.Add(1)
		return nil, errors.New("connection reset by peer")
	}, false)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pctx, pcancel := context.WithTimeout(ctx, 300*time.Millisecond)
	if r.n.Probe(pctx, 8080) {
		t.Fatal("nadie escucha todavía")
	}
	pcancel()
	if llamadas.Load() != 1 {
		t.Fatalf("llamadas = %d", llamadas.Load())
	}
	escucharEnInvitado(t, r.g, 8080, "tcp")
	if !r.n.Probe(ctx, 8080) {
		t.Fatal("arrancado, la sonda debe ver el TCP")
	}
	addr, _ := r.n.Forward(8080)
	for i := 0; i < 3; i++ {
		if got := pedir(t, addr, "z"); got != "tcp z\n" {
			t.Fatalf("%q", got)
		}
	}
	if llamadas.Load() != 2 {
		t.Fatalf("tras ver que solo hay TCP se siguió probando vsock: %d llamadas, quiero 2", llamadas.Load())
	}
	if !r.n.GuestOnlyTCP(8080) {
		t.Fatal("GuestOnlyTCP no lo dice (no viajaría en el snapshot)")
	}

	// Lo sabido de antes (un snapshot): ni se prueba.
	r.n.SetGuestDialer(8080, func(ctx context.Context) (net.Conn, error) {
		t.Error("con soloTCP no se debe probar vsock")
		return nil, errors.New("no")
	}, true)
	if got := pedir(t, addr, "w"); got != "tcp w\n" {
		t.Fatalf("%q", got)
	}
}

// Un camino propio que no contesta (el connect de vsock no vuelve) no deja
// colgado el reenvío: se corta a intentoGuestDial y sigue por TCP.
func TestGuestDialerQueNoContesta(t *testing.T) {
	r := newRig(t, "")
	escucharEnInvitado(t, r.g, 8080, "tcp")
	r.n.SetGuestDialer(8080, func(ctx context.Context) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}, false)
	addr, _ := r.n.Forward(8080)
	t0 := time.Now()
	if got := pedir(t, addr, "y"); got != "tcp y\n" {
		t.Fatalf("%q", got)
	}
	if d := time.Since(t0); d > intentoGuestDial+time.Second {
		t.Fatalf("tardó %s", d)
	}
}

// contarTramas recuerda la mayor trama que el host manda al invitado.
type contarTramas struct {
	net.Conn
	mu  sync.Mutex
	max int
}

func (c *contarTramas) Read(b []byte) (int, error) {
	k, err := c.Conn.Read(b)
	c.mu.Lock()
	c.max = max(c.max, k)
	c.mu.Unlock()
	return k, err
}

// socketpairGrande es socketpair con los búferes que pone New: sin ellos, un
// datagrama de dominio Unix en macOS no pasa de 2 KiB.
func socketpairGrande(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, b := socketpair(t)
	for _, c := range []net.Conn{a, b} {
		rc, err := c.(syscall.Conn).SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		_ = rc.Control(func(fd uintptr) {
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, sndBuf)
			_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBuf)
		})
	}
	return a, b
}

// Con Config.MTU, la pila manda tramas de ese tamaño al invitado (que tiene la
// misma MTU, como la adopta Linux de virtio-net) y un reenvío las usa.
func TestMTUGrande(t *testing.T) {
	const m = 16384
	hostSide, guestSide := socketpairGrande(t)
	n, err := NewWithConn(Config{MTU: m}, hostSide)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	ct := &contarTramas{Conn: guestSide}
	g := newGuestMTU(t, ct, m)
	l, err := gonet.ListenTCP(g.s, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(GuestIP.As4()), Port: 9000}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	const total = 4 << 20
	got := make(chan int64, 1)
	go func() {
		c, err := l.Accept()
		if err != nil {
			got <- -1
			return
		}
		k, _ := io.Copy(io.Discard, c)
		c.Close()
		got <- k
	}()
	addr, _ := n.Forward(9000)
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(c, zeros{}, total); err != nil {
		t.Fatal(err)
	}
	c.Close()
	select {
	case k := <-got:
		if k != total {
			t.Fatalf("el invitado recibió %d de %d bytes", k, total)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("la transferencia no terminó")
	}
	ct.mu.Lock()
	mx := ct.max
	ct.mu.Unlock()
	if mx <= DefaultMTU+14 || mx > m+14 {
		t.Fatalf("la mayor trama al invitado fue de %d bytes; quiero más de 1514 y hasta %d", mx, m+14)
	}
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) { clear(b); return len(b), nil }

func TestMTUFueraDeRango(t *testing.T) {
	for _, m := range []int{1499, 65536} {
		a, _ := socketpair(t)
		if _, err := NewWithConn(Config{MTU: m}, a); err == nil {
			t.Errorf("mtu %d aceptada", m)
		}
	}
}
