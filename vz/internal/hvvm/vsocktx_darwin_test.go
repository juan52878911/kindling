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
