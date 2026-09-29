package xz

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"testing"
)

// Los ficheros de testdata los hizo xz 5.8.3 con distintos ajustes (ver cada
// caso); el sha256 es el del original sin comprimir.
func TestDecode(t *testing.T) {
	cases := []struct{ file, size, sum string }{
		{"text.xz", "188422", "042524c2c12bbb00290094d59701e7b1e0b0478638c4a06225a577e665c8d655"},    // -9e, CRC64
		{"random.xz", "20000", "5dbcf023cf3484f9de1d5326e107aed335e2188906a0541dbe600dcc1e014e71"},   // -0, CRC32: trozos sin comprimir
		{"mix.xz", "235000", "4189c6f70e1bf251218fbe855088972432a07b0c53e1e76958ecad1137b91098"},     // -6, SHA-256, bloques de 32 KiB
		{"mix-lp2.xz", "235000", "4189c6f70e1bf251218fbe855088972432a07b0c53e1e76958ecad1137b91098"}, // lc=0 lp=2 pb=0, sin comprobación
		{"empty.xz", "0", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
	}
	for _, c := range cases {
		f, err := os.Open("testdata/" + c.file)
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewReader(f)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		h := sha256.New()
		n, err := io.Copy(h, r)
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != c.sum {
			t.Errorf("%s: %d bytes, sha256 %s", c.file, n, got)
		}
	}
}

func TestCorrupt(t *testing.T) {
	b, err := os.ReadFile("testdata/text.xz")
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)/2] ^= 0x40
	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, r); err == nil {
		t.Fatal("corrupted stream decoded without error")
	}
}
