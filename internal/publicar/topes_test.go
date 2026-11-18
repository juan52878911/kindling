package publicar

import (
	"bufio"
	"io"
	"net"
	"os"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// errAccept es un Accept que falla como con EMFILE: ni cierre ni plazo.
type errAccept struct {
	net.Listener
	fallos atomic.Int32
}

func (l *errAccept) Accept() (net.Conn, error) {
	if l.fallos.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: os.NewSyscallError("accept", syscall.EMFILE)}
	}
	return l.Listener.Accept()
}

// Un error de Accept que no es el cierre (EMFILE) no deja el puerto muerto:
// se reintenta y la conexión que esperaba en el backlog se atiende.
func TestAcceptSobreviveAEMFILE(t *testing.T) {
	srv := eco(t)
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"m1": {ID: "m1", Name: "db", State: api.StateRunning, Labels: map[string]string{api.LabelPorts: "5432"}, Forwards: map[string]string{"5432": srv.Addr().String()}},
	}}
	p := nuevo(t, f)
	p.escuchar = func(a string) (net.Listener, error) {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			return nil, err
		}
		l := &errAccept{Listener: ln}
		l.fallos.Store(3)
		return l, nil
	}
	mc, _ := f.Get("m1")
	pub, err := p.Publicar(mc, []api.PortPublish{{Host: 0, Guest: 5432}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := pedir(t, "127.0.0.1:"+strconv.Itoa(pub.Ports[0].Host), "hola")
	if err != nil || got != "eco:hola" {
		t.Fatalf("tras EMFILE: got %q, %v", got, err)
	}
}

// Con el tope de conexiones lleno, la de más se corta en seguida; al cerrar
// una, vuelve a haber sitio.
func TestTopeConexionesTCP(t *testing.T) {
	srv := eco(t)
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"m1": {ID: "m1", Name: "db", State: api.StateRunning, Labels: map[string]string{api.LabelPorts: "5432"}, Forwards: map[string]string{"5432": srv.Addr().String()}},
	}}
	p := nuevo(t, f)
	p.topeTCP = 1
	mc, _ := f.Get("m1")
	pub, err := p.Publicar(mc, []api.PortPublish{{Host: 0, Guest: 5432}})
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(pub.Ports[0].Host)
	c1, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_ = c1.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = io.WriteString(c1, "uno\n")
	if r, err := bufio.NewReader(c1).ReadString('\n'); err != nil || r != "eco:uno\n" {
		t.Fatalf("primera: %q, %v", r, err)
	}
	if got, err := pedir(t, addr, "dos"); err == nil {
		t.Fatalf("segunda con el tope lleno: %q, want cortada", got)
	}
	c1.Close()
	limite := time.Now().Add(3 * time.Second)
	for {
		got, err := pedir(t, addr, "tres")
		if err == nil && got == "eco:tres" {
			break
		}
		if time.Now().After(limite) {
			t.Fatalf("tras cerrar la primera: %q, %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ecoUDP es un invitado UDP que devuelve cada datagrama con "eco:" delante.
func ecoUDP(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		b := make([]byte, 1500)
		for {
			n, de, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(append([]byte("eco:"), b[:n]...), de)
		}
	}()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// Los flujos UDP de un puerto tienen tope: un origen nuevo desaloja al más
// ocioso en vez de sumar otro socket y otra goroutine durante udpOcioso.
func TestTopeFlujosUDP(t *testing.T) {
	guest := ecoUDP(t)
	f := &maquinasFalsas{ms: map[string]*api.Machine{
		"a": {ID: "a", Name: "dns", State: api.StateRunning, IP: "127.0.0.1"},
	}}
	p := nuevo(t, f)
	p.topeUDP = 2
	ma, _ := f.Get("a")
	pub, err := p.Publicar(ma, []api.PortPublish{{Host: 0, Guest: guest, Proto: "udp"}})
	if err != nil {
		t.Fatal(err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(pub.Ports[0].Host)
	for i := range 5 {
		c, err := net.Dial("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		msg := "m" + strconv.Itoa(i)
		_, _ = c.Write([]byte(msg))
		b := make([]byte, 100)
		if n, err := c.Read(b); err != nil || string(b[:n]) != "eco:"+msg {
			t.Fatalf("origen %d: %q, %v", i, b[:n], err)
		}
	}
	p.mu.Lock()
	mq := p.por["a"]
	p.mu.Unlock()
	limite := time.Now().Add(3 * time.Second)
	for mq.activas.Load() > 2 {
		if time.Now().After(limite) {
			t.Fatalf("flujos abiertos = %d, want <= 2", mq.activas.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
