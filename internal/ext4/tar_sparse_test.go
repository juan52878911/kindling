package ext4

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// cabeceraUstar es una cabecera ustar de 512 bytes con su suma puesta.
func cabeceraUstar(name string, typ byte, size int) []byte {
	h := make([]byte, 512)
	copy(h, name)
	copy(h[100:], "0000644\x00")
	copy(h[108:], "0000000\x00")
	copy(h[116:], "0000000\x00")
	copy(h[124:], fmt.Sprintf("%011o\x00", size))
	copy(h[136:], "00000000000\x00")
	h[156] = typ
	copy(h[257:], "ustar\x0000")
	copy(h[148:], "        ")
	sum := 0
	for _, c := range h {
		sum += int(c)
	}
	copy(h[148:], fmt.Sprintf("%06o\x00 ", sum))
	return h
}

func bloque(b []byte) []byte {
	return append(b, make([]byte, (512-len(b)%512)%512)...)
}

func registroPAX(k, v string) string {
	s := " " + k + "=" + v + "\n"
	n := len(s) + 1
	for len(fmt.Sprint(n))+len(s) != n {
		n++
	}
	return fmt.Sprint(n) + s
}

// Un fichero disperso (PAX GNU.sparse.*) declara un tamaño que no viene en
// el tar: archive/tar lo rellena con ceros, y una capa de unos KiB se
// convertiría en un disco de GiB. Se rechaza, como hace internal/oci.
func TestAddTarRejectsSparse(t *testing.T) {
	pax := registroPAX("GNU.sparse.size", "1073741824") +
		registroPAX("GNU.sparse.numblocks", "1") +
		registroPAX("GNU.sparse.offset", "0") +
		registroPAX("GNU.sparse.numbytes", "1")
	var tb bytes.Buffer
	tb.Write(cabeceraUstar("PaxHeaders/f", 'x', len(pax)))
	tb.Write(bloque([]byte(pax)))
	tb.Write(cabeceraUstar("f", '0', 1))
	tb.Write(bloque([]byte("x")))
	tb.Write(make([]byte, 1024))

	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	err := root.AddTar(&tb, TarOptions{})
	if err == nil || !strings.Contains(err.Error(), "sparse") {
		if n, _ := root.Resolve("/f"); n != nil {
			t.Fatalf("sparse entry accepted with size %d (err %v)", n.Size, err)
		}
		t.Fatalf("sparse entry: %v", err)
	}
}
