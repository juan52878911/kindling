package upgrade

import (
	"encoding/binary"
	"math/bits"
)

// BLAKE2b (RFC 7693) sin clave, de una sola pasada. minisign la usa en dos
// sitios: el «prehash» de lo que firma (BLAKE2b-512, firmas «ED») y la suma de
// control de su clave secreta (BLAKE2b-256). La stdlib no la trae y el núcleo
// no admite dependencias (golang.org/x/crypto/blake2b), así que va aquí: son
// ~60 líneas y los tests la comparan con los vectores del RFC y de hashlib.
// Lo que se firma es un SHA256SUMS de pocos KiB, así que no hace falta streaming.

var blakeIV = [8]uint64{
	0x6a09e667f3bcc908, 0xbb67ae8584caa73b, 0x3c6ef372fe94f82b, 0xa54ff53a5f1d36f1,
	0x510e527fade682d1, 0x9b05688c2b3e6c1f, 0x1f83d9abfb41bd6b, 0x5be0cd19137e2179,
}

var blakeSigma = [12][16]byte{
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
	{11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4},
	{7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8},
	{9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13},
	{2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9},
	{12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11},
	{13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10},
	{6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5},
	{10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0},
	{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
	{14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3},
}

// blake2b devuelve los size primeros bytes (1..64) del BLAKE2b de datos.
func blake2b(datos []byte, size int) []byte {
	if size < 1 || size > 64 {
		panic("blake2b: tamaño fuera de 1..64")
	}
	h := blakeIV
	h[0] ^= 0x01010000 ^ uint64(size) // fanout 1, profundidad 1, sin clave
	n := len(datos)
	var t uint64
	var bloque [128]byte
	if n == 0 {
		blakeComprimir(&h, &bloque, 0, true)
	}
	for off := 0; off < n; off += 128 {
		fin := min(off+128, n)
		bloque = [128]byte{}
		copy(bloque[:], datos[off:fin])
		t += uint64(fin - off)
		blakeComprimir(&h, &bloque, t, fin == n)
	}
	out := make([]byte, 64)
	for i, v := range h {
		binary.LittleEndian.PutUint64(out[8*i:], v)
	}
	return out[:size]
}

func blakeComprimir(h *[8]uint64, bloque *[128]byte, t uint64, ultimo bool) {
	var m [16]uint64
	for i := range m {
		m[i] = binary.LittleEndian.Uint64(bloque[8*i:])
	}
	var v [16]uint64
	copy(v[:8], h[:])
	copy(v[8:], blakeIV[:])
	v[12] ^= t // el contador alto (v[13]) sigue a 0: nada aquí pasa de 2^64 bytes
	if ultimo {
		v[14] = ^v[14]
	}
	g := func(a, b, c, d int, x, y uint64) {
		v[a] += v[b] + x
		v[d] = bits.RotateLeft64(v[d]^v[a], -32)
		v[c] += v[d]
		v[b] = bits.RotateLeft64(v[b]^v[c], -24)
		v[a] += v[b] + y
		v[d] = bits.RotateLeft64(v[d]^v[a], -16)
		v[c] += v[d]
		v[b] = bits.RotateLeft64(v[b]^v[c], -63)
	}
	for _, s := range blakeSigma {
		g(0, 4, 8, 12, m[s[0]], m[s[1]])
		g(1, 5, 9, 13, m[s[2]], m[s[3]])
		g(2, 6, 10, 14, m[s[4]], m[s[5]])
		g(3, 7, 11, 15, m[s[6]], m[s[7]])
		g(0, 5, 10, 15, m[s[8]], m[s[9]])
		g(1, 6, 11, 12, m[s[10]], m[s[11]])
		g(2, 7, 8, 13, m[s[12]], m[s[13]])
		g(3, 4, 9, 14, m[s[14]], m[s[15]])
	}
	for i := range h {
		h[i] ^= v[i] ^ v[i+8]
	}
}
