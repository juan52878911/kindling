package guest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"syscall"
	"testing"
)

func TestParseRed(t *testing.T) {
	c, err := parseRed("172.16.0.2/30,172.16.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if c.IP != [4]byte{172, 16, 0, 2} || c.Prefijo != 30 || c.GW != [4]byte{172, 16, 0, 1} {
		t.Fatalf("parseRed = %+v", c)
	}
	for _, mal := range []string{"", "172.16.0.2/30", "172.16.0.2,172.16.0.1", "172.16.0.2/33,172.16.0.1",
		"x/30,172.16.0.1", "172.16.0.2/30,::1", "172.16.0.2/30,172.16.0.1,1"} {
		if _, err := parseRed(mal); err == nil {
			t.Errorf("parseRed(%q) debería fallar", mal)
		}
	}
}

func TestKlingNetEsConocido(t *testing.T) {
	p := parseBootParams("console=ttyS0 kling.net=172.16.0.2/30,172.16.0.1")
	if p.values[netBootParam] != "172.16.0.2/30,172.16.0.1" || len(p.unknown) != 0 {
		t.Fatalf("parseBootParams = %+v", p)
	}
}

func TestMensajesRed(t *testing.T) {
	le := binary.NativeEndian
	b := mensajeEnlaceArriba(7, 3)
	if len(b) != 32 || le.Uint32(b[0:]) != 32 || le.Uint16(b[4:]) != 16 || le.Uint32(b[8:]) != 7 ||
		int32(le.Uint32(b[20:])) != 3 || le.Uint32(b[24:]) != 1 || le.Uint32(b[28:]) != 1 {
		t.Errorf("RTM_NEWLINK: % x", b)
	}
	b = mensajeDireccion(8, 3, [4]byte{172, 16, 0, 2}, 30)
	if len(b) != 16+8+3*8 || le.Uint16(b[4:]) != 20 || b[16] != afInet || b[17] != 30 || le.Uint32(b[20:]) != 3 {
		t.Errorf("RTM_NEWADDR: % x", b)
	}
	if !bytes.Contains(b, []byte{172, 16, 0, 2}) || !bytes.Contains(b, []byte{172, 16, 0, 3}) {
		t.Errorf("RTM_NEWADDR sin local/broadcast: % x", b)
	}
	b = mensajeRutaDefecto(9, [4]byte{172, 16, 0, 1}, 3)
	if len(b) != 16+12+16 || le.Uint16(b[4:]) != rtmNewRoute || b[16] != afInet || b[17] != 0 || b[22] != 0 {
		t.Errorf("ruta por defecto: % x", b)
	}
	if !bytes.Contains(b, []byte{172, 16, 0, 1}) {
		t.Errorf("ruta por defecto sin pasarela: % x", b)
	}
}

func TestConfigurarRedOrden(t *testing.T) {
	var tipos []uint16
	var devs []string
	em := func(dev string, msg []byte) error {
		devs = append(devs, dev)
		tipos = append(tipos, binary.NativeEndian.Uint16(msg[4:]))
		return nil
	}
	if err := configurarRed("172.16.0.2/30,172.16.0.1", em, func(string) (int32, error) { return 2, nil }); err != nil {
		t.Fatal(err)
	}
	wantT := []uint16{16, 16, 20, rtmNewRoute}
	wantD := []string{"lo", "eth0", "eth0", "eth0"}
	for i := range wantT {
		if tipos[i] != wantT[i] || devs[i] != wantD[i] {
			t.Fatalf("orden: %v %v", devs, tipos)
		}
	}
	// EEXIST en dirección o ruta no es un fallo; otro error sí.
	n := 0
	em2 := func(string, []byte) error { n++; return syscall.EEXIST }
	if err := configurarRed("172.16.0.2/30,172.16.0.1", em2, func(string) (int32, error) { return 2, nil }); err != nil {
		t.Fatalf("EEXIST: %v", err)
	}
	if n != 4 {
		t.Fatalf("mensajes = %d", n)
	}
	boom := errors.New("boom")
	if err := configurarRed("172.16.0.2/30,172.16.0.1", func(string, []byte) error { return boom }, func(string) (int32, error) { return 2, nil }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
}
