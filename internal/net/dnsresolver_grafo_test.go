package net

import (
	"encoding/binary"
	stdnet "net"
	"testing"
	"time"
)

// rcodeDe devuelve el RCODE de una respuesta.
func rcodeDe(t *testing.T, resp []byte) byte {
	t.Helper()
	if len(resp) < 12 {
		t.Fatalf("respuesta corta: %v", resp)
	}
	return resp[3] & 0x0F
}

// ipDeRespuesta es la IP del único A de una respuesta de respondA.
func ipDeRespuesta(t *testing.T, resp []byte) string {
	t.Helper()
	if binary.BigEndian.Uint16(resp[6:8]) != 1 {
		t.Fatalf("sin respuesta A: %v", resp)
	}
	return stdnet.IP(resp[len(resp)-4:]).String()
}

func resolverDeGrafo(modo modoResolver) *dnsResolver {
	r := &dnsResolver{
		modo:    modo,
		allowed: normalizeDomains([]string{"example.org"}),
		sem:     make(chan struct{}, dnsMaxInFlight),
		limiter: newTokenBucket(dnsRate, dnsBurst),
		seeded:  map[string]time.Time{},
	}
	host := stdnet.ParseIP("172.30.0.1")
	r.setGraphHosts([]string{"api.graph"}, host)
	r.setCredHosts([]string{"db.graph"}, host)
	return r
}

// En los tres modos, un nodo solo resuelve los *.graph de SUS aristas, a la
// IP del host en su veth; el resto de *.graph es NXDOMAIN y no sale a ningún
// sitio. En egress none, además, todo lo demás es NXDOMAIN.
func TestResolverSoloNombresDeSusAristas(t *testing.T) {
	for _, modo := range []modoResolver{modoAllowlist, modoSoloGrafo, modoLibre} {
		r := resolverDeGrafo(modo)
		for _, name := range []string{"api.graph", "API.graph.", "db.graph"} {
			resp := r.process(buildQuery(7, name, 1), false)
			if rcodeDe(t, resp) != 0 || ipDeRespuesta(t, resp) != "172.30.0.1" {
				t.Fatalf("modo %d: %s no resolvió a la IP del host: %v", modo, name, resp)
			}
		}
		for _, name := range []string{"web.graph", "otro.graph", "x.api.graph"} {
			if rc := rcodeDe(t, r.process(buildQuery(8, name, 1), false)); rc != 3 {
				t.Fatalf("modo %d: %s dio rcode %d, quería NXDOMAIN", modo, name, rc)
			}
		}
		// AAAA de un nombre de arista: vacía, para que el cliente use IPv4.
		resp := r.process(buildQuery(9, "api.graph", 28), false)
		if rcodeDe(t, resp) != 0 || binary.BigEndian.Uint16(resp[6:8]) != 0 {
			t.Fatalf("modo %d: AAAA de api.graph = %v", modo, resp)
		}
	}
	r := resolverDeGrafo(modoSoloGrafo)
	for _, name := range []string{"example.org", "evil.com", "graph"} {
		if rc := rcodeDe(t, r.process(buildQuery(10, name, 1), false)); rc != 3 {
			t.Fatalf("egress none: %s dio rcode %d, quería NXDOMAIN", name, rc)
		}
	}
	// allowlist: lo no listado sigue siendo REFUSED, como siempre.
	r = resolverDeGrafo(modoAllowlist)
	if rc := rcodeDe(t, r.process(buildQuery(11, "evil.com", 1), false)); rc != 5 {
		t.Fatalf("allowlist: evil.com dio rcode %d, quería REFUSED", rc)
	}
}
