//go:build darwin && arm64

package hvvm

import "testing"

// Con VIRTIO_NET_F_MRG_RXBUF una trama ocupa los búferes pequeños de rx que
// haga falta (num_buffers), y el invitado no tiene que reservarlos del tamaño
// de la MTU. Sin él, la trama que no cabe en un búfer se tira, como siempre.
func TestRedMrgRxbuf(t *testing.T) {
	if r := redMrg(true); r != 0 {
		t.Fatalf("con MRG_RXBUF: paso %d", r)
	}
	if r := redMrg(false); r != 0 {
		t.Fatalf("sin MRG_RXBUF: paso %d", r)
	}
}
