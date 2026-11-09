//go:build darwin

package energia

import (
	"bufio"
	"io"
	"testing"
	"time"
)

// El vigía se registra (sin dormir el Mac de verdad: eso no se prueba aquí),
// dice que está listo y de dónde come el Mac, y sale en cuanto el daemon
// cierra su lado.
func TestServirRegistraYSale(t *testing.T) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	fin := make(chan int, 1)
	go func() { fin <- Servir(inR, outW) }()
	lineas := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			lineas <- sc.Text()
		}
	}()
	for _, quiero := range []string{"ready", "power"} {
		select {
		case l := <-lineas:
			if quiero == "power" {
				// Un Mac de sobremesa también tiene fuente: corriente.
				if l != "power ac" && l != "power battery" {
					t.Fatalf("tras ready, %q; quería la fuente", l)
				}
				continue
			}
			if l != quiero {
				t.Fatalf("primera línea %q", l)
			}
		case c := <-fin:
			t.Fatalf("salió con %d sin registrarse", c)
		case <-time.After(5 * time.Second):
			t.Fatalf("no llegó %q", quiero)
		}
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

// Solo se dice la fuente cuando se sabe.
func TestLineaFuente(t *testing.T) {
	for v, quiero := range map[int32]string{1: "power battery", 0: "power ac", -1: "", sinLeer: ""} {
		if got := lineaFuente(v); got != quiero {
			t.Errorf("lineaFuente(%d) = %q; quería %q", v, got, quiero)
		}
	}
}
