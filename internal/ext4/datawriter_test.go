package ext4

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// La escritura de fondo deja los bloques donde tocan, juntos o no, y con
// los búferes turnándose no se pisa lo que aún se está escribiendo.
func TestDataWriterDeFondo(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "img"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := dataWriter{f: f}
	want := make([]byte, 64*blockSize)
	for _, blk := range []uint64{0, 1, 2, 10, 11, 40, 3, 63} {
		b := bytes.Repeat([]byte{byte(blk + 1)}, blockSize)
		copy(want[blk*blockSize:], b)
		if err := d.write(blk, b); err != nil {
			t.Fatal(err)
		}
		if blk == 11 {
			if err := d.flush(); err != nil { // dos en vuelo seguidas
				t.Fatal(err)
			}
		}
	}
	if err := d.flush(); err != nil {
		t.Fatal(err)
	}
	if err := d.wait(); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("the blocks written in the background are not where they belong")
	}
}

// Un error de la escritura de fondo no se pierde: sale en wait (o en la
// siguiente flush).
func TestDataWriterErrorDeFondo(t *testing.T) {
	p := filepath.Join(t.TempDir(), "img")
	os.WriteFile(p, nil, 0o644)
	f, err := os.Open(p) // solo lectura: WriteAt falla
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	d := dataWriter{f: f}
	d.write(0, make([]byte, blockSize))
	if err := d.flush(); err != nil {
		t.Fatalf("flush only launches the write: %v", err)
	}
	if err := d.wait(); err == nil {
		t.Fatal("the background write failed and wait said nothing")
	}
	d.write(5, make([]byte, blockSize))
	d.flush()
	d.write(9, make([]byte, blockSize))
	if err := d.flush(); err == nil {
		t.Fatal("the next flush has to report the previous write's error")
	}
}
