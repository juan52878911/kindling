package credproxy

import (
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"strings"
	"testing"
)

// El vector de la RFC 7677 §3: con su nonce, el client-final y la
// verificación del server-final tienen que salir byte a byte.
func TestScramVectorRFC7677(t *testing.T) {
	s := &scramCliente{clave: "pencil", usuario: "user", nonce: "rOprNGfwEbeRWgbNEkqO", gs2: "n,,"}
	if got := string(s.primero()); got != "n,,n=user,r=rOprNGfwEbeRWgbNEkqO" {
		t.Fatalf("client-first = %q", got)
	}
	final, err := s.final([]byte("r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"))
	if err != nil {
		t.Fatal(err)
	}
	want := "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	if string(final) != want {
		t.Fatalf("client-final = %q\n quería %q", final, want)
	}
	if err := s.verificar([]byte("v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=")); err != nil {
		t.Fatalf("firma del servidor del vector: %v", err)
	}
	if err := s.verificar([]byte("v=7rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=")); err == nil {
		t.Fatal("aceptó una firma del servidor cambiada")
	}
}

// Lo que se exige del server-first: nonce que alarga el nuestro, sal,
// iteraciones en rango, sin extensiones obligatorias; y del server-final, que
// no sea un error ni llegue antes de tiempo.
func TestScramRechazaServidoresRaros(t *testing.T) {
	nuevo := func() *scramCliente {
		s := &scramCliente{clave: "pencil", nonce: "abc", gs2: "n,,"}
		s.primero()
		return s
	}
	for nombre, sf := range map[string]string{
		"nonce ajeno":        "r=xyz123,s=c2FsdA==,i=4096",
		"nonce igual":        "r=abc,s=c2FsdA==,i=4096",
		"nonce con coma":     "r=abc1,,s=c2FsdA==,i=4096",
		"sin sal":            "r=abc123,s=,i=4096",
		"sal no base64":      "r=abc123,s=***,i=4096",
		"pocas iteraciones":  "r=abc123,s=c2FsdA==,i=4095",
		"muchas iteraciones": "r=abc123,s=c2FsdA==,i=1000001",
		"iteraciones texto":  "r=abc123,s=c2FsdA==,i=mil",
		"extensión m":        "m=x,r=abc123,s=c2FsdA==,i=4096",
		"repetido":           "r=abc123,r=abc123,s=c2FsdA==,i=4096",
		"mal formado":        "r=abc123,s",
		"enorme":             "r=abc" + strings.Repeat("x", scramMaxMsg) + ",s=c2FsdA==,i=4096",
	} {
		if _, err := nuevo().final([]byte(sf)); err == nil {
			t.Errorf("%s: aceptó %q", nombre, sf)
		}
	}
	if _, err := nuevo().final([]byte("r=abc123,s=c2FsdA==,i=4096")); err != nil {
		t.Fatalf("un server-first correcto: %v", err)
	}
	s := nuevo()
	if err := s.verificar([]byte("v=AAAA")); err == nil {
		t.Error("verificó antes del client-final")
	}
	if _, err := s.final([]byte("r=abc123,s=c2FsdA==,i=4096")); err != nil {
		t.Fatal(err)
	}
	if err := s.verificar([]byte("e=invalid-proof")); err == nil {
		t.Error("aceptó un e= del servidor")
	}
}

// La cabecera GS2: -PLUS con sus datos, "y" si podríamos pero el servidor no
// lo ofrece, "n" si no hay datos de canal.
func TestScramCabeceraGS2(t *testing.T) {
	cb := []byte{1, 2, 3}
	for _, c := range []struct {
		cb   []byte
		plus bool
		want string
	}{{cb, true, "p=tls-server-end-point,,"}, {cb, false, "y,,"}, {nil, false, "n,,"}} {
		s, err := nuevoScram("x", c.cb, c.plus)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(s.primero()), c.want+"n=,r=") {
			t.Errorf("cb=%v plus=%v: %q", c.cb, c.plus, s.primero())
		}
	}
}

// tls-server-end-point: SHA-256 para SHA-1/SHA-256 (y MD5), el hash de la firma
// para SHA-384/512, y nada para Ed25519.
func TestTLSServerEndPoint(t *testing.T) {
	raw := []byte("certificado")
	s256 := sha256.Sum256(raw)
	s384 := sha512.Sum384(raw)
	s512 := sha512.Sum512(raw)
	for alg, want := range map[x509.SignatureAlgorithm][]byte{
		x509.SHA1WithRSA:     s256[:],
		x509.ECDSAWithSHA256: s256[:],
		x509.SHA384WithRSA:   s384[:],
		x509.ECDSAWithSHA512: s512[:],
		x509.PureEd25519:     nil,
	} {
		got := tlsServerEndPoint(&x509.Certificate{Raw: raw, SignatureAlgorithm: alg})
		if string(got) != string(want) {
			t.Errorf("%v: %x, quería %x", alg, got, want)
		}
	}
}
