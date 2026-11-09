package net

import (
	"encoding/binary"
	"errors"
	"fmt"
	stdnet "net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// respuestaConAs fabrica una respuesta a query con n registros A distintos.
func respuestaConAs(query []byte, n int, ttl uint32) []byte {
	name, qtype, _ := parseQuestion(query)
	msg := buildQuery(binary.BigEndian.Uint16(query[0:2]), name, qtype)
	msg[2] |= 0x80
	binary.BigEndian.PutUint16(msg[6:8], uint16(n))
	for i := 0; i < n; i++ {
		var h [10]byte
		binary.BigEndian.PutUint16(h[0:2], 1)
		binary.BigEndian.PutUint16(h[2:4], 1)
		binary.BigEndian.PutUint32(h[4:8], ttl)
		binary.BigEndian.PutUint16(h[8:10], 4)
		msg = append(msg, 0xC0, 12)
		msg = append(msg, h[:]...)
		msg = append(msg, stdnet.ParseIP(fmt.Sprintf("93.184.%d.%d", i/250, i%250+1)).To4()...)
	}
	return msg
}

// sembradoFalso cambia anadirNetlink por uno que apunta IP y timeout.
func sembradoFalso(t *testing.T) (*sync.Mutex, map[string]int) {
	t.Helper()
	oldE, oldN := ejecutar, anadirNetlink
	t.Cleanup(func() { ejecutar, anadirNetlink = oldE, oldN })
	var mu sync.Mutex
	vistas := map[string]int{}
	ejecutar = func(args ...string) error { return nil }
	anadirNetlink = func(_, _, ip string, timeout int) error {
		mu.Lock()
		vistas[ip] = timeout
		mu.Unlock()
		return nil
	}
	return &mu, vistas
}

// Una respuesta con 200 A solo mete dnsMaxASembrar en el ipset, aunque el
// invitado reciba la respuesta entera.
func TestSembradoAcotadoPorRespuesta(t *testing.T) {
	mu, vistas := sembradoFalso(t)
	addr, closeFn := fakeUpstream(t, func(q []byte) []byte { return respuestaConAs(q, 200, 300) })
	defer closeFn()
	r := newTestResolver(addr)
	resp := r.process(buildQuery(7, "example.com", 1), false)
	if got := len(extractA(resp)); got != 200 {
		t.Fatalf("el invitado recibe %d A, quería los 200", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(vistas) != dnsMaxASembrar {
		t.Fatalf("sembró %d IP de una respuesta, quería %d", len(vistas), dnsMaxASembrar)
	}
}

// Un TTL de un día se siembra con el techo de una hora; el suelo sigue.
func TestSembradoConTechoDeTTL(t *testing.T) {
	mu, vistas := sembradoFalso(t)
	r := newTestResolver("")
	r.seed("93.184.216.34", 86400)
	r.seed("93.184.216.35", 0xFFFFFFFF)
	r.seed("93.184.216.36", 5)
	mu.Lock()
	defer mu.Unlock()
	if vistas["93.184.216.34"] != dnsMaxTTL || vistas["93.184.216.35"] != dnsMaxTTL || vistas["93.184.216.36"] != dnsMinTTL {
		t.Fatalf("timeouts = %v, quería techo %d y suelo %d", vistas, dnsMaxTTL, dnsMinTTL)
	}
}

// El set del allowlist se crea con maxelem.
func TestSetAllowlistConMaxelem(t *testing.T) {
	n := Plan(1, "abcdef0123")
	aplicadas := reglasAplicadas(t, n, EgressAllowlist)
	if len(aplicadas) == 0 {
		t.Fatal("no aplicó nada")
	}
	want := fmt.Sprintf("ipset create %s hash:ip timeout 0 maxelem %d -exist", n.setName(), ipsetMaxElem)
	if got := strings.Join(aplicadas[0], " "); got != want {
		t.Fatalf("create = %q, quería %q", got, want)
	}
}

// Si el kernel contesta por netlink (el set está lleno), no se repite con un
// fork del comando, que daría el mismo error; si netlink no está, sí.
func TestAnadirIPSetNoCaeAlComandoSiElKernelRechaza(t *testing.T) {
	oldE, oldN := ejecutar, anadirNetlink
	t.Cleanup(func() { ejecutar, anadirNetlink = oldE, oldN })
	var procesos int32
	ejecutar = func(args ...string) error { atomic.AddInt32(&procesos, 1); return nil }
	anadirNetlink = func(_, set, _ string, _ int) error {
		return &rechazoIPSet{set: set, errno: 4352} // IPSET_ERR_HASH_FULL
	}
	err := anadirIPSet("kl-abc", "klaw-abc", "1.2.3.4", 60)
	var rechazo *rechazoIPSet
	if !errors.As(err, &rechazo) {
		t.Fatalf("err = %v, quería el rechazo del kernel", err)
	}
	if procesos != 0 {
		t.Fatalf("cayó al comando %d veces con el set lleno", procesos)
	}
	anadirNetlink = func(string, string, string, int) error { return errors.New("socket: protocol not supported") }
	if err := anadirIPSet("kl-abc", "klaw-abc", "1.2.3.4", 60); err != nil || procesos != 1 {
		t.Fatalf("sin netlink: err = %v, procesos = %d; quería caer al comando", err, procesos)
	}
}
