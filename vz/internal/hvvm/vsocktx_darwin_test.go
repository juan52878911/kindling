//go:build darwin && arm64

package hvvm

import (
	"bytes"
	"math/rand"
	"syscall"
	"testing"
	"time"
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
// mucho VS_TANDA_MAX (1 MiB) y, con la VM en pausa, un solo paquete. El
// control de las conexiones de detrás (aquí un CREDIT_UPDATE) que la vuelta
// cortada no atiende tiene que salir en la siguiente: el hilo queda avisado.
// Antes el aviso ya se había leído y esperaba a que otra cosa lo despertara.
func TestVsockRxTandaAcotada(t *testing.T) {
	const tope, paquete = 1 << 20, 64 << 10
	for _, c := range []struct {
		pausa bool
		max   uint64
	}{{false, tope + paquete}, {true, paquete}} {
		lleno, movido, d, control, r := vsockTandaRx(c.pausa)
		if r != 0 {
			t.Fatalf("could not set up the test: %d", r)
		}
		if lleno < 2*tope {
			t.Skipf("the host socket only took %d bytes: not enough to see the cap", lleno)
		}
		switch control {
		case 0:
		case 1:
			t.Errorf("pause=%v: the round stopped at the cap and left the vsock thread without a wake-up for the connection behind", c.pausa)
		case 2:
			t.Errorf("pause=%v: the next round did not send the pending credit update of the connection behind", c.pausa)
		case 3:
			t.Errorf("pause=%v: the round did not stop before the connection behind (moved %d of %d bytes)", c.pausa, movido, lleno)
		default:
			t.Errorf("pause=%v: unexpected control result %d", c.pausa, control)
		}
		if movido > c.max {
			t.Errorf("pause=%v: one round moved %d of %d bytes to the guest (at most %d), holding the lock %v",
				c.pausa, movido, lleno, c.max, d)
		}
		t.Logf("pause=%v: %d of %d bytes in one round, lock held %v", c.pausa, movido, lleno, d)
	}
}

// R1: con datos del host y la cola de rx sin búferes, vs_hilo no puede girar
// en vacío (poll listo, bombear sin sitio, otra vez): un núcleo del host por
// VM fuera del tope de CPU. Espera al aviso de la cola 0, y entonces el dato
// llega.
func TestVsockSinBuferesDeRxNoGira(t *testing.T) { vsockSinRxNoGira(t, false) }

// R5: lo mismo con un paquete de control en cola delante de los datos.
// bombear se paraba en él sin sitio en la cola 0 sin marcar sin_rx, y el poll
// seguía pidiendo POLLIN del socket con datos: un núcleo al 100 %.
func TestVsockSinBuferesDeRxConControlNoGira(t *testing.T) { vsockSinRxNoGira(t, true) }

func vsockSinRxNoGira(t *testing.T, ctl bool) {
	var ru0, ru1 syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru0)
	vueltas, llego, r := vsockSinRx(ctl)
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru1)
	cpu := time.Duration(ru1.Utime.Nano() + ru1.Stime.Nano() - ru0.Utime.Nano() - ru0.Stime.Nano())
	t.Logf("process CPU over the ~250 ms of the test: %v", cpu)
	if r != 0 {
		t.Fatalf("setting up: %d", r)
	}
	if vueltas > 5 {
		t.Fatalf("vs_hilo looped %d times in 200 ms with no rx buffers", vueltas)
	}
	if llego != 4096 {
		t.Fatalf("after publishing an rx buffer %d of 4096 bytes reached the guest", llego)
	}
	t.Logf("%d loops in 200 ms with no rx buffers", vueltas)
}

// F6: si Go cierra una conexión a la que el invitado no da crédito, el slot
// no puede quedarse ocupado para siempre: con 64 así no había exec ni health.
func TestVsockCierreDelHostSinCredito(t *testing.T) {
	switch r := vsockHostCierra(); r {
	case 0:
	case 1:
		t.Fatal("the host closed a connection with no credit and vs_hilo did not notice")
	case 2:
		t.Fatal("with every slot taken, a closed connection with no credit did not give up its slot")
	default:
		t.Fatalf("setting up: %d", r)
	}
}
