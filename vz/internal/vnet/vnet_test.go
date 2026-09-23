package vnet

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/vz/internal/egress"
	"github.com/juan52878911/kindling/vz/internal/mmds"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

// guest es una segunda pila de gVisor con la configuración que el kernel del
// invitado recibe por ip=: 172.16.0.2/30, pasarela 172.16.0.1, y la ruta on-link
// a MMDS que pone el puente. Hace de "VM" al otro lado del socketpair.
type guest struct {
	s      *stack.Stack
	ep     *channel.Endpoint
	conn   net.Conn
	cancel context.CancelFunc
}

func newGuest(t *testing.T, conn net.Conn) *guest {
	t.Helper()
	mac, _ := tcpip.ParseMACAddress(DefaultGuestMAC)
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	ep := channel.New(256, mtu+header.EthernetMinimumSize, mac)
	if e := s.CreateNIC(1, ethernet.New(ep)); e != nil {
		t.Fatal(e)
	}
	if e := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(GuestIP.As4()), PrefixLen: subnetL}},
		stack.AddressProperties{}); e != nil {
		t.Fatal(e)
	}
	subnet, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{172, 16, 0, 0}), tcpip.MaskFromBytes([]byte{255, 255, 255, 252}))
	mmdsNet, _ := tcpip.NewSubnet(tcpip.AddrFrom4(DefaultMMDSIP.As4()), tcpip.MaskFromBytes([]byte{255, 255, 255, 255}))
	s.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: 1},
		{Destination: mmdsNet, NIC: 1},
		{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(GatewayIP.As4()), NIC: 1},
	})
	ctx, cancel := context.WithCancel(context.Background())
	g := &guest{s: s, ep: ep, conn: conn, cancel: cancel}
	go func() {
		buf := make([]byte, 65536)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:k]...))})
			ep.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	go func() {
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			_, _ = conn.Write(v.AsSlice())
			v.Release()
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		conn.Close()
		ep.Close()
		s.Close()
	})
	return g
}

func (g *guest) dialTCP(ctx context.Context, addr string) (net.Conn, error) {
	ap := netip.MustParseAddrPort(addr)
	return gonet.DialContextTCP(ctx, g.s, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
}

func (g *guest) httpClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			return g.dialTCP(ctx, net.JoinHostPort(host, port))
		},
	}}
}

func socketpair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conns := make([]net.Conn, 2)
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "pair")
		c, err := net.FileConn(f)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		conns[i] = c
	}
	return conns[0], conns[1]
}

type rig struct {
	n      *Net
	g      *guest
	policy *egress.Policy
	store  *mmds.Store
	mu     sync.Mutex
	dialed []string
}

// newRig monta host y "invitado". Las salidas al host van a dialTo en vez de a
// internet: el test comprueba A DÓNDE querría salir, no que haya red.
func newRig(t *testing.T, dialTo string) *rig {
	t.Helper()
	r := &rig{policy: egress.NewPolicy(), store: mmds.NewStore()}
	hostSide, guestSide := socketpair(t)
	resolver := &egress.Resolver{Policy: r.policy, Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		// Upstream falso: example.com -> 93.184.215.14 con TTL 30.
		resp := append([]byte(nil), q...)
		resp[2] |= 0x80
		resp[7] = 1
		return append(resp, 0xC0, 12, 0, 1, 0, 1, 0, 0, 0, 30, 0, 4, 93, 184, 215, 14), nil
	}}
	n, err := NewWithConn(Config{
		MMDS:     r.store.Handler(),
		Policy:   r.policy,
		Resolver: resolver,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			r.mu.Lock()
			r.dialed = append(r.dialed, network+" "+addr)
			r.mu.Unlock()
			if dialTo == "" {
				return nil, errors.New("no route in tests")
			}
			var d net.Dialer
			return d.DialContext(ctx, network[:3], dialTo)
		},
	}, hostSide)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	r.n = n
	r.g = newGuest(t, guestSide)
	return r
}

func TestForwardReachesGuest(t *testing.T) {
	r := newRig(t, "")
	l, err := gonet.ListenTCP(r.g.s, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(GuestIP.As4()), Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				line, _ := bufio.NewReader(c).ReadString('\n')
				fmt.Fprintf(c, "guest got %s", line)
			}()
		}
	}()
	addr, err := r.n.Forward(8080)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := r.n.Forward(8080); again != addr {
		t.Fatalf("a repeated forward returned %s, want %s", again, addr)
	}
	if !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("forward must listen on loopback, got %s", addr)
	}
	for i := 0; i < 3; i++ {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(c, "ping %d\n", i)
		got, _ := io.ReadAll(c)
		c.Close()
		if string(got) != fmt.Sprintf("guest got ping %d\n", i) {
			t.Fatalf("through the forward: %q", got)
		}
	}
	// Un puerto donde el invitado no escucha: la conexión se cierra, no cuelga.
	addr2, _ := r.n.Forward(9999)
	c, err := net.DialTimeout("tcp", addr2, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(7 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("a forward to a closed guest port must fail")
	}
	c.Close()
	// El reenvío acepta aunque dentro no escuche nadie; Probe sí distingue.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !r.n.Probe(ctx, 8080) {
		t.Fatal("probe of the listening guest port says closed")
	}
	if r.n.Probe(ctx, 9999) {
		t.Fatal("probe of a closed guest port says open")
	}
	if _, err := r.n.Forward(0); err == nil {
		t.Fatal("port 0 must be rejected")
	}
	if ports := r.n.ForwardPorts(); len(ports) != 2 || ports[0] != 8080 || ports[1] != 9999 {
		t.Fatalf("ForwardPorts = %v", ports)
	}
}

func TestMMDSFromGuest(t *testing.T) {
	r := newRig(t, "")
	r.store.Put(map[string]any{"env": map[string]any{"TOKEN": "s3cret"}})
	hc := r.g.httpClient()
	// El mismo flujo que pkg/guest/mmds.go.
	req, _ := http.NewRequest(http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	req.Header.Set("X-metadata-token-ttl-seconds", "60")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("token request from the guest: %v", err)
	}
	tok, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	resp.Body.Close()
	req, _ = http.NewRequest(http.MethodGet, "http://169.254.169.254/", nil)
	req.Header.Set("X-metadata-token", string(tok))
	req.Header.Set("Accept", "application/json")
	resp, err = hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "s3cret") {
		t.Fatalf("MMDS read: %d %s", resp.StatusCode, body)
	}
	// Otro puerto de la IP de MMDS no es MMDS: bloqueado como link-local.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.g.dialTCP(ctx, "169.254.169.254:8080"); err == nil {
		t.Fatal("169.254.169.254:8080 must be refused")
	}
}

func TestEgressNoneAndInternet(t *testing.T) {
	// Un "internet" local: el Dial del rig acaba aquí.
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
	try := func(addr string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c, err := r.g.dialTCP(ctx, addr)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write([]byte("hello")); err != nil {
			return err
		}
		buf := make([]byte, 5)
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		if string(buf) != "hello" {
			return fmt.Errorf("echo %q", buf)
		}
		return nil
	}

	if err := try("93.184.215.14:80"); err == nil {
		t.Fatal("egress none let the guest out")
	}
	r.policy.Set(egress.Internet, nil)
	if err := try("93.184.215.14:80"); err != nil {
		t.Fatalf("egress internet: %v", err)
	}
	for _, blocked := range []string{"192.168.1.1:80", "10.0.0.1:22", "172.16.0.1:80", "127.0.0.1:80"} {
		if err := try(blocked); err == nil {
			t.Fatalf("egress internet reached %s", blocked)
		}
	}
	r.mu.Lock()
	dialed := strings.Join(r.dialed, ",")
	r.mu.Unlock()
	if dialed != "tcp4 93.184.215.14:80" {
		t.Fatalf("the host only dialed the allowed destination; dialed %q", dialed)
	}
}

func dnsQuery(t *testing.T, g *guest, server, name string) byte {
	t.Helper()
	c, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(netip.MustParseAddr(server).As4()), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(egress.BuildQuery(name, 1)); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	k, err := c.Read(buf)
	if err != nil || k < 12 {
		t.Fatalf("no DNS answer from %s: %v", server, err)
	}
	return buf[3] & 0x0F
}

func TestDNSAndAllowlist(t *testing.T) {
	up, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer up.Close()
	go func() {
		for {
			c, err := up.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	r := newRig(t, up.Addr().String())
	// En none el DNS contesta REFUSED sin salir, a cualquier servidor.
	if rc := dnsQuery(t, r.g, "1.1.1.1", "example.com"); rc != 5 {
		t.Fatalf("none: rcode %d, want REFUSED", rc)
	}
	r.policy.Set(egress.Allowlist, []string{"example.com"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := r.g.dialTCP(ctx, "93.184.215.14:443"); err == nil {
		t.Fatal("allowlist: an IP must not open before being resolved")
	}
	if rc := dnsQuery(t, r.g, "172.16.0.1", "google.com"); rc != 5 {
		t.Fatalf("allowlist: unlisted name rcode %d, want REFUSED", rc)
	}
	if rc := dnsQuery(t, r.g, "8.8.8.8", "www.example.com"); rc != 0 {
		t.Fatalf("allowlist: listed name rcode %d", rc)
	}
	c, err := r.g.dialTCP(ctx, "93.184.215.14:443")
	if err != nil {
		t.Fatalf("allowlist: the resolved IP must open: %v", err)
	}
	c.Close()
}

func TestCloseIsIdempotent(t *testing.T) {
	r := newRig(t, "")
	r.n.Close()
	r.n.Close()
	if _, err := r.n.Forward(80); err == nil {
		t.Fatal("Forward after Close must fail")
	}
}

func TestNewCreatesSocketpair(t *testing.T) {
	n, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if n.VMFile() == nil {
		t.Fatal("no VM side of the socketpair")
	}
	st, err := n.VMFile().Stat()
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("VM file is not a socket: %v %v", st, err)
	}
}
