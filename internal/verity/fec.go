package verity

import (
	"io"
	"os"
	"runtime"
	"sync"
)

// FEC de dm-verity: Reed-Solomon RS(255, 255-raíces) sobre GF(2^8) con el
// polinomio 0x11d, primera raíz 0 y primitivo 1, el codificador de Phil Karn
// que usan libfec y cryptsetup (init_rs_char(8, 0x11d, 0, 1, raíces)).
//
// El área cubierta (datos + árbol, sin huecos entre medias) se ve como rsn
// franjas seguidas de rounds*4096 bytes; la palabra c toma el byte c de cada
// franja (lo que pase del final cuenta como cero) y su paridad va en
// fec[c*raíces:]. Es lo que hace fec_interleave() del núcleo al leerlo.

const gfPoly = 0x11d

type rs struct {
	roots int
	// mul[j][v] = v * g_j en GF(256), con g el polinomio generador.
	mul [][256]byte
}

func newRS(roots int) *rs {
	var alpha [256]byte
	var index [256]int
	index[0] = 255
	sr := 1
	for i := 0; i < 255; i++ {
		index[sr] = i
		alpha[i] = byte(sr)
		sr <<= 1
		if sr&0x100 != 0 {
			sr ^= gfPoly
		}
		sr &= 255
	}
	modnn := func(x int) int {
		for x >= 255 {
			x -= 255
			x = (x >> 8) + (x & 255)
		}
		return x
	}
	// Generador: prod (x + alpha^(fcr+i)*prim), fcr = 0, prim = 1.
	gen := make([]int, roots+1)
	gen[0] = 1
	for i, root := 0, 0; i < roots; i, root = i+1, root+1 {
		gen[i+1] = 1
		for j := i; j > 0; j-- {
			if gen[j] != 0 {
				gen[j] = gen[j-1] ^ int(alpha[modnn(index[gen[j]]+root)])
			} else {
				gen[j] = gen[j-1]
			}
		}
		gen[0] = int(alpha[modnn(index[gen[0]]+root)])
	}
	r := &rs{roots: roots, mul: make([][256]byte, roots+1)}
	for j := 0; j <= roots; j++ {
		gj := index[gen[j]]
		for v := 1; v < 256; v++ {
			r.mul[j][v] = alpha[modnn(index[v]+gj)]
		}
	}
	return r
}

// update mete el símbolo d en el registro bb (raíces bytes) de una palabra.
func (r *rs) update(bb []byte, d byte) {
	v := d ^ bb[0]
	n := r.roots
	for j := 1; j < n; j++ {
		bb[j-1] = bb[j] ^ r.mul[n-j][v]
	}
	bb[n-1] = r.mul[0][v]
}

func writeFEC(f *os.File, blocks uint64, roots int, at int64) error {
	return fecTo(f, blocks, roots, f, at)
}

// fecTo calcula el FEC de los primeros blocks bloques de src y lo escribe en
// dst a partir de at.
func fecTo(src io.ReaderAt, blocks uint64, roots int, dst io.WriterAt, at int64) error {
	code := newRS(roots)
	rsn := uint64(255 - roots)
	rounds := (blocks + rsn - 1) / rsn
	stripe := rounds * BlockSize // bytes por franja = número de palabras
	size := blocks * BlockSize
	state := make([]byte, stripe*uint64(roots))
	buf := make([]byte, stripe)
	workers := runtime.GOMAXPROCS(0)
	for i := uint64(0); i < rsn; i++ {
		off := i * stripe
		clear(buf)
		if off < size {
			n := stripe
			if off+n > size {
				n = size - off
			}
			if _, err := src.ReadAt(buf[:n], int64(off)); err != nil {
				return err
			}
		}
		var wg sync.WaitGroup
		per := (stripe + uint64(workers) - 1) / uint64(workers)
		for w := 0; w < workers; w++ {
			lo := uint64(w) * per
			hi := lo + per
			if hi > stripe {
				hi = stripe
			}
			if lo >= hi {
				break
			}
			wg.Add(1)
			go func(lo, hi uint64) {
				defer wg.Done()
				for c := lo; c < hi; c++ {
					code.update(state[c*uint64(roots):(c+1)*uint64(roots)], buf[c])
				}
			}(lo, hi)
		}
		wg.Wait()
	}
	_, err := dst.WriteAt(state, at)
	return err
}
