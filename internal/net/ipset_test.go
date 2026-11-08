package net

import (
	"encoding/binary"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

// El resolver siembra cada IP nueva antes de contestar al invitado. Por
// netlink no lanza ningún proceso (antes, `ip netns exec` + `ipset` por IP);
// el comando queda solo de respaldo.
func TestSeedVaPorNetlinkSinProcesos(t *testing.T) {
	oldE, oldN := ejecutar, anadirNetlink
	t.Cleanup(func() { ejecutar, anadirNetlink = oldE, oldN })
	var procesos, mensajes int32
	ejecutar = func(args ...string) error { atomic.AddInt32(&procesos, 1); return nil }
	var ns, set, ip string
	var to int
	anadirNetlink = func(n, s, i string, timeout int) error {
		atomic.AddInt32(&mensajes, 1)
		ns, set, ip, to = n, s, i, timeout
		return nil
	}
	r := newTestResolver("")
	r.seed("93.184.216.34", 300)
	if procesos != 0 || mensajes != 1 {
		t.Fatalf("procesos = %d, mensajes netlink = %d; quería 0 y 1", procesos, mensajes)
	}
	if ns != r.ns || set != r.set || ip != "93.184.216.34" || to != 300 {
		t.Fatalf("netlink recibió %s %s %s %d", ns, set, ip, to)
	}
}

// Sin netlink (otro sistema, un netns que no se abre), al comando de siempre.
func TestAnadirIPSetCaeAlComando(t *testing.T) {
	oldE, oldN := ejecutar, anadirNetlink
	t.Cleanup(func() { ejecutar, anadirNetlink = oldE, oldN })
	anadirNetlink = func(string, string, string, int) error { return errors.ErrUnsupported }
	var got []string
	ejecutar = func(args ...string) error { got = args; return nil }
	if err := anadirIPSet("kl-abc", "klaw-abc", "1.2.3.4", 60); err != nil {
		t.Fatal(err)
	}
	if want := "ip netns exec kl-abc ipset add klaw-abc 1.2.3.4 timeout 60 -exist"; strings.Join(got, " ") != want {
		t.Fatalf("comando = %q, quería %q", strings.Join(got, " "), want)
	}
}

// La forma del mensaje: nlmsghdr con el subsistema de ipset, NLM_F_ACK sin
// NLM_F_EXCL (es el -exist) y la IP y el timeout en orden de red. Que el
// kernel lo acepta lo prueba TestAnadirIPSetNetlinkKernel (Linux, root).
func TestMensajeAddIPSet(t *testing.T) {
	m := mensajeAddIPSet(7, "klaw-x", [4]byte{93, 184, 216, 34}, 300)
	le := binary.NativeEndian
	if int(le.Uint32(m)) != len(m) || len(m)%4 != 0 {
		t.Fatalf("longitud %d en cabecera, %d real", le.Uint32(m), len(m))
	}
	if typ := le.Uint16(m[4:]); typ != 6<<8|9 {
		t.Fatalf("tipo %#x, quería IPSET_CMD_ADD de NFNL_SUBSYS_IPSET", typ)
	}
	if fl := le.Uint16(m[6:]); fl != 0x5 {
		t.Fatalf("flags %#x, quería REQUEST|ACK", fl)
	}
	if le.Uint32(m[8:]) != 7 {
		t.Fatal("seq")
	}
	if binary.BigEndian.Uint32(m[len(m)-4:]) != 300 || string(m[len(m)-12:len(m)-8]) != string([]byte{93, 184, 216, 34}) {
		t.Fatalf("cola del mensaje %v", m[len(m)-16:])
	}
}
