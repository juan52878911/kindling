//go:build darwin

package energia

import (
	"bufio"
	"io"
	"testing"
	"time"
)

// El vigía se registra (sin dormir el Mac de verdad: eso no se prueba aquí),
// dice que está listo y sale en cuanto el daemon cierra su lado.
func TestServirRegistraYSale(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	fin := make(chan int, 1)
	go func() { fin <- Servir(inR, outW) }()
	linea := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(outR)
		if sc.Scan() {
			linea <- sc.Text()
		}
	}()
	select {
	case l := <-linea:
		if l != "ready" {
			t.Fatalf("primera línea %q", l)
		}
	case c := <-fin:
		t.Fatalf("salió con %d sin registrarse", c)
	case <-time.After(5 * time.Second):
		t.Fatal("no se registró")
	}
	inW.Close()
	select {
	case c := <-fin:
		if c != 0 {
			t.Fatalf("salió con %d", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no salió al cerrar stdin")
	}
}

// Sin ack, esperarAck vuelve al vencer el plazo o al cerrarse el daemon; con
// el ack de otro sueño sigue esperando el suyo.
func TestEsperarAck(t *testing.T) {
	acks := make(chan int64, 2)
	cerrado := make(chan struct{})
	acks <- 7
	acks <- 8
	t0 := time.Now()
	esperarAck(8, acks, cerrado)
	if time.Since(t0) > time.Second {
		t.Fatal("no tomó su ack")
	}
	close(cerrado)
	esperarAck(9, acks, cerrado)
}
