package zstd

import (
	"bytes"
	"strings"
	"testing"
)

// Marcos hechos a mano para que cada comprobación contra una entrada mala
// tenga su caso: sin ella, el caso da un pánico o una salida sin error.

// bitw escribe un flujo de bits hacia atrás: lo último que se escribe es lo
// primero que lee el descompresor.
type bitw struct{ bits []byte }

func (w *bitw) put(v, n int) {
	for i := 0; i < n; i++ {
		w.bits = append(w.bits, byte(v>>i&1))
	}
}

// bytes cierra el flujo con el bit marcador; skip quita los primeros bits
// escritos (los últimos que se leen): faltan al leer.
func (w *bitw) bytes(skip int) []byte {
	bits := append(append([]byte(nil), w.bits[skip:]...), 1)
	out := make([]byte, (len(bits)+7)/8)
	for i, b := range bits {
		out[i/8] |= b << (i % 8)
	}
	return out
}

// state da un estado de la tabla predefinida k que decodifica sym.
func state(t *testing.T, k, sym int) int {
	for i, e := range predefined(k).e {
		if int(e.sym) == sym {
			return i
		}
	}
	t.Fatalf("no state for symbol %d of table %d", sym, k)
	return 0
}

// seq es una secuencia por sus códigos y bits extra.
type seq struct{ llc, llx, ofc, ofx, mlc, mlx int }

// seqBlock hace un bloque comprimido con lits en crudo y una secuencia con
// las tablas predefinidas. junk añade bits de más al principio del flujo y
// skip quita bits.
func seqBlock(t *testing.T, lits []byte, s seq, junk, skip int) []byte {
	var w bitw
	w.put(0, junk)
	w.put(s.llx, int(llBits[s.llc]))
	w.put(s.mlx, int(mlBits[s.mlc]))
	w.put(s.ofx, s.ofc)
	w.put(state(t, kML, s.mlc), 6)
	w.put(state(t, kOF, s.ofc), 5)
	w.put(state(t, kLL, s.llc), 6)
	b := append([]byte{byte(len(lits) << 3)}, lits...) // literales en crudo
	b = append(b, 1, 0)                                // una secuencia, tablas predefinidas
	return append(b, w.bytes(skip)...)
}

// huffBlock hace un bloque de literales con Huffman, sin secuencias, con dos
// símbolos (0 y 1) de un bit cada uno.
func huffBlock(lits []byte, junk, skip int) []byte {
	var w bitw
	w.put(0, junk)
	for i := len(lits) - 1; i >= 0; i-- {
		w.put(int(lits[i]), 1)
	}
	data := append([]byte{128, 0x10}, w.bytes(skip)...) // peso 1 al símbolo 0; el 1, implícito
	v := 2 | len(lits)<<4 | len(data)<<14
	return append(append([]byte{byte(v), byte(v >> 8), byte(v >> 16)}, data...), 0)
}

// frame1k hace un marco sin tamaño y con ventana de 1 KiB: un bloque raw de
// prefix bytes (si hay) y luego el bloque comprimido b, el último.
func frame1k(prefix int, b []byte) []byte {
	out := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x00}
	if prefix > 0 {
		h := prefix << 3
		out = append(append(out, byte(h), byte(h>>8), byte(h>>16)), random(prefix, 51)...)
	}
	h := len(b)<<3 | 2<<1 | 1
	return append(append(out, byte(h), byte(h>>8), byte(h>>16)), b...)
}

func TestHostile(t *testing.T) {
	abcd := []byte("abcd")
	// Cuatro literales y repetir 8 bytes a distancia 4 (desplazamiento 7: 4+3).
	good := seq{llc: 4, ofc: 2, ofx: 3, mlc: 5}
	long := good
	long.mlc, long.mlx = 52, 0xFFFF // 131074 bytes: se sale del bloque
	far := good
	far.ofc, far.ofx = 10, 1500+3-1024 // a 1500 bytes, con ventana de 1 KiB

	ok := []struct {
		name string
		in   []byte
		want []byte
	}{
		{"sequence", frame1k(0, seqBlock(t, abcd, good, 0, 0)), []byte("abcdabcdabcd")},
		{"huffman", frame1k(0, huffBlock([]byte{0, 1, 1, 0}, 0, 0)), []byte{0, 1, 1, 0}},
	}
	for _, c := range ok {
		got, err := decode(c.in)
		if err != nil || !bytes.Equal(got, c.want) {
			t.Fatalf("%s: got %q, %v; want %q", c.name, got, err, c.want)
		}
	}

	// 4 flujos de Huffman con 5 literales (2, 2 y 1, todos ceros): el cuarto
	// empezaría después del final.
	four := []byte{0x56, 0x00, 0x03, 128, 0x10, 1, 0, 1, 0, 1, 0, 0b100, 0b100, 0b10, 1, 0}

	bad := []struct {
		name string
		in   []byte
		want string
	}{
		{"sequence bits left over", frame1k(0, seqBlock(t, abcd, good, 8, 0)), "corrupt"},
		{"sequence bits missing", frame1k(0, seqBlock(t, abcd, good, 0, 2)), "corrupt"},
		{"match past the block", frame1k(0, seqBlock(t, abcd, long, 0, 0)), "corrupt"},
		{"offset past the window", frame1k(2000, seqBlock(t, abcd, far, 0, 0)), "corrupt"},
		{"huffman bits left over", frame1k(0, huffBlock([]byte{0, 1, 1, 0}, 8, 0)), "corrupt"},
		{"huffman bits missing", frame1k(0, huffBlock([]byte{0, 1, 1, 0}, 0, 1)), "corrupt"},
		{"4 streams, 5 literals", frame1k(0, four), "corrupt"},
		// Tamaño 3 y un bloque RLE de 5.
		{"longer than its size", []byte{0x28, 0xB5, 0x2F, 0xFD, 0x20, 3, 0x2B, 0, 0, 'z'}, "longer than"},
	}
	for _, c := range bad {
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("%s: panic: %v", c.name, p)
				}
			}()
			got, err := decode(c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: got %d bytes, %v; want an error with %q", c.name, len(got), err, c.want)
			}
		}()
	}
}

// Una tabla FSE cuyas probabilidades no suman 1<<log se rechaza, aunque el
// reparto acabe donde empezó.
func TestFSEBuildSum(t *testing.T) {
	for _, norm := range [][]int16{{32, 32}, {0, 0}} {
		var f fseTable
		if err := f.build(norm, 5); err == nil {
			t.Errorf("build(%v, 5) accepted", norm)
		}
	}
}
