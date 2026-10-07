package zstd

import (
	"encoding/binary"
	"math/bits"
)

// fwdBits lee bits hacia delante, el menos significativo primero: las
// descripciones de las tablas FSE. Pasado el final lee ceros; quien llama
// comprueba al acabar que no se pasó.
type fwdBits struct {
	b   []byte
	pos uint // en bits
}

func (r *fwdBits) peek() uint32 {
	i := int(r.pos >> 3)
	var v uint64
	for k := 0; k < 5 && i+k < len(r.b); k++ {
		v |= uint64(r.b[i+k]) << (8 * k)
	}
	return uint32(v >> (r.pos & 7))
}

func (r *fwdBits) get(n uint) uint32 {
	v := r.peek() & (1<<n - 1)
	r.pos += n
	return v
}

// revBits lee un flujo de bits hacia atrás, como los de Huffman y los de
// las secuencias: empieza por el final, tras el bit marcador del último
// byte. val es una ventana de 8 bytes del flujo que empieza en pos; used, los
// bits ya leídos desde arriba. Leer más allá del principio da ceros y deja
// used > 64, que es como se detecta.
type revBits struct {
	b    []byte
	pos  int
	val  uint64
	used uint
}

func (r *revBits) init(b []byte) error {
	if len(b) == 0 || b[len(b)-1] == 0 {
		return errCorrupt
	}
	r.b = b
	r.used = 9 - uint(bits.Len8(b[len(b)-1]))
	if len(b) >= 8 {
		r.pos = len(b) - 8
		r.val = binary.LittleEndian.Uint64(b[r.pos:])
		return nil
	}
	r.pos, r.val = 0, 0
	for i := len(b) - 1; i >= 0; i-- {
		r.val = r.val<<8 | uint64(b[i])
	}
	r.used += uint(8-len(b)) * 8
	return nil
}

// get lee n bits (n <= 56 desde el último reload).
func (r *revBits) get(n uint8) int {
	v := r.val << r.used >> (64 - n)
	r.used += uint(n)
	return int(v)
}

// reload rellena la ventana: tras él, used <= 7 salvo al final del flujo.
func (r *revBits) reload() {
	if r.used > 64 || len(r.b) < 8 {
		return
	}
	nb := min(int(r.used>>3), r.pos)
	r.pos -= nb
	r.used -= uint(nb) * 8
	r.val = binary.LittleEndian.Uint64(r.b[r.pos:])
}

// done dice si se leyó el flujo entero, ni un bit más ni uno menos.
func (r *revBits) done() bool { return r.pos == 0 && r.used == 64 }
