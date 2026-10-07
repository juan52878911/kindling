package zstd

import (
	"encoding/binary"
	"math/bits"
)

// xxh64 es XXH64 con semilla 0, en streaming: la suma de comprobación de
// los marcos.
type xxh64 struct {
	v     [4]uint64
	total uint64
	buf   [32]byte
	nbuf  int
}

const (
	prime1 uint64 = 11400714785074694791
	prime2 uint64 = 14029467366897019727
	prime3 uint64 = 1609587929392839161
	prime4 uint64 = 9650029242287828579
	prime5 uint64 = 2870177450012600261
)

func (x *xxh64) reset() {
	p1 := prime1 // sin constante: las sumas dan la vuelta, como en C
	*x = xxh64{v: [4]uint64{p1 + prime2, prime2, 0, -p1}}
}

func round(acc, in uint64) uint64 {
	return bits.RotateLeft64(acc+in*prime2, 31) * prime1
}

func (x *xxh64) stripes(b []byte) {
	for ; len(b) >= 32; b = b[32:] {
		x.v[0] = round(x.v[0], binary.LittleEndian.Uint64(b))
		x.v[1] = round(x.v[1], binary.LittleEndian.Uint64(b[8:]))
		x.v[2] = round(x.v[2], binary.LittleEndian.Uint64(b[16:]))
		x.v[3] = round(x.v[3], binary.LittleEndian.Uint64(b[24:]))
	}
}

func (x *xxh64) write(b []byte) {
	x.total += uint64(len(b))
	if x.nbuf > 0 {
		k := copy(x.buf[x.nbuf:], b)
		x.nbuf += k
		b = b[k:]
		if x.nbuf < 32 {
			return
		}
		x.stripes(x.buf[:])
		x.nbuf = 0
	}
	n := len(b) &^ 31
	x.stripes(b[:n])
	x.nbuf = copy(x.buf[:], b[n:])
}

func (x *xxh64) sum() uint64 {
	var h uint64
	if x.total >= 32 {
		v := x.v
		h = bits.RotateLeft64(v[0], 1) + bits.RotateLeft64(v[1], 7) +
			bits.RotateLeft64(v[2], 12) + bits.RotateLeft64(v[3], 18)
		for _, a := range v {
			h ^= round(0, a)
			h = h*prime1 + prime4
		}
	} else {
		h = prime5
	}
	h += x.total
	b := x.buf[:x.nbuf]
	for ; len(b) >= 8; b = b[8:] {
		h ^= round(0, binary.LittleEndian.Uint64(b))
		h = bits.RotateLeft64(h, 27)*prime1 + prime4
	}
	if len(b) >= 4 {
		h ^= uint64(binary.LittleEndian.Uint32(b)) * prime1
		h = bits.RotateLeft64(h, 23)*prime2 + prime3
		b = b[4:]
	}
	for _, c := range b {
		h ^= uint64(c) * prime5
		h = bits.RotateLeft64(h, 11) * prime1
	}
	h ^= h >> 33
	h *= prime2
	h ^= h >> 29
	h *= prime3
	h ^= h >> 32
	return h
}
