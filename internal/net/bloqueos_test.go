package net

import (
	"fmt"
	"strings"
	"testing"
)

// En egress none el resolver contesta NXDOMAIN y apunta el nombre; una A y una
// AAAA del mismo nombre cuentan como un intento.
func TestResolverNoneAnotaNombres(t *testing.T) {
	r := newTestResolver("127.0.0.1:1")
	r.modo = modoSoloGrafo
	r.process(buildQuery(1, "Example.com", 1), false)
	r.process(buildQuery(2, "example.com", 28), false)
	r.process(buildQuery(3, "example.com", 1), false)
	r.process(buildQuery(4, "only6.test", 28), false)
	r.process(buildQuery(5, "1.0.0.127.in-addr.arpa", 12), false)

	got := juntarDestinos(r.bloq.destinos())
	want := []Destino{{"example.com", "dns", 2}, {"only6.test", "dns", 1}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// En allowlist solo se apunta lo que se rechaza, no lo permitido.
func TestResolverAllowlistAnotaRechazados(t *testing.T) {
	r := newTestResolver("127.0.0.1:1")
	r.process(buildQuery(1, "evil.io", 1), false)
	got := r.bloq.destinos()
	if len(got) != 1 || got[0].Dest != "evil.io" {
		t.Fatalf("got %v", got)
	}
}

// Un invitado que consulte nombres sin fin no hace crecer el registro.
func TestRegistroBloqueosTope(t *testing.T) {
	var b registroBloqueos
	for i := 0; i < bloqueosMaxNombres*3; i++ {
		b.anotar(fmt.Sprintf("h%d.example", i), 1)
	}
	if n := len(b.destinos()); n != bloqueosMaxNombres {
		t.Fatalf("%d nombres, tope %d", n, bloqueosMaxNombres)
	}
	if n := len(juntarDestinos(b.destinos())); n != bloqueosMaxDestinos {
		t.Fatalf("%d destinos devueltos, tope %d", n, bloqueosMaxDestinos)
	}
}

func TestJuntarDestinosOrden(t *testing.T) {
	got := juntarDestinos(
		[]Destino{{"b.test", "dns", 2}, {"a.test", "dns", 2}},
		[]Destino{{"1.2.3.4:80", "ip", 5}},
	)
	if got[0].Dest != "1.2.3.4:80" || got[1].Dest != "a.test" || got[2].Dest != "b.test" {
		t.Fatalf("orden: %v", got)
	}
}

func TestFormatoDestino(t *testing.T) {
	if s := formatoDestino("1.2.3.4", "tcp", 443); s != "1.2.3.4:443" {
		t.Fatal(s)
	}
	if s := formatoDestino("1.2.3.4", "udp", 123); s != "1.2.3.4:123/udp" {
		t.Fatal(s)
	}
}

func consultaCruda(labels ...string) []byte {
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range labels {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	return append(q, 0, 0, 1, 0, 1)
}

// B12: un nombre con bytes fuera de [a-z0-9._-] no se guarda: llegaría a la
// terminal del operador y a la orden -allow sugerida.
func TestAnotarDescartaNombresConBytesRaros(t *testing.T) {
	r := &dnsResolver{modo: modoSoloGrafo}
	r.process(consultaCruda("\x1b[2J\x1b[Hok", "a.com -egress internet", "x"), false)
	r.process(consultaCruda("ok", "example", "com"), false)
	ds := r.bloq.destinos()
	if len(ds) != 1 || ds[0].Dest != "ok.example.com" {
		t.Fatalf("destinos: %v", ds)
	}
	if strings.ContainsAny(ds[0].Dest, "\x1b ") {
		t.Fatalf("sin sanear: %q", ds[0].Dest)
	}
}
