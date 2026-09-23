// Package vnet es la red de espacio de usuario de una microVM: una pila TCP/IP
// de gVisor al otro lado de la tarjeta virtio-net del invitado.
//
// Imita la red que el núcleo monta en Linux con netns + veth + tap: el invitado
// es 172.16.0.2/30 con pasarela 172.16.0.1, sin DHCP. Así un snapshot sirve
// igual en los dos sistemas, porque el invitado despierta con su IP congelada
// en memoria y no hay forma de cambiarla. La diferencia es que aquí todo lo que
// en Linux hacen iptables, ipset y el resolver del host lo hace este proceso: la
// pila termina cada conexión del invitado y la vuelve a abrir desde el Mac solo
// si la política lo permite.
//
// El enlace con Virtualization.framework es un socketpair de datagramas
// (VZFileHandleNetworkDeviceAttachment): cada datagrama es una trama ethernet.
package vnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/juan52878911/kindling/vz/internal/egress"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

var (
	GuestIP   = netip.MustParseAddr("172.16.0.2")
	GatewayIP = netip.MustParseAddr("172.16.0.1")
	// GatewayMAC es fija a propósito: el invitado guarda en su caché ARP la MAC
	// de la pasarela, y un snapshot restaurado en otro proceso debe seguir
	// encontrándola sin volver a preguntar.
	GatewayMAC = "06:00:ac:10:00:01"
	// DefaultGuestMAC es la que manda el núcleo (knet.GuestMAC).
	DefaultGuestMAC = "06:00:ac:10:00:02"
	DefaultMMDSIP   = netip.MustParseAddr("169.254.169.254")
)

const (
	nicID   = 1
	mtu     = 1500
	subnetL = 30
	// Tamaños de buffer del socketpair. Apple recomienda que el de recepción
	// sea al menos el doble del de envío para no perder tramas en ráfagas.
	sndBuf = 1 << 20
	rcvBuf = 4 << 20

	dialTimeout = 10 * time.Second
	udpIdle     = 60 * time.Second
	dnsIdle     = 10 * time.Second
)

// Config es lo que la red necesita saber de la máquina.
type Config struct {
	GuestMAC string
	// MMDS, si no es nil, se sirve en MMDSAddr:80.
	MMDS     http.Handler
	MMDSAddr netip.Addr
	Policy   *egress.Policy
	Resolver *egress.Resolver
	// Dial abre las conexiones de salida en el host. Nil = net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	Logf func(format string, args ...any)
}

// Net es la red de una máquina.
type Net struct {
	cfg    Config
	stack  *stack.Stack
	ep     *channel.Endpoint
	conn   net.Conn // nuestro lado del socketpair
	vmFile *os.File // el lado que se entrega al framework
	ctx    context.Context
	cancel context.CancelFunc
	mmds   *http.Server

	mu       sync.Mutex
	forwards map[int]net.Listener
	closed   bool
	wg       sync.WaitGroup
}

// New crea el socketpair y la pila. El lado de la VM se obtiene con VMFile.
func New(cfg Config) (*Net, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	for _, fd := range fds {
		syscall.CloseOnExec(fd)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, sndBuf)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBuf)
	}
	vmFile := os.NewFile(uintptr(fds[0]), "vnet-vm")
	ours := os.NewFile(uintptr(fds[1]), "vnet-host")
	conn, err := net.FileConn(ours)
	ours.Close() // FileConn duplica el descriptor
	if err != nil {
		vmFile.Close()
		return nil, fmt.Errorf("socketpair conn: %w", err)
	}
	n, err := NewWithConn(cfg, conn)
	if err != nil {
		vmFile.Close()
		return nil, err
	}
	n.vmFile = vmFile
	return n, nil
}

// NewWithConn monta la pila sobre una conexión de datagramas ya abierta. Es la
// costura que usan los tests para poner otra pila de gVisor como "invitado".
func NewWithConn(cfg Config, conn net.Conn) (*Net, error) {
	if cfg.Policy == nil {
		cfg.Policy = egress.NewPolicy()
	}
	if cfg.Resolver == nil {
		cfg.Resolver = egress.NewResolver(cfg.Policy)
	}
	if cfg.GuestMAC == "" {
		cfg.GuestMAC = DefaultGuestMAC
	}
	if cfg.Dial == nil {
		d := &net.Dialer{}
		cfg.Dial = d.DialContext
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if !cfg.MMDSAddr.IsValid() {
		cfg.MMDSAddr = DefaultMMDSIP
	}
	guestMAC, err := tcpip.ParseMACAddress(cfg.GuestMAC)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("guest mac %q: %w", cfg.GuestMAC, err)
	}
	gwMAC, _ := tcpip.ParseMACAddress(GatewayMAC)

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4},
	})
	ep := channel.New(1024, mtu+header.EthernetMinimumSize, gwMAC)
	// El framework entrega las tramas por memoria, sin cable: no hay nada que
	// pueda corromperlas por el camino, y un invitado con offload de checksum
	// las mandaría con sumas parciales que la pila descartaría.
	ep.LinkEPCapabilities |= stack.CapabilityRXChecksumOffload

	ctx, cancel := context.WithCancel(context.Background())
	n := &Net{cfg: cfg, stack: s, ep: ep, conn: conn, ctx: ctx, cancel: cancel, forwards: map[int]net.Listener{}}
	fail := func(what string, e tcpip.Error) (*Net, error) {
		n.Close()
		return nil, fmt.Errorf("netstack %s: %s", what, e)
	}
	if e := s.CreateNIC(nicID, ethernet.New(ep)); e != nil {
		return fail("create nic", e)
	}
	gw := tcpip.AddrFrom4(GatewayIP.As4())
	if e := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol: ipv4.ProtocolNumber, AddressWithPrefix: gw.WithPrefix(),
	}, stack.AddressProperties{}); e != nil {
		return fail("gateway address", e)
	}
	// La dirección de MMDS es PROPIA de la pila y no solo un destino más: el
	// puente del invitado pone una ruta on-link a ella y pregunta por ARP, y
	// la pila solo contesta ARP por direcciones que tiene asignadas.
	if cfg.MMDS != nil {
		mm := tcpip.AddrFrom4(cfg.MMDSAddr.As4())
		if e := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
			Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: mm, PrefixLen: 32},
		}, stack.AddressProperties{}); e != nil {
			return fail("mmds address", e)
		}
	}
	// Promiscuo + suplantación: el invitado manda a la MAC de la pasarela
	// paquetes para cualquier IP, y las respuestas tienen que salir con la IP
	// de ese destino como origen.
	if e := s.SetPromiscuousMode(nicID, true); e != nil {
		return fail("promiscuous", e)
	}
	if e := s.SetSpoofing(nicID, true); e != nil {
		return fail("spoofing", e)
	}
	subnet, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{172, 16, 0, 0}), tcpip.MaskFromBytes([]byte{255, 255, 255, 252}))
	s.SetRouteTable([]tcpip.Route{
		{Destination: subnet, NIC: nicID},
		{Destination: header.IPv4EmptySubnet, NIC: nicID},
	})
	// Vecino estático: la MAC del invitado la fija el núcleo, y preguntar por
	// ARP desde una IP suplantada no siempre obtiene respuesta.
	if e := s.AddStaticNeighbor(nicID, ipv4.ProtocolNumber, tcpip.AddrFrom4(GuestIP.As4()), guestMAC); e != nil {
		return fail("static neighbor", e)
	}

	tcpFwd := tcp.NewForwarder(s, 0, 512, n.handleTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)
	udpFwd := udp.NewForwarder(s, n.handleUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udpFwd.HandlePacket)

	if cfg.MMDS != nil {
		l, err := gonet.ListenTCP(s, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(cfg.MMDSAddr.As4()), Port: 80}, ipv4.ProtocolNumber)
		if err != nil {
			n.Close()
			return nil, fmt.Errorf("mmds listener: %w", err)
		}
		n.mmds = &http.Server{
			Handler:           cfg.MMDS,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			MaxHeaderBytes:    16 << 10,
		}
		go func() { _ = n.mmds.Serve(l) }()
	}

	n.wg.Add(2)
	go n.rxLoop()
	go n.txLoop()
	return n, nil
}

// VMFile es el extremo del socketpair que se entrega al framework.
func (n *Net) VMFile() *os.File { return n.vmFile }

func (n *Net) rxLoop() {
	defer n.wg.Done()
	buf := make([]byte, 65536)
	for {
		k, err := n.conn.Read(buf)
		if err != nil {
			if n.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			// Un error transitorio (ENOBUFS) no debe tumbar la red.
			time.Sleep(time.Millisecond)
			continue
		}
		if k < header.EthernetMinimumSize {
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
			Payload: buffer.MakeWithData(append([]byte(nil), buf[:k]...)),
		})
		n.ep.InjectInbound(0, pkt)
		pkt.DecRef()
	}
}

func (n *Net) txLoop() {
	defer n.wg.Done()
	for {
		pkt := n.ep.ReadContext(n.ctx)
		if pkt == nil {
			return
		}
		v := pkt.ToView()
		// Con la VM en pausa el otro lado no lee y el envío falla con ENOBUFS:
		// se descarta la trama y TCP la retransmite, que es lo que haría un
		// cable desconectado un momento.
		_, _ = n.conn.Write(v.AsSlice())
		v.Release()
		pkt.DecRef()
	}
}

func addrOf(a tcpip.Address) netip.Addr {
	if a.Len() != 4 {
		return netip.Addr{}
	}
	return netip.AddrFrom4(a.As4())
}

// handleTCP decide cada conexión que abre el invitado. Corre en su propia
// goroutine (la lanza el forwarder), así que puede bloquearse en el dial.
func (n *Net) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := addrOf(id.LocalAddress)
	if id.LocalPort == 53 {
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		if err != nil {
			r.Complete(true)
			return
		}
		r.Complete(false)
		n.serveDNSTCP(gonet.NewTCPConn(&wq, ep))
		return
	}
	if !n.cfg.Policy.AllowConn(dst) {
		r.Complete(true)
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, dialTimeout)
	hc, err := n.cfg.Dial(ctx, "tcp4", netip.AddrPortFrom(dst, id.LocalPort).String())
	cancel()
	if err != nil {
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		hc.Close()
		r.Complete(true)
		return
	}
	r.Complete(false)
	splice(gonet.NewTCPConn(&wq, ep), hc)
}

func (n *Net) serveDNSTCP(c net.Conn) {
	defer c.Close()
	for {
		_ = c.SetDeadline(time.Now().Add(dnsIdle))
		q, err := egress.ReadTCPMsg(c)
		if err != nil {
			return
		}
		resp := n.cfg.Resolver.Process(n.ctx, q, true)
		if resp == nil || egress.WriteTCPMsg(c, resp) != nil {
			return
		}
	}
}

// handleUDP la llama la pila de forma síncrona: no puede bloquearse.
func (n *Net) handleUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := addrOf(id.LocalAddress)
	isDNS := id.LocalPort == 53
	if !isDNS && !n.cfg.Policy.AllowConn(dst) {
		return true // descartado en silencio, como el DROP de iptables
	}
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		return true
	}
	gc := gonet.NewUDPConn(&wq, ep)
	if isDNS {
		go n.serveDNSUDP(gc)
		return true
	}
	go func() {
		ctx, cancel := context.WithTimeout(n.ctx, dialTimeout)
		hc, err := n.cfg.Dial(ctx, "udp4", netip.AddrPortFrom(dst, id.LocalPort).String())
		cancel()
		if err != nil {
			gc.Close()
			return
		}
		relayUDP(gc, hc)
	}()
	return true
}

func (n *Net) serveDNSUDP(c *gonet.UDPConn) {
	defer c.Close()
	buf := make([]byte, 4096)
	for {
		_ = c.SetReadDeadline(time.Now().Add(dnsIdle))
		k, err := c.Read(buf)
		if err != nil {
			return
		}
		if resp := n.cfg.Resolver.Process(n.ctx, append([]byte(nil), buf[:k]...), false); resp != nil {
			_, _ = c.Write(resp)
		}
	}
}

// relayUDP reenvía un flujo UDP en los dos sentidos hasta que calla udpIdle.
func relayUDP(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	var once sync.Once
	done := make(chan struct{})
	pump := func(dst, src net.Conn) {
		buf := make([]byte, 65536)
		for {
			_ = src.SetReadDeadline(time.Now().Add(udpIdle))
			k, err := src.Read(buf)
			if err != nil {
				once.Do(func() { close(done) })
				return
			}
			_, _ = dst.Write(buf[:k])
		}
	}
	go pump(a, b)
	go pump(b, a)
	<-done
}

type closeWriter interface{ CloseWrite() error }

// splice copia en los dos sentidos respetando el medio cierre: HTTP/1.0 y
// muchos clientes cierran su lado de escritura y esperan la respuesta.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	cp := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	wg.Add(2)
	go cp(a, b)
	go cp(b, a)
	wg.Wait()
	a.Close()
	b.Close()
}

// Forward abre (o devuelve, si ya existe) un puerto en 127.0.0.1 que llega al
// puerto port del invitado. Es como el host alcanza a cada invitado en el Mac:
// todos son 172.16.0.2 en redes separadas, así que la IP no sirve.
func (n *Net) Forward(port int) (string, error) {
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port %d", port)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return "", errors.New("network is closed")
	}
	if l, ok := n.forwards[port]; ok {
		return l.Addr().String(), nil
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	n.forwards[port] = l
	go n.acceptForward(l, uint16(port))
	return l.Addr().String(), nil
}

// Forwards devuelve los puertos abiertos: puerto del invitado -> dirección.
func (n *Net) Forwards() map[int]string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make(map[int]string, len(n.forwards))
	for p, l := range n.forwards {
		out[p] = l.Addr().String()
	}
	return out
}

// ForwardPorts devuelve los puertos del invitado reenviados, ordenados.
func (n *Net) ForwardPorts() []int {
	m := n.Forwards()
	out := make([]int, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func (n *Net) acceptForward(l net.Listener, port uint16) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(n.ctx, 5*time.Second)
			gc, err := gonet.DialTCPWithBind(ctx, n.stack,
				tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(GatewayIP.As4())},
				tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(GuestIP.As4()), Port: port},
				ipv4.ProtocolNumber)
			cancel()
			if err != nil {
				c.Close()
				return
			}
			splice(c, gc)
		}()
	}
}

// Close cierra puertos, pila y socketpair.
func (n *Net) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	for _, l := range n.forwards {
		l.Close()
	}
	n.mu.Unlock()
	n.cancel()
	if n.mmds != nil {
		_ = n.mmds.Close()
	}
	n.conn.Close()
	n.ep.Close()
	n.stack.Close()
	n.wg.Wait()
	if n.vmFile != nil {
		n.vmFile.Close()
	}
}
