package scram

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

// El ejemplo de RFC 7677, sección 3: usuario "user", contraseña "pencil".
const (
	rfcSalt        = "W22ZaJ0SNY7soEsUEjb6gQ=="
	rfcClientFirst = "n=user,r=rOprNGfwEbeRWgbNEkqO"
	rfcServerFirst = "r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096"
	rfcFinalNoProf = "c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	rfcProof       = "dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ="
	rfcServerSig   = "6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4="
)

func b64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestVectorRFC7677 comprueba el verificador contra el intercambio del RFC: un
// servidor que solo tuviera nuestro StoredKey y ServerKey tiene que aceptar la
// prueba del cliente del RFC y firmar exactamente lo mismo que el RFC.
func TestVectorRFC7677(t *testing.T) {
	v, err := Verifier("pencil", b64(t, rfcSalt), 4096)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`^SCRAM-SHA-256\$4096:([^$]+)\$([^:]+):(.+)$`).FindStringSubmatch(v)
	if m == nil {
		t.Fatalf("formato: %q", v)
	}
	if m[1] != rfcSalt {
		t.Fatalf("sal: %q", m[1])
	}
	storedKey, serverKey := b64(t, m[2]), b64(t, m[3])

	authMsg := rfcClientFirst + "," + rfcServerFirst + "," + rfcFinalNoProf
	// Lo que hace el servidor: ClientKey = proof XOR HMAC(StoredKey, AuthMessage),
	// y H(ClientKey) tiene que ser StoredKey.
	proof := b64(t, rfcProof)
	sig := HMAC(storedKey, authMsg)
	ck := make([]byte, len(proof))
	for i := range proof {
		ck[i] = proof[i] ^ sig[i]
	}
	if h := sha256.Sum256(ck); !bytes.Equal(h[:], storedKey) {
		t.Fatal("the RFC client proof does not match our StoredKey")
	}
	if got := base64.StdEncoding.EncodeToString(HMAC(serverKey, authMsg)); got != rfcServerSig {
		t.Fatalf("server signature %s, want %s", got, rfcServerSig)
	}
}

func TestNewVerifierSalAleatoria(t *testing.T) {
	a, err := NewVerifier("x")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewVerifier("x")
	if a == b {
		t.Fatal("two verifiers of the same password must differ (salt)")
	}
	if !strings.HasPrefix(a, "SCRAM-SHA-256$4096:") || strings.Contains(a, "'") {
		t.Fatalf("formato: %q", a)
	}
}

func TestDeriveRechaza(t *testing.T) {
	for _, pw := range []string{"", "con espacio", "ñ", "a\nb"} {
		if _, err := Derive(pw, []byte("s"), 1); err == nil {
			t.Errorf("%q: want error", pw)
		}
	}
	if _, err := Derive("x", nil, 1); err == nil {
		t.Error("empty salt: want error")
	}
}

func TestGeneratePassword(t *testing.T) {
	p, err := GeneratePassword()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(p) {
		t.Fatalf("%q", p)
	}
	q, _ := GeneratePassword()
	if p == q {
		t.Fatal("not random")
	}
}
