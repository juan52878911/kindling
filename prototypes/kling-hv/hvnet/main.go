// hvnet lanza kling-hv con una tarjeta virtio-net cuyo otro extremo es una
// pila TCP/IP de gVisor en este proceso (la misma idea que vz/internal/vnet,
// reducida a lo mínimo: pasarela 172.16.0.1, invitado 172.16.0.2, sin salida a
// internet). Mide cuánto tarda el agente kling-guest del invitado en contestar
// GET /healthz en el 8080 desde que se lanza kling-hv.
//
// Uso: hvnet [-kling-hv RUTA] [-path /healthz] [-hold 0s] -- ARGS DE kling-hv...
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

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
)

const (
	nicID      = 1
	mtu        = 1500
	gatewayMAC = "06:00:ac:10:00:01"
	guestMAC   = "06:00:ac:10:00:02"
)

var (
	gatewayIP = [4]byte{172, 16, 0, 1}
	guestIP   = [4]byte{172, 16, 0, 2}
)

func main() {
	bin := flag.String("kling-hv", "kling-hv", "ruta del VMM")
	path := flag.String("path", "/healthz", "ruta del agente a pedir")
	hold := flag.Duration("hold", 0, "mantener la VM viva este tiempo tras la respuesta")
	repeat := flag.Int("n", 1, "peticiones a medir tras la primera")
	flag.Parse()

	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_DGRAM, 0)
	if err != nil {
		fail("socketpair: %v", err)
	}
	for _, fd := range fds {
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 1<<20)
		_ = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4<<20)
	}
	vmFile := os.NewFile(uintptr(fds[0]), "vm")
	ours := os.NewFile(uintptr(fds[1]), "host")
	conn, err := net.FileConn(ours)
	if err != nil {
		fail("fileconn: %v", err)
	}
	ours.Close()

	st := pila(conn)

	args := append(flag.Args(), "--net-fd", "3")
	cmd := exec.Command(*bin, args...)
	cmd.ExtraFiles = []*os.File{vmFile}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		fail("start: %v", err)
	}
	vmFile.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return gonet.DialContextTCP(ctx, st, tcpip.FullAddress{NIC: nicID, Addr: tcpip.AddrFrom4(guestIP), Port: 8080}, ipv4.ProtocolNumber)
			},
		},
	}
	url := "http://172.16.0.2:8080" + *path
	intentos := 0
	for {
		select {
		case err := <-done:
			fail("kling-hv terminó antes de contestar: %v", err)
		default:
		}
		intentos++
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			cancel()
			fmt.Fprintf(os.Stderr, "[hvnet] GET %s -> %d %q a los %.1f ms (%d intentos)\n", *path, resp.StatusCode,
				trunc(string(body)), ms(time.Since(t0)), intentos)
			break
		}
		cancel()
		time.Sleep(2 * time.Millisecond)
	}
	for i := 0; i < *repeat-1; i++ {
		t := time.Now()
		resp, err := client.Get(url)
		if err != nil {
			fail("petición %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		fmt.Fprintf(os.Stderr, "[hvnet] petición %d: %.2f ms\n", i+2, ms(time.Since(t)))
	}
	if *hold > 0 {
		select {
		case <-time.After(*hold):
		case <-done:
		}
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	<-done
}

func pila(conn net.Conn) *stack.Stack {
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	gwMAC, _ := tcpip.ParseMACAddress(gatewayMAC)
	gMAC, _ := tcpip.ParseMACAddress(guestMAC)
	ep := channel.New(1024, mtu+header.EthernetMinimumSize, gwMAC)
	ep.LinkEPCapabilities |= stack.CapabilityRXChecksumOffload
	if e := s.CreateNIC(nicID, ethernet.New(ep)); e != nil {
		fail("nic: %s", e)
	}
	gw := tcpip.AddrFrom4(gatewayIP)
	if e := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: gw.WithPrefix()},
		stack.AddressProperties{}); e != nil {
		fail("addr: %s", e)
	}
	subnet, _ := tcpip.NewSubnet(tcpip.AddrFrom4([4]byte{172, 16, 0, 0}), tcpip.MaskFromBytes([]byte{255, 255, 255, 252}))
	s.SetRouteTable([]tcpip.Route{{Destination: subnet, NIC: nicID}})
	if e := s.AddStaticNeighbor(nicID, ipv4.ProtocolNumber, tcpip.AddrFrom4(guestIP), gMAC); e != nil {
		fail("neighbor: %s", e)
	}
	go func() { // invitado -> pila
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if n < header.EthernetMinimumSize {
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
			ep.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	go func() { // pila -> invitado
		for {
			pkt := ep.ReadContext(context.Background())
			if pkt == nil {
				return
			}
			var b []byte
			for _, v := range pkt.AsSlices() {
				b = append(b, v...)
			}
			pkt.DecRef()
			_, _ = conn.Write(b)
		}
	}()
	return s
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func trunc(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "hvnet: "+f+"\n", a...)
	os.Exit(1)
}
