//go:build darwin && arm64

package hvvm

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestVsockTxDesdeLosBuferes: lo que manda el invitado llega a Go entero y
// en orden cuando la cabecera y los datos van en búferes distintos (se
// escriben con writev desde su RAM) y cuando no cabe en el socket: lo que
// sobra espera en pend y vs_hilo lo vacía en cuanto Go lee, aunque tx ya no
// lo despierte en cada aviso.
func TestVsockTxDesdeLosBuferes(t *testing.T) {
	enviado := make([]byte, 4000+5*64*1024) // 4000 + (PRUEBA_TX - 1) * VS_PAYLOAD
	rand.New(rand.NewSource(2)).Read(enviado)
	leido, usoPend, ret := vsockTx(enviado)
	if ret < 0 {
		t.Fatalf("could not prepare: %d", ret)
	}
	if !usoPend {
		t.Error("nothing waited in pend: the test does not cover draining it")
	}
	if len(leido) != len(enviado) {
		t.Fatalf("Go read %d of %d bytes (the I/O thread did not drain pend?)", len(leido), len(enviado))
	}
	if !bytes.Equal(leido, enviado) {
		t.Fatal("the bytes Go read are not the ones the guest sent")
	}
}

// TestVsockLiberarNoCierraEnPoll: liberar (un RST del invitado en el vCPU,
// un dial que vence, el final de la prueba de tx) no cierra el fd que vs_hilo
// espera en poll: ese close se quedaba a veces colgado en el kernel para
// siempre (TestVsockTxDesdeLosBuferes, 10 min en CI). Lo cierra el hilo al
// salir de poll, y Go ve el fin igual.
func TestVsockLiberarNoCierraEnPoll(t *testing.T) {
	switch r := vsockFdEnPoll(); r {
	case 0:
	case 1:
		t.Fatal("liberar closed the fd while the vsock thread was polling it")
	case 2:
		t.Fatal("Go did not see EOF after the vsock thread left poll")
	case 3:
		t.Fatal("the vsock thread never polled the connection")
	default:
		t.Fatalf("could not prepare: %d", r)
	}
}

// Una vuelta de bombear (rx hacia el invitado) se hace con el cerrojo del
// vsock: el vCPU que notifica y la pausa la esperan. El crédito lo pone el
// invitado y puede reponer búferes sin avisar, así que sin tope una conexión
// con el host escribiendo sin parar la alargaba sin fin. Ahora mete como
// mucho VS_TANDA_MAX (1 MiB) y, con la VM en pausa, un solo paquete.
func TestVsockRxTandaAcotada(t *testing.T) {
	const tope, paquete = 1 << 20, 64 << 10
	for _, c := range []struct {
		pausa bool
		max   uint64
	}{{false, tope + paquete}, {true, paquete}} {
		lleno, movido, d, r := vsockTandaRx(c.pausa)
		if r != 0 {
			t.Fatalf("could not set up the test: %d", r)
		}
		if lleno < 2*tope {
			t.Skipf("the host socket only took %d bytes: not enough to see the cap", lleno)
		}
		if movido > c.max {
			t.Errorf("pause=%v: one round moved %d of %d bytes to the guest (at most %d), holding the lock %v",
				c.pausa, movido, lleno, c.max, d)
		}
		t.Logf("pause=%v: %d of %d bytes in one round, lock held %v", c.pausa, movido, lleno, d)
	}
}
