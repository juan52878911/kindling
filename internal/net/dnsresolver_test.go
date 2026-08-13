package net

import (
	"encoding/binary"
	"testing"
)

// Estos tests ejercen SOLO el parseo DNS a mano y la lógica de allowlist: son
// funciones puras, sin root ni iptables, así que corren en cualquier plataforma.

// buildQuery arma una consulta DNS mínima (una pregunta) para un nombre y tipo.
func buildQuery(id uint16, name string, qtype uint16) []byte {
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[0:2], id)
	binary.BigEndian.PutUint16(msg[2:4], 0x0100) // RD
	binary.BigEndian.PutUint16(msg[4:6], 1)      // QDCOUNT
	msg = append(msg, encodeName(name)...)
	var tc [4]byte
	binary.BigEndian.PutUint16(tc[0:2], qtype)
	binary.BigEndian.PutUint16(tc[2:4], 1) // IN
	return append(msg, tc[:]...)
}

func encodeName(name string) []byte {
	var out []byte
	if name != "" {
		for len(name) > 0 {
			i := 0
			for i < len(name) && name[i] != '.' {
				i++
			}
			out = append(out, byte(i))
			out = append(out, name[:i]...)
			if i < len(name) {
				name = name[i+1:]
			} else {
				name = ""
			}
		}
	}
	return append(out, 0)
}

func TestParseQuestion(t *testing.T) {
	q := buildQuery(0x1234, "www.example.com", 1)
	name, qtype, ok := parseQuestion(q)
	if !ok || name != "www.example.com" || qtype != 1 {
		t.Fatalf("parseQuestion = %q, %d, %v", name, qtype, ok)
	}
	if _, _, ok := parseQuestion([]byte{0, 1, 2}); ok {
		t.Fatal("una cabecera truncada no debería parsear")
	}
	// QNAME que se sale del buffer: no debe parsear ni entrar en pánico.
	bad := buildQuery(1, "a.b", 1)
	bad = bad[:14] // corta en mitad del nombre
	if _, _, ok := parseQuestion(bad); ok {
		t.Fatal("un nombre truncado no debería parsear")
	}
}

func TestReadNameCompressionLoop(t *testing.T) {
	// Un puntero que apunta a sí mismo no debe colgar el resolver.
	msg := make([]byte, 14)
	binary.BigEndian.PutUint16(msg[4:6], 1)
	msg[12] = 0xC0
	msg[13] = 12 // apunta al propio offset 12
	if _, _, ok := readName(msg, 12); ok {
		t.Fatal("un bucle de punteros debería fallar, no colgarse")
	}
}

func TestIsAllowed(t *testing.T) {
	r := &dnsResolver{allowed: normalizeDomains([]string{"Example.com", "cdn.net.", " "})}
	cases := map[string]bool{
		"example.com":         true,
		"EXAMPLE.COM":         true,
		"www.example.com":     true,
		"a.b.cdn.net":         true,
		"cdn.net":             true,
		"notexample.com":      false, // no es sufijo con punto
		"example.com.evil.io": false,
		"evil.io":             false,
	}
	for name, want := range cases {
		if got := r.isAllowed(name); got != want {
			t.Errorf("isAllowed(%q) = %v, quería %v", name, got, want)
		}
	}
}

func TestExtractA(t *testing.T) {
	// Respuesta con una pregunta, un A público (1.2.3.4, ttl 300), un A privado
	// (192.168.0.1, debe descartarse) y un AAAA (debe ignorarse).
	msg := buildQuery(0x1, "example.com", 1)
	binary.BigEndian.PutUint16(msg[6:8], 3) // ANCOUNT = 3
	msg[2] |= 0x80                          // QR

	add := func(typ uint16, ttl uint32, rdata []byte) {
		msg = append(msg, 0xC0, 12) // puntero al nombre de la pregunta
		var h [10]byte
		binary.BigEndian.PutUint16(h[0:2], typ)
		binary.BigEndian.PutUint16(h[2:4], 1) // IN
		binary.BigEndian.PutUint32(h[4:8], ttl)
		binary.BigEndian.PutUint16(h[8:10], uint16(len(rdata)))
		msg = append(msg, h[:]...)
		msg = append(msg, rdata...)
	}
	add(1, 300, []byte{1, 2, 3, 4})    // A público
	add(1, 60, []byte{192, 168, 0, 1}) // A privado -> descartado
	add(28, 300, make([]byte, 16))     // AAAA -> ignorado (TODO)

	recs := extractA(msg)
	if len(recs) != 1 {
		t.Fatalf("esperaba 1 registro A público, obtuve %d: %+v", len(recs), recs)
	}
	if recs[0].ip != "1.2.3.4" || recs[0].ttl != 300 {
		t.Fatalf("registro inesperado: %+v", recs[0])
	}
}

func TestRespondError(t *testing.T) {
	q := buildQuery(0xABCD, "example.com", 1)
	resp := respondError(q, 5) // REFUSED
	if resp == nil {
		t.Fatal("respondError devolvió nil")
	}
	if binary.BigEndian.Uint16(resp[0:2]) != 0xABCD {
		t.Error("el ID de la respuesta no coincide con el de la consulta")
	}
	if resp[2]&0x80 == 0 {
		t.Error("QR debería estar puesto en la respuesta")
	}
	if resp[3]&0x0F != 5 {
		t.Errorf("RCODE = %d, quería 5 (REFUSED)", resp[3]&0x0F)
	}
	if binary.BigEndian.Uint16(resp[6:8]) != 0 {
		t.Error("ANCOUNT debería ser 0")
	}
}

func TestNormalizeDomains(t *testing.T) {
	got := normalizeDomains([]string{" Foo.COM ", "bar.net.", "", "  "})
	want := []string{"foo.com", "bar.net"}
	if len(got) != len(want) {
		t.Fatalf("normalizeDomains = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("normalizeDomains = %v, quería %v", got, want)
		}
	}
}
