package transport

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewSinEndpointUsaElSocketLocal(t *testing.T) {
	if d := New(""); d.Endpoint != DefaultSocket {
		t.Errorf("New(\"\") = %q, esperaba %q", d.Endpoint, DefaultSocket)
	}
	if d := New("ssh://juan@lab"); d.Endpoint != "ssh://juan@lab" {
		t.Errorf("New no respeto el endpoint dado: %q", d.Endpoint)
	}
}

func TestDialLlegaAlSocketConYSinEsquema(t *testing.T) {
	dir, err := os.MkdirTemp("", "tr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aqui: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()

	// Las dos formas documentadas tienen que llegar al mismo sitio.
	for _, endpoint := range []string{sock, "unix://" + sock} {
		c, err := New(endpoint).Dial(context.Background())
		if err != nil {
			t.Errorf("Dial(%q): %v", endpoint, err)
			continue
		}
		c.Close()
	}
}

// El daemon de microVMs equivale a root en su host: puede montar discos y
// arrancar kernels arbitrarios. Por eso NUNCA escucha en un puerto de red, y por
// eso el dialer no debe aprender a hablar TCP.
//
// Este test existe para que anadir un `case "tcp://"` se ponga rojo. Es
// exactamente el error que costo a Docker una decada de servidores comprometidos,
// y el comentario de cabecera del paquete ya lo advierte — pero un comentario no
// falla cuando alguien lo ignora.
func TestElDialerNoAbreConexionesDeRed(t *testing.T) {
	// Un servidor TCP local de verdad: si el dialer aprendiera a hablar TCP, se
	// conectaria aqui y el test lo veria.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("sin TCP local: %v", err)
	}
	defer ln.Close()
	conectado := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			conectado <- struct{}{}
			c.Close()
		}
	}()

	for _, endpoint := range []string{
		"tcp://" + ln.Addr().String(),
		"http://" + ln.Addr().String(),
		ln.Addr().String(), // "127.0.0.1:54321" a secas
	} {
		c, err := New(endpoint).Dial(context.Background())
		if err == nil {
			c.Close()
			t.Errorf("Dial(%q) abrio una conexion; el daemon no debe ser alcanzable por red", endpoint)
			continue
		}
		// Y el error tiene que hablar del socket, no de la red: es lo que le dice
		// a quien se equivoco que aqui no hay un puerto que buscar.
		if !strings.Contains(err.Error(), "cannot talk to the daemon") {
			t.Errorf("Dial(%q) fallo con un error que despista: %v", endpoint, err)
		}
	}

	select {
	case <-conectado:
		t.Error("alguien se conecto al puerto TCP: el dialer habla red")
	default:
	}
}
