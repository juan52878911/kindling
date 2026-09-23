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
