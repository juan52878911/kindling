package egress

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{"10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1", "169.254.169.254",
		"127.0.0.1", "100.64.0.1", "0.0.0.0", "224.0.0.251", "255.255.255.255", "::1", "2606:4700::1"}
	for _, s := range blocked {
		if !IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s must be blocked", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.215.14", "172.32.0.1", "100.128.0.1", "::ffff:8.8.8.8"} {
		if IsBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s must not be blocked", s)
		}
	}
}

func TestPolicyModes(t *testing.T) {
	pub := netip.MustParseAddr("93.184.215.14")
	priv := netip.MustParseAddr("192.168.1.10")
	p := NewPolicy()
	if p.Mode() != None || p.AllowConn(pub) || p.AllowDNSName("example.com") {
		t.Fatal("the default policy must be none")
	}
	p.Set(Internet, nil)
	if !p.AllowConn(pub) || p.AllowConn(priv) || !p.AllowDNSName("anything.example") {
		t.Fatal("internet: public yes, private no, any DNS name")
	}
	p.Set(Allowlist, []string{" Example.COM. ", ""})
	if p.AllowConn(pub) {
		t.Fatal("allowlist: an IP not seeded must be refused")
	}
	p.IPSet().Add(pub, 0)
	if !p.AllowConn(pub) {
		t.Fatal("allowlist: a seeded IP must pass")
	}
	for name, want := range map[string]bool{
		"example.com": true, "www.example.com.": true, "EXAMPLE.com": true,
		"badexample.com": false, "example.com.evil.net": false, "google.com": false,
	} {
		if got := p.AllowDNSName(name); got != want {
			t.Errorf("AllowDNSName(%q) = %v, want %v", name, got, want)
		}
	}
	// Cambiar de política vacía el conjunto: nada de la lista anterior queda abierto.
	p.Set(Allowlist, []string{"other.org"})
	if p.AllowConn(pub) {
		t.Fatal("a new policy must start with an empty set")
	}
	if _, err := ParseMode("open"); err == nil {
		t.Fatal("ParseMode accepted an unknown mode")
	}
	if m, _ := ParseMode(""); m != None {
		t.Fatal("empty mode must be none")
	}
}

func TestIPSetTTL(t *testing.T) {
	now := time.Unix(1000, 0)
	s := NewIPSet()
	s.now = func() time.Time { return now }
	a := netip.MustParseAddr("1.2.3.4")
	b := netip.MustParseAddr("5.6.7.8")
	s.Add(a, 5*time.Second) // se sube al suelo de MinTTL
	s.Add(b, 0)             // permanente
	s.Add(netip.MustParseAddr("10.0.0.1"), 0)
	if s.Len() != 2 {
		t.Fatalf("blocked IPs must never enter the set (len %d)", s.Len())
	}
	now = now.Add(MinTTL - time.Second)
	if !s.Contains(a) {
		t.Fatal("the TTL floor was not applied")
	}
	// Una temporal no degrada a una permanente.
	s.Add(b, time.Second)
	now = now.Add(2 * time.Second)
	if s.Contains(a) {
		t.Fatal("the entry must have expired")
	}
	if !s.Contains(b) {
		t.Fatal("a permanent entry expired")
	}
}

// answer fabrica una respuesta con registros A para name.
func answer(query []byte, ttl uint32, ips ...string) []byte {
	resp := append([]byte(nil), query...)
	resp[2] |= 0x80
	binary.BigEndian.PutUint16(resp[6:8], uint16(len(ips)))
	for _, s := range ips {
		ip := netip.MustParseAddr(s).As4()
		// Nombre comprimido: puntero al de la pregunta (offset 12).
		resp = append(resp, 0xC0, 12, 0, 1, 0, 1)
		resp = binary.BigEndian.AppendUint32(resp, ttl)
		resp = append(resp, 0, 4)
		resp = append(resp, ip[:]...)
	}
	return resp
}

func TestResolverProcess(t *testing.T) {
	p := NewPolicy()
	var forwarded int
	r := &Resolver{Policy: p, Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		forwarded++
		return answer(q, 30, "93.184.215.14", "192.168.0.9"), nil
	}}
	q := BuildQuery("www.example.com", 1)
	rcode := func(b []byte) byte { return b[3] & 0x0F }

	if resp := r.Process(context.Background(), q, false); rcode(resp) != 5 || forwarded != 0 {
		t.Fatalf("none: rcode %d, forwarded %d; want REFUSED and nothing sent", rcode(resp), forwarded)
	}
	p.Set(Allowlist, []string{"example.com"})
	if resp := r.Process(context.Background(), BuildQuery("google.com", 1), false); rcode(resp) != 5 || forwarded != 0 {
		t.Fatal("allowlist: an unlisted name must be refused without forwarding")
	}
	resp := r.Process(context.Background(), q, false)
	if rcode(resp) != 0 || forwarded != 1 {
		t.Fatal("allowlist: a listed name must be forwarded")
	}
	if !p.AllowConn(netip.MustParseAddr("93.184.215.14")) {
		t.Fatal("allowlist: the resolved IP was not seeded")
	}
	if p.IPSet().Contains(netip.MustParseAddr("192.168.0.9")) {
		t.Fatal("a private IP from the upstream was seeded")
	}
	if resp := r.Process(context.Background(), []byte{1, 2, 3}, false); resp != nil {
		t.Fatal("a message without header has no possible answer")
	}
	if resp := r.Process(context.Background(), q[:14], false); rcode(resp) != 1 {
		t.Fatal("a truncated question must get FORMERR")
	}
	r.Exchange = func(context.Context, []byte, bool) ([]byte, error) { return nil, errors.New("down") }
	if resp := r.Process(context.Background(), q, false); rcode(resp) != 2 {
		t.Fatal("an upstream failure must get SERVFAIL")
	}
}

func TestSeedStatic(t *testing.T) {
	p := NewPolicy()
	p.Set(Allowlist, []string{"example.com"})
	r := &Resolver{Policy: p, Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		return answer(q, 1, "93.184.215.14"), nil
	}}
	r.SeedStatic(context.Background())
	if !p.AllowConn(netip.MustParseAddr("93.184.215.14")) {
		t.Fatal("static seeding did not open the domain's IP")
	}
}

func TestParseQuestionAndTCPFraming(t *testing.T) {
	q := BuildQuery("a.b.example.com.", 28)
	name, qtype, ok := ParseQuestion(q)
	if !ok || name != "a.b.example.com" || qtype != 28 {
		t.Fatalf("ParseQuestion = %q %d %v", name, qtype, ok)
	}
	// Un bucle de punteros no debe colgar el parseo.
	loop := append([]byte(nil), q[:12]...)
	loop = append(loop, 0xC0, 12)
	if _, _, ok := ParseQuestion(loop); ok {
		t.Fatal("a pointer loop was accepted")
	}
	var buf bytes.Buffer
	if err := WriteTCPMsg(&buf, q); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTCPMsg(&buf)
	if err != nil || !bytes.Equal(got, q) {
		t.Fatalf("TCP framing round trip: %v", err)
	}
	if BuildQuery("", 1) != nil || BuildQuery(string(make([]byte, 64))+".com", 1) != nil {
		t.Fatal("invalid names must not build a query")
	}
}

// Un dominio con credencial se contesta con la IP del proxy sin preguntar al
// upstream ni sembrar nada, solo en allowlist y solo con el nombre exacto.
func TestResolverCredHost(t *testing.T) {
	gw := netip.MustParseAddr("172.16.0.1")
	p := NewPolicy()
	var forwarded int
	r := &Resolver{Policy: p, Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		forwarded++
		return answer(q, 30, "93.184.215.14"), nil
	}}
	p.SetCredHosts([]string{"API.Example.com."}, gw)
	rcode := func(b []byte) byte { return b[3] & 0x0F }

	// En none no hay proxy que valga: ni desvío ni reenvío.
	if resp := r.Process(context.Background(), BuildQuery("api.example.com", 1), false); rcode(resp) != 5 || forwarded != 0 {
		t.Fatalf("none: rcode %d, forwarded %d; the credential host must not be answered", rcode(resp), forwarded)
	}
	// En internet tampoco: el proxy solo existe para allowlist.
	p.Set(Internet, nil)
	if _, ok := p.CredHost("api.example.com"); ok {
		t.Fatal("internet: a credential host must not be diverted")
	}

	p.Set(Allowlist, []string{"other.org"}) // el dominio con credencial no está en la lista
	resp := r.Process(context.Background(), BuildQuery("api.example.com", 1), false)
	if rcode(resp) != 0 || forwarded != 0 {
		t.Fatalf("A: rcode %d, forwarded %d; want NOERROR without forwarding", rcode(resp), forwarded)
	}
	if n := binary.BigEndian.Uint16(resp[6:8]); n != 1 || !bytes.Equal(resp[len(resp)-4:], []byte{172, 16, 0, 1}) {
		t.Fatalf("A: %d answers, rdata %v; want one record with the gateway", n, resp[len(resp)-4:])
	}
	if ttl := binary.BigEndian.Uint32(resp[len(resp)-10 : len(resp)-6]); ttl != CredTTL {
		t.Fatalf("TTL %d, want %d", ttl, CredTTL)
	}
	if p.IPSet().Len() != 0 {
		t.Fatal("a diverted name must not seed anything")
	}
	// AAAA: NOERROR vacío, para que el cliente use IPv4.
	resp = r.Process(context.Background(), BuildQuery("api.example.com", 28), false)
	if rcode(resp) != 0 || binary.BigEndian.Uint16(resp[6:8]) != 0 || forwarded != 0 {
		t.Fatal("AAAA of a credential host must be an empty NOERROR without forwarding")
	}
	// Un subdominio no hereda la credencial: sigue la lista (y aquí no está).
	if resp := r.Process(context.Background(), BuildQuery("x.api.example.com", 1), false); rcode(resp) != 5 {
		t.Fatal("a subdomain of a credential host must not be diverted")
	}
	// Quitar las credenciales devuelve el nombre a la política normal.
	p.SetCredHosts(nil, gw)
	if resp := r.Process(context.Background(), BuildQuery("api.example.com", 1), false); rcode(resp) != 5 {
		t.Fatal("after clearing, the name must follow the list again")
	}
}

// El lookup del proxy nunca devuelve una IP a la que la clave no deba viajar.
func TestPublicIPv4(t *testing.T) {
	r := &Resolver{Policy: NewPolicy(), Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		return answer(q, 30, "93.184.215.14", "0.0.0.0", "127.0.0.1", "192.168.1.1", "169.254.169.254"), nil
	}}
	got := r.PublicIPv4(context.Background(), "api.example.com")
	if len(got) != 1 || got[0] != "93.184.215.14" {
		t.Fatalf("PublicIPv4 = %v, want only the public IP", got)
	}
	r.Exchange = func(context.Context, []byte, bool) ([]byte, error) { return nil, errors.New("down") }
	if got := r.PublicIPv4(context.Background(), "api.example.com"); len(got) != 0 {
		t.Fatalf("an upstream failure must give nothing, got %v", got)
	}
}

// Los nombres de grafo: los de las aristas del nodo, con la pasarela, en
// TODOS los modos; cualquier otro *.graph es NXDOMAIN y no sale nunca.
func TestResolverGraphHosts(t *testing.T) {
	gw := netip.MustParseAddr("172.16.0.1")
	p := NewPolicy()
	var forwarded int
	r := &Resolver{Policy: p, Exchange: func(_ context.Context, q []byte, _ bool) ([]byte, error) {
		forwarded++
		return answer(q, 30, "93.184.215.14"), nil
	}}
	rcode := func(b []byte) byte { return b[3] & 0x0F }
	p.SetGraphHosts([]string{"API.graph.", "db.graph", "no-es-de-grafo.com"}, gw)

	for _, mode := range []Mode{None, Internet, Allowlist} {
		p.Set(mode, []string{"example.com"})
		resp := r.Process(context.Background(), BuildQuery("api.graph", 1), false)
		if rcode(resp) != 0 || !bytes.Equal(resp[len(resp)-4:], []byte{172, 16, 0, 1}) {
			t.Fatalf("%s: api.graph must resolve to the gateway (rcode %d)", mode, rcode(resp))
		}
		if resp := r.Process(context.Background(), BuildQuery("db.graph", 28), false); rcode(resp) != 0 || binary.BigEndian.Uint16(resp[6:8]) != 0 {
			t.Fatalf("%s: AAAA of a graph host must be an empty NOERROR", mode)
		}
		if resp := r.Process(context.Background(), BuildQuery("cache.graph", 1), false); rcode(resp) != 3 {
			t.Fatalf("%s: a graph name without an edge must be NXDOMAIN, got %d", mode, rcode(resp))
		}
		if resp := r.Process(context.Background(), BuildQuery("x.api.graph", 1), false); rcode(resp) != 3 {
			t.Fatalf("%s: a subdomain of a graph host must be NXDOMAIN", mode)
		}
	}
	if forwarded != 0 || p.IPSet().Len() != 0 {
		t.Fatalf("graph names went upstream (%d) or seeded the set", forwarded)
	}
	if _, esGrafo, _ := p.GraphHost("no-es-de-grafo.com"); esGrafo {
		t.Fatal("a name outside .graph is not a graph name")
	}
	p.SetGraphHosts(nil, gw)
	if resp := r.Process(context.Background(), BuildQuery("api.graph", 1), false); rcode(resp) != 3 {
		t.Fatal("after clearing, api.graph must be NXDOMAIN")
	}
}
