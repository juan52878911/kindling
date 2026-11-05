package egress

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
)

// host.kindling.internal: la pasarela si la máquina tiene puentes al host, en
// todos los modos; si no, NXDOMAIN. Nunca sale al resolver de fuera ni
// siembra la allowlist.
func TestResolverHostBridge(t *testing.T) {
	p := NewPolicy()
	var forwarded int
	r := &Resolver{Policy: p, Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		forwarded++
		return answer(q, 30, "93.184.215.14"), nil
	}}
	rcode := func(b []byte) byte { return b[3] & 0x0F }
	for _, mode := range []Mode{None, Internet, Allowlist} {
		p.Set(mode, []string{"host.kindling.internal"})
		if resp := r.Process(context.Background(), BuildQuery("host.kindling.internal", 1), false); rcode(resp) != 3 {
			t.Fatalf("%s: without host ports the name must be NXDOMAIN, got %d", mode, rcode(resp))
		}
	}
	p.SetHostBridge(netip.MustParseAddr("172.16.0.1"))
	for _, mode := range []Mode{None, Internet, Allowlist} {
		p.Set(mode, nil)
		resp := r.Process(context.Background(), BuildQuery("Host.Kindling.Internal.", 1), false)
		if rcode(resp) != 0 || !bytes.Equal(resp[len(resp)-4:], []byte{172, 16, 0, 1}) {
			t.Fatalf("%s: host.kindling.internal must resolve to the gateway (rcode %d)", mode, rcode(resp))
		}
		if resp := r.Process(context.Background(), BuildQuery("host.kindling.internal", 28), false); rcode(resp) != 0 {
			t.Fatalf("%s: AAAA must be an empty NOERROR", mode)
		}
	}
	if forwarded != 0 || p.IPSet().Len() != 0 {
		t.Fatalf("the bridge name went upstream (%d) or seeded the set", forwarded)
	}
	p.SetHostBridge(netip.Addr{})
	if resp := r.Process(context.Background(), BuildQuery("host.kindling.internal", 1), false); rcode(resp) != 3 {
		t.Fatal("after clearing, the name must be NXDOMAIN")
	}
}
