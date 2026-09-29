package grafo

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
	"github.com/juan52878911/kindling/pkg/linkbroker"
)

// daemonFalso hace de daemon al otro lado del socket: contesta cada petición
// con decidir (nil = rechazo) y guarda su copia de cada socket entregado para
// poder cortarlo.
type daemonFalso struct {
	t       *testing.T
	decidir func(linkbroker.Request) (string, error) // dirección a la que marcar

	mu        sync.Mutex
	pedidas   []linkbroker.Request
	copias    []*net.TCPConn
	arriendos []*net.UnixConn
}

func (d *daemonFalso) dial(ctx context.Context) (*net.UnixConn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	conv := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "par")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			d.t.Fatal(err)
		}
		return c.(*net.UnixConn)
	}
	vz, dm := conv(fds[0]), conv(fds[1])
	go d.atender(dm)
	return vz, nil
}

func (d *daemonFalso) atender(c *net.UnixConn) {
	req, err := linkbroker.LeerPeticion(c, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	d.mu.Lock()
	d.pedidas = append(d.pedidas, req)
	d.mu.Unlock()
	addr, err := d.decidir(req)
	if err != nil {
		_ = linkbroker.Rechazar(c, linkbroker.Response{Error: err.Error(), Reason: linkbroker.ReasonMachineUnavailable})
		c.Close()
		return
	}
	nc, err := net.Dial("tcp", addr)
	if err != nil {
		_ = linkbroker.Rechazar(c, linkbroker.Response{Error: err.Error(), Reason: linkbroker.ReasonUpstreamError})
		c.Close()
		return
	}
	tc := nc.(*net.TCPConn)
	if err := linkbroker.Entregar(c, linkbroker.Response{Machine: "0123456789abcdef"}, tc); err != nil {
		tc.Close()
		c.Close()
		return
	}
	d.mu.Lock()
	d.copias = append(d.copias, tc)
	d.arriendos = append(d.arriendos, c)
	d.mu.Unlock()
	linkbroker.EsperarFin(c)
	tc.Close()
	c.Close()
}

// cortarTodo es la invalidación del daemon: shutdown del socket y fin del
// arrendamiento.
func (d *daemonFalso) cortarTodo() {
	for fin := time.Now().Add(2 * time.Second); time.Now().Before(fin); time.Sleep(time.Millisecond) {
		d.mu.Lock()
		n := len(d.copias)
		d.mu.Unlock()
		if n > 0 {
			break
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for i, tc := range d.copias {
		linkbroker.Cortar(tc)
		d.arriendos[i].Close()
	}
	d.copias, d.arriendos = nil, nil
}

func eco(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); c.Close() }()
		}
	}()
	return ln.Addr().String()
}

func TestValidar(t *testing.T) {
	if err := Validar([]Enlace{{Host: "api.graph", Port: 8081}, {Host: "db.graph", Port: 5432}}); err != nil {
		t.Fatal(err)
	}
	for _, mal := range [][]Enlace{
		{{Host: "api.graph", Port: 8080}},
		{{Host: "api.graph", Port: 53}},
		{{Host: "api.graph", Port: 80}},
		{{Host: "api.graph", Port: 0}},
		{{Host: "api.example.com", Port: 8081}},
		{{Host: "api.graph", Port: 8081}, {Host: "db.graph", Port: 8081}},
	} {
		if err := Validar(mal); err == nil {
			t.Errorf("aceptado: %+v", mal)
		}
	}
	muchos := make([]Enlace, MaxEnlaces+1)
	for i := range muchos {
		muchos[i] = Enlace{Host: "api.graph", Port: 10000 + i}
	}
	if err := Validar(muchos); err == nil {
		t.Error("aceptó más enlaces que el tope")
	}
}

// par de conexiones TCP en loopback: el "invitado" y lo que ve la red.
func parTCP(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lado := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		lado <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-lado
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// Serve: pide al daemon SOLO la arista (host y puerto, nunca una dirección),
// completa la del invitado con el destino ya abierto, y un corte del daemon
// termina la sesión aunque el invitado no cierre. Todo queda auditado.
func TestServeEnlace(t *testing.T) {
	dest := eco(t)
	d := &daemonFalso{t: t, decidir: func(r linkbroker.Request) (string, error) {
		if r.Kind == linkbroker.KindLink && r.Host == "api.graph" && r.Port == 8081 {
			return dest, nil
		}
		return "", io.ErrUnexpectedEOF
	}}
	audPath := filepath.Join(t.TempDir(), credproxy.AuditFile)
	aud := credproxy.NewAuditor(audPath, nil)
	g := NewConDial(d.dial, aud, nil)
	if err := g.Set([]Enlace{{Host: "api.graph", Port: 8081}, {Host: "db.graph", Port: 5432}}); err != nil {
		t.Fatal(err)
	}
	if !g.Link(8081) || !g.Link(5432) || g.Link(9090) {
		t.Fatal("Link no refleja las aristas")
	}

	invitado, red := parTCP(t)
	hecho := make(chan struct{})
	go func() {
		defer close(hecho)
		g.Serve(context.Background(), 8081, func() (net.Conn, error) { return red, nil }, func() { t.Error("rechazada") })
	}()
	if _, err := invitado.Write([]byte("hola")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(invitado, b); err != nil || string(b) != "hola" {
		t.Fatalf("eco: %q %v", b, err)
	}
	d.cortarTodo()
	_ = invitado.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := invitado.Read(b); err == nil {
		t.Fatal("la sesión siguió tras el corte del daemon")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("tras el corte, el invitado se quedó esperando")
	}
	select {
	case <-hecho:
	case <-time.After(3 * time.Second):
		t.Fatal("Serve no terminó tras el corte")
	}

	// Una arista que el daemon rechaza: RST al invitado, sin aceptar.
	rechazada := false
	g.Serve(context.Background(), 5432, func() (net.Conn, error) {
		t.Error("aceptó una conexión rechazada")
		return nil, io.EOF
	}, func() { rechazada = true })
	if !rechazada {
		t.Fatal("no rechazó")
	}

	d.mu.Lock()
	for _, r := range d.pedidas {
		if r.Machine != "" || r.Owner != "" {
			t.Errorf("la petición de enlace llevaba máquina: %+v", r)
		}
	}
	d.mu.Unlock()

	aud.Close()
	raw, err := os.ReadFile(audPath)
	if err != nil {
		t.Fatal(err)
	}
	lineas := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lineas) != 2 {
		t.Fatalf("auditoría: %q", raw)
	}
	var r1, r2 credproxy.Record
	_ = json.Unmarshal([]byte(lineas[0]), &r1)
	_ = json.Unmarshal([]byte(lineas[1]), &r2)
	if r1.Kind != credproxy.KindLink || r1.Host != "api.graph:8081" || r1.Upstream != "machine:0123456789abcdef" ||
		r1.Reason != credproxy.ReasonInvalidated || r1.ReqBytes != 4 {
		t.Errorf("registro del enlace cortado: %+v", r1)
	}
	if !r2.Denied || r2.Reason != credproxy.ReasonMachineUnavailable || r2.Host != "db.graph:5432" {
		t.Errorf("registro del rechazo: %+v", r2)
	}
}

// Por encima de MaxConnsEnlace conexiones a la vez en una arista, la
// siguiente se rechaza sin preguntar al daemon.
func TestServeTope(t *testing.T) {
	soltar := make(chan struct{})
	dest := eco(t)
	d := &daemonFalso{t: t, decidir: func(linkbroker.Request) (string, error) {
		<-soltar
		return dest, nil
	}}
	g := NewConDial(d.dial, nil, nil)
	if err := g.Set([]Enlace{{Host: "api.graph", Port: 8081}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range MaxConnsEnlace {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, red := parTCP(t)
			g.Serve(context.Background(), 8081, func() (net.Conn, error) { red.Close(); return red, nil }, func() {})
		}()
	}
	for {
		d.mu.Lock()
		n := len(d.pedidas)
		d.mu.Unlock()
		if n == MaxConnsEnlace {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	rechazada := false
	g.Serve(context.Background(), 8081, func() (net.Conn, error) { return nil, io.EOF }, func() { rechazada = true })
	if !rechazada {
		t.Fatal("pasó por encima del tope")
	}
	d.mu.Lock()
	if len(d.pedidas) != MaxConnsEnlace {
		t.Fatalf("la de sobra llegó al daemon")
	}
	d.mu.Unlock()
	close(soltar)
	wg.Wait()
}

// DialMachine: la conexión de una credencial hacia otra máquina, con su
// arrendamiento. Si el daemon lo corta, la conexión muere.
func TestDialMachine(t *testing.T) {
	dest := eco(t)
	d := &daemonFalso{t: t, decidir: func(r linkbroker.Request) (string, error) {
		if r.Kind == linkbroker.KindMachine && r.Machine == "0123456789abcdef0123456789abcdef" && r.Owner == "g1" && r.Port == 5432 {
			return dest, nil
		}
		return "", io.ErrUnexpectedEOF
	}}
	g := NewConDial(d.dial, nil, nil)
	c, err := g.DialMachine(context.Background(), "0123456789abcdef0123456789abcdef", "g1", 5432)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.DialMachine(context.Background(), "0123456789abcdef0123456789abcdef", "otro", 5432); err == nil {
		t.Fatal("el daemon dijo que no y hubo conexión")
	}
	if _, err := c.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 1)
	if _, err := io.ReadFull(c, b); err != nil {
		t.Fatal(err)
	}
	d.cortarTodo()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(b); err == nil {
		t.Fatal("la conexión siguió tras el corte")
	}
	deadline := time.Now().Add(3 * time.Second)
	for !c.(*Conexion).Cortada() {
		if time.Now().After(deadline) {
			t.Fatal("no se marcó como cortada por el daemon")
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.Close()
}

// Cerrar la conexión suelta el arrendamiento: el daemon lo ve.
func TestCerrarSueltaArrendamiento(t *testing.T) {
	dest := eco(t)
	d := &daemonFalso{t: t, decidir: func(linkbroker.Request) (string, error) { return dest, nil }}
	g := NewConDial(d.dial, nil, nil)
	c, err := g.DialMachine(context.Background(), "0123456789abcdef", "local", 5432)
	if err != nil {
		t.Fatal(err)
	}
	var arr *net.UnixConn
	for arr == nil {
		d.mu.Lock()
		if len(d.arriendos) > 0 {
			arr = d.arriendos[0]
		}
		d.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	c.Close()
	fin := make(chan struct{})
	go func() { linkbroker.EsperarFin(arr); close(fin) }()
	select {
	case <-fin:
	case <-time.After(3 * time.Second):
		t.Fatal("el daemon no vio soltar el arrendamiento")
	}
	if c.(*Conexion).Cortada() {
		t.Fatal("un cierre propio no es un corte del daemon")
	}
}
