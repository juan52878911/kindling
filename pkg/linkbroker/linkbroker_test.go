//go:build unix

package linkbroker

import (
	"io"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// par devuelve los dos extremos de un socketpair Unix de flujo, como
// net.UnixConn: kling-vz y el daemon.
func par(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	conv := func(fd int) *net.UnixConn {
		f := os.NewFile(uintptr(fd), "par")
		defer f.Close()
		c, err := net.FileConn(f)
		if err != nil {
			t.Fatal(err)
		}
		return c.(*net.UnixConn)
	}
	a, b := conv(fds[0]), conv(fds[1])
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

// eco es un servidor TCP que devuelve lo que recibe: el "destino".
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

func TestValidate(t *testing.T) {
	buenas := []Request{
		{V: 1, Kind: KindLink, Host: "api.graph", Port: 8081},
		{V: 1, Kind: KindMachine, Machine: "0123456789abcdef", Owner: "local", Port: 5432},
	}
	for _, r := range buenas {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	malas := []Request{
		{V: 2, Kind: KindLink, Host: "api.graph", Port: 8081},
		{V: 1, Kind: KindLink, Host: "api.graph", Port: 0},
		{V: 1, Kind: KindLink, Host: "api.graph", Port: 70000},
		{V: 1, Kind: KindLink, Host: "api", Port: 8081},
		{V: 1, Kind: KindLink, Host: "127.0.0.1", Port: 8081},
		{V: 1, Kind: KindLink, Host: "a.b.graph", Port: 8081},
		{V: 1, Kind: KindLink, Host: ".graph", Port: 8081},
		{V: 1, Kind: KindLink, Host: "api.graph", Port: 8081, Machine: "0123456789abcdef"},
		{V: 1, Kind: KindMachine, Machine: "127.0.0.1:5432", Owner: "local", Port: 5432},
		{V: 1, Kind: KindMachine, Machine: "0123456789abcdef", Owner: "", Port: 5432},
		{V: 1, Kind: KindMachine, Machine: "0123456789abcdef", Owner: "local", Port: 5432, Host: "db.graph"},
		{V: 1, Kind: "raw", Port: 5432},
	}
	for _, r := range malas {
		if err := r.Validate(); err == nil {
			t.Errorf("aceptada: %+v", r)
		}
	}
}

// El camino bueno: el daemon marca, entrega el socket y kling-vz habla por él
// con el destino. Cortar lo termina en los dos lados.
func TestEntregaYCorte(t *testing.T) {
	vz, daemon := par(t)
	dest := eco(t)
	listo := make(chan *net.TCPConn, 1)
	go func() {
		req, err := LeerPeticion(daemon, time.Second)
		if err != nil || req.Host != "api.graph" || req.Port != 8081 {
			t.Errorf("petición: %+v %v", req, err)
			listo <- nil
			return
		}
		c, err := net.Dial("tcp", dest)
		if err != nil {
			t.Error(err)
			listo <- nil
			return
		}
		tc := c.(*net.TCPConn)
		if err := Entregar(daemon, Response{Machine: "0123456789abcdef"}, tc); err != nil {
			t.Error(err)
		}
		listo <- tc
	}()
	tc, resp, err := Pedir(vz, Request{V: Version, Kind: KindLink, Host: "api.graph", Port: 8081}, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	if !resp.OK || resp.Machine != "0123456789abcdef" {
		t.Fatalf("respuesta: %+v", resp)
	}
	copiaDaemon := <-listo
	if copiaDaemon == nil {
		t.FailNow()
	}
	if _, err := tc.Write([]byte("hola")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err := io.ReadFull(tc, b); err != nil || string(b) != "hola" {
		t.Fatalf("eco: %q %v", b, err)
	}
	// El daemon corta con su copia: la de kling-vz ve el fin aunque siga
	// abierta.
	Cortar(copiaDaemon)
	_ = tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := tc.Read(b); err == nil {
		t.Fatalf("tras cortar se leyeron %d bytes", n)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("tras cortar, la lectura se quedó esperando")
	}
	// El arrendamiento: cerrar un lado lo ve el otro.
	vz.Close()
	fin := make(chan struct{})
	go func() { EsperarFin(daemon); close(fin) }()
	select {
	case <-fin:
	case <-time.After(2 * time.Second):
		t.Fatal("el daemon no vio el fin del arrendamiento")
	}
}

// Un rechazo llega con su motivo y sin descriptor.
func TestRechazo(t *testing.T) {
	vz, daemon := par(t)
	go func() {
		if _, err := LeerPeticion(daemon, time.Second); err != nil {
			t.Error(err)
		}
		_ = Rechazar(daemon, Response{OK: true, Error: "graph tienda has no link edge web -> db:5432", Reason: ReasonMachineUnavailable})
	}()
	tc, resp, err := Pedir(vz, Request{V: Version, Kind: KindLink, Host: "db.graph", Port: 5432}, 5*time.Second)
	if err == nil || tc != nil {
		t.Fatal("un rechazo dio conexión")
	}
	if resp.OK || resp.Reason != ReasonMachineUnavailable || !strings.Contains(err.Error(), "no link edge") {
		t.Fatalf("respuesta: %+v %v", resp, err)
	}
}

// Un "sí" sin descriptor, o con algo que no es TCP, no se da por bueno.
func TestEntregaSinSocketTCP(t *testing.T) {
	vz, daemon := par(t)
	go func() {
		_, _ = LeerPeticion(daemon, time.Second)
		// Un fichero cualquiera en vez de un socket TCP.
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		_, _, _ = daemon.WriteMsgUnix([]byte(`{"ok":true}`+"\n"), syscall.UnixRights(int(f.Fd())), nil)
	}()
	if tc, _, err := Pedir(vz, Request{V: Version, Kind: KindLink, Host: "api.graph", Port: 8081}, 5*time.Second); err == nil {
		tc.Close()
		t.Fatal("aceptó un descriptor que no es TCP")
	}

	vz2, daemon2 := par(t)
	go func() {
		_, _ = LeerPeticion(daemon2, time.Second)
		_, _ = daemon2.Write([]byte(`{"ok":true}` + "\n"))
	}()
	if tc, _, err := Pedir(vz2, Request{V: Version, Kind: KindLink, Host: "api.graph", Port: 8081}, 5*time.Second); err == nil {
		tc.Close()
		t.Fatal("aceptó un sí sin descriptor")
	}
}

// El daemon no acepta campos que no conoce ni líneas sin fin, y cierra lo que
// le manden como descriptor.
func TestLeerPeticionEstricta(t *testing.T) {
	vz, daemon := par(t)
	go func() {
		_, _ = vz.Write([]byte(`{"v":1,"kind":"link","host":"api.graph","port":8081,"addr":"127.0.0.1:29001"}` + "\n"))
	}()
	if _, err := LeerPeticion(daemon, time.Second); err == nil {
		t.Fatal("aceptó un campo desconocido (una dirección)")
	}

	vz2, daemon2 := par(t)
	go func() { _, _ = vz2.Write([]byte(strings.Repeat("x", MaxRequest+10))) }()
	if _, err := LeerPeticion(daemon2, time.Second); err == nil {
		t.Fatal("aceptó una línea sin fin")
	}

	vz3, daemon3 := par(t)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	go func() {
		_, _, _ = vz3.WriteMsgUnix([]byte(`{"v":1,"kind":"link","host":"api.graph","port":8081}`+"\n"), syscall.UnixRights(int(w.Fd())), nil)
		w.Close()
	}()
	if _, err := LeerPeticion(daemon3, time.Second); err != nil {
		t.Fatal(err)
	}
	// El descriptor que llegó con la petición se cerró: el pipe ve EOF en
	// cuanto el emisor suelta el suyo.
	_ = r.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := r.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("el descriptor recibido sigue abierto: %v", err)
	}
}
