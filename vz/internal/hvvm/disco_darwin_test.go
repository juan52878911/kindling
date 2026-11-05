//go:build darwin && arm64

package hvvm

import (
	"bytes"
	"math/rand"
	"testing"
)

// TestDiscoVariosBuferes: una petición con varios búferes de datos (que van
// en un solo preadv/pwritev) lee y escribe lo mismo que uno a uno, sin
// importar cómo se corte, y un búfer que no vale deja hechos los de antes y
// la petición en error, como antes. El disco de pruebas mide 1 MiB (2048
// sectores).
func TestDiscoVariosBuferes(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	escrito := make([]byte, 512+1536+512)
	r.Read(escrito)
	if e, u := esDisco(1, 10, []uint32{512, 1536, 512}, append([]byte(nil), escrito...), -1); e != 0 || u != 1 {
		t.Fatalf("OUT: status %d, used len %d; want 0, 1", e, u)
	}
	leido := make([]byte, len(escrito))
	if e, u := esDisco(0, 10, []uint32{1024, 1024, 512}, leido, -1); e != 0 || u != uint32(len(leido))+1 {
		t.Fatalf("IN: status %d, used len %d; want 0, %d", e, u, len(leido)+1)
	}
	if !bytes.Equal(leido, escrito) {
		t.Fatal("IN with other buffer sizes does not return what OUT wrote")
	}

	// El segundo búfer fuera de la RAM: el primero se lee igual.
	leido = make([]byte, 3*512)
	if e, u := esDisco(0, 10, []uint32{512, 512, 512}, leido, 1); e != 1 || u != 513 {
		t.Fatalf("IN with a bad buffer: status %d, used len %d; want 1, 513", e, u)
	}
	if !bytes.Equal(leido[:512], escrito[:512]) {
		t.Fatal("IN with a bad buffer did not read the buffers before it")
	}

	// Pasado el final del disco: el primer búfer (el último sector) se
	// escribe, el segundo no, y la petición falla.
	fin := make([]byte, 1024)
	r.Read(fin)
	if e, _ := esDisco(1, 2047, []uint32{512, 512}, append([]byte(nil), fin...), -1); e != 1 {
		t.Fatalf("OUT past the end: status %d, want 1", e)
	}
	ultimo := make([]byte, 512)
	if e, _ := esDisco(0, 2047, []uint32{512}, ultimo, -1); e != 0 || !bytes.Equal(ultimo, fin[:512]) {
		t.Fatalf("last sector: status %d, same data %v", e, bytes.Equal(ultimo, fin[:512]))
	}
	if e, _ := esDisco(0, 2048, []uint32{512}, make([]byte, 512), -1); e != 1 {
		t.Fatalf("IN at the end of the disk: status %d, want 1", e)
	}
	// Sin búferes de datos no hay nada que leer: bien, como antes.
	if e, u := esDisco(0, 0, nil, nil, -1); e != 0 || u != 1 {
		t.Fatalf("IN without data: status %d, used len %d; want 0, 1", e, u)
	}
}
