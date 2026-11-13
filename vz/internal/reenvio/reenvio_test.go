package reenvio

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/credproxy"
)

// proxyFalso escucha como el socket de reenvío: lee la cabecera, contesta
// "puerto N: " seguido de lo que reciba hasta EOF y cierra.
func proxyFalso(t *testing.T) (net.Listener, chan int) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	puertos := make(chan int, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				p, err := credproxy.LeerSplitHeader(c)
				if err != nil {
					return
				}
				puertos <- p
				datos, _ := io.ReadAll(c)
				io.WriteString(c, "eco:"+string(datos))
			}()
		}
	}()
	return l, puertos
}

func TestForwardPasaLaCabeceraYLosBytes(t *testing.T) {
	l, puertos := proxyFalso(t)
	r := New("", nil)
	r.Dial = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", l.Addr().String())
	}
	r.Fijar(true, true)
	if !r.Activo() || !r.DB() {
		t.Fatal("Fijar(true, true) no lo encendió")
	}
	// El invitado: un par TCP de verdad, para que haya CloseWrite.
	li, _ := net.Listen("tcp", "127.0.0.1:0")
	defer li.Close()
	invitado, _ := net.Dial("tcp", li.Addr().String())
	defer invitado.Close()
	lado, _ := li.Accept()
	hecho := make(chan struct{})
	go func() { r.Forward(context.Background(), lado, 5432); close(hecho) }()

	invitado.Write([]byte("hola"))
	invitado.(*net.TCPConn).CloseWrite()
	_ = invitado.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := io.ReadAll(invitado)
	if err != nil || string(resp) != "eco:hola" {
		t.Fatalf("respuesta %q, %v", resp, err)
	}
	if p := <-puertos; p != 5432 {
		t.Fatalf("cabecera con el puerto %d", p)
	}
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("Forward no terminó")
	}
}

// Sin proxy al otro lado, la conexión del invitado se cierra en el acto.
func TestForwardSinProxyCorta(t *testing.T) {
	r := New("/dev/null/no-existe.sock", nil)
	r.Fijar(true, false)
	if r.DB() {
		t.Fatal("DB encendido sin pedirlo")
	}
	a, b := net.Pipe()
	defer a.Close()
	go r.Forward(context.Background(), b, 80)
	_ = a.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err := a.Read(make([]byte, 1))
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("quería la conexión cerrada, %v", err)
	}
	r.Fijar(false, true)
	if r.Activo() || r.DB() {
		t.Fatal("Fijar(false, ...) no lo apagó")
	}
}

// Cancelar el contexto corta una conexión en curso.
func TestUnirSeCortaConElContexto(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	defer a1.Close()
	defer b2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	hecho := make(chan struct{})
	go func() { Unir(ctx, a2, b1); close(hecho) }()
	cancel()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("Unir no terminó al cancelar")
	}
}
