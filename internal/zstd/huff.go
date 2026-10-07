package zstd

import "math/bits"

const maxHuffBits = 11

// huffTable decodifica literales: cada entrada, indexada por los maxBits
// bits siguientes del flujo, es símbolo<<8 | bits que ocupa.
type huffTable struct {
	maxBits uint8
	t       [1 << maxHuffBits]uint16
}

// read lee una descripción de árbol Huffman (los pesos, directos o
// comprimidos con FSE) y devuelve los bytes que ocupa.
func (h *huffTable) read(b []byte, norm []int16) (int, error) {
	if len(b) == 0 {
		return 0, errCorrupt
	}
	var w [256]uint8
	n, used := 0, 0
	if hb := int(b[0]); hb >= 128 {
		n = hb - 127
		used = 1 + (n+1)/2
		if used > len(b) {
			return 0, errCorrupt
		}
		for i := 0; i < n; i++ {
			v := b[1+i/2]
			if i%2 == 0 {
				v >>= 4
			}
			w[i] = v & 15
		}
	} else {
		used = 1 + hb
		if used > len(b) {
			return 0, errCorrupt
		}
		var err error
		if n, err = fseWeights(b[1:used], norm, &w); err != nil {
			return 0, err
		}
	}
	return used, h.build(w[:n])
}

// fseWeights decodifica los pesos comprimidos con FSE: dos estados que se
// turnan sobre el mismo flujo hasta que se pasa del principio.
func fseWeights(b []byte, norm []int16, w *[256]uint8) (int, error) {
	ns, log, k, err := readNorm(b, 6, maxHuffBits+1, norm)
	if err != nil {
		return 0, err
	}
	var t fseTable
	var e [1 << 6]fseEntry
	t.e = e[:0]
	if err := t.build(norm[:ns], log); err != nil {
		return 0, err
	}
	var r revBits
	if err := r.init(b[k:]); err != nil {
		return 0, err
	}
	s := [2]int{r.get(t.log), r.get(t.log)}
	n := 0
	for i := 0; ; i ^= 1 {
		if n >= 255 {
			return 0, errCorrupt
		}
		x := t.e[s[i]]
		w[n] = x.sym
		n++
		s[i] = int(x.base) + r.get(x.nb)
		r.reload()
		if r.used > 64 {
			if n >= 255 {
				return 0, errCorrupt
			}
			w[n] = t.e[s[i^1]].sym
			return n + 1, nil
		}
	}
}

// build hace la tabla de los pesos w; el peso del último símbolo no viene:
// es el que completa una potencia de 2.
func (h *huffTable) build(w []uint8) error {
	var count [maxHuffBits + 2]int
	sum := 0
	for _, x := range w {
		if x > maxHuffBits {
			return errCorrupt
		}
		count[x]++
		if x > 0 {
			sum += 1 << (x - 1)
		}
	}
	if sum == 0 {
		return errCorrupt
	}
	mb := bits.Len(uint(sum))
	if mb > maxHuffBits {
		return errCorrupt
	}
	rest := 1<<mb - sum
	if rest&(rest-1) != 0 {
		return errCorrupt
	}
	last := uint8(bits.Len(uint(rest)))
	count[last]++
	// Los de menos peso (códigos más largos) van primero en la tabla, y a
	// igual peso, por orden de símbolo.
	var start [maxHuffBits + 2]int
	p := 0
	for x := 1; x <= mb; x++ {
		start[x] = p
		p += count[x] << (x - 1)
	}
	h.maxBits = uint8(mb)
	put := func(sym int, x uint8) {
		if x == 0 {
			return
		}
		e := uint16(sym)<<8 | uint16(mb+1-int(x))
		n := 1 << (x - 1)
		for i := start[x]; i < start[x]+n; i++ {
			h.t[i] = e
		}
		start[x] += n
	}
	for s, x := range w {
		put(s, x)
	}
	put(len(w), last)
	return nil
}

// decode llena dst con los símbolos de un flujo, que tiene que acabar justo.
func (h *huffTable) decode(dst, src []byte) error {
	var r revBits
	if err := r.init(src); err != nil {
		return err
	}
	shift := 64 - h.maxBits
	t := &h.t
	i := 0
	for ; i+4 <= len(dst); i += 4 {
		r.reload()
		for k := 0; k < 4; k++ {
			e := t[r.val<<r.used>>shift]
			dst[i+k] = byte(e >> 8)
			r.used += uint(e & 0xff)
		}
	}
	for ; i < len(dst); i++ {
		r.reload()
		e := t[r.val<<r.used>>shift]
		dst[i] = byte(e >> 8)
		r.used += uint(e & 0xff)
	}
	if !r.done() {
		return errCorrupt
	}
	return nil
}
