package zstd

import (
	"encoding/binary"
	"sync"
)

// Códigos de longitud de literales y de coincidencia: base y bits extra.
var (
	llBase = [36]uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		16, 18, 20, 22, 24, 28, 32, 40, 48, 64, 128, 256, 512, 1024, 2048, 4096,
		8192, 16384, 32768, 65536}
	llBits = [36]uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		1, 1, 1, 1, 2, 2, 3, 3, 4, 6, 7, 8, 9, 10, 11, 12,
		13, 14, 15, 16}
	mlBase = [53]uint32{3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18,
		19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32, 33, 34,
		35, 37, 39, 41, 43, 47, 51, 59, 67, 83, 99, 131, 259, 515, 1027, 2051,
		4099, 8195, 16387, 32771, 65539}
	mlBits = [53]uint8{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		1, 1, 1, 1, 2, 2, 3, 3, 4, 4, 5, 7, 8, 9, 10, 11,
		12, 13, 14, 15, 16}
)

// Las tres clases de símbolo de las secuencias.
const (
	kLL = iota
	kOF
	kML
)

var seqKinds = [3]struct {
	maxLog uint
	maxSym int
	def    []int16 // distribución predefinida
	defLog uint
}{
	kLL: {9, 35, []int16{4, 3, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1, 1, 1,
		2, 2, 2, 2, 2, 2, 2, 2, 2, 3, 2, 1, 1, 1, 1, 1,
		-1, -1, -1, -1}, 6},
	kOF: {8, 31, []int16{1, 1, 1, 1, 1, 1, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, -1, -1, -1, -1, -1}, 5},
	kML: {9, 52, []int16{1, 4, 3, 2, 2, 2, 2, 2, 2, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, -1, -1,
		-1, -1, -1, -1, -1}, 6},
}

var (
	defOnce   sync.Once
	defTables [3]fseTable
)

func predefined(k int) *fseTable {
	defOnce.Do(func() {
		for i, s := range seqKinds {
			if err := defTables[i].build(s.def, s.defLog); err != nil {
				panic("zstd: predefined table: " + err.Error())
			}
		}
	})
	return &defTables[k]
}

// compressed descomprime un bloque comprimido al final de hist.
func (z *Reader) compressed(b []byte) error {
	lits, b, err := z.literals(b)
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return errCorrupt
	}
	nseq := int(b[0])
	switch {
	case nseq < 128:
		b = b[1:]
	case nseq < 255:
		if len(b) < 2 {
			return errCorrupt
		}
		nseq = (nseq-128)<<8 + int(b[1])
		b = b[2:]
	default:
		if len(b) < 3 {
			return errCorrupt
		}
		nseq = int(b[1]) + int(b[2])<<8 + 0x7F00
		b = b[3:]
	}
	if nseq == 0 {
		return z.emit(lits)
	}
	if len(b) == 0 {
		return errCorrupt
	}
	modes := b[0]
	if modes&3 != 0 {
		return errCorrupt
	}
	b = b[1:]
	tabs := [3]**fseTable{&z.ll, &z.of, &z.ml}
	for k, shift := range [3]uint{6, 4, 2} {
		n, err := z.table(k, modes>>shift&3, tabs[k], b)
		if err != nil {
			return err
		}
		b = b[n:]
	}
	return z.sequences(nseq, lits, b)
}

// table prepara la tabla de la clase k según su modo y devuelve los bytes
// que ocupa su descripción.
func (z *Reader) table(k int, mode byte, t **fseTable, b []byte) (int, error) {
	s := seqKinds[k]
	switch mode {
	case 0:
		*t = predefined(k)
		return 0, nil
	case 1:
		if len(b) == 0 || int(b[0]) > s.maxSym {
			return 0, errCorrupt
		}
		z.own[k].rle(b[0])
		*t = &z.own[k]
		return 1, nil
	case 2:
		n, log, used, err := readNorm(b, s.maxLog, s.maxSym, z.norm[:])
		if err != nil {
			return 0, err
		}
		if err := z.own[k].build(z.norm[:n], log); err != nil {
			return 0, err
		}
		*t = &z.own[k]
		return used, nil
	default:
		if *t == nil {
			return 0, errCorrupt
		}
		return 0, nil
	}
}

// literals lee la sección de literales y devuelve los literales y el resto
// del bloque.
func (z *Reader) literals(b []byte) (lits, rest []byte, err error) {
	if len(b) == 0 {
		return nil, nil, errCorrupt
	}
	typ, sf := b[0]&3, b[0]>>2&3
	if typ < 2 { // raw o RLE
		var regen, hl int
		switch sf {
		case 0, 2:
			regen, hl = int(b[0]>>3), 1
		case 1:
			if len(b) < 2 {
				return nil, nil, errCorrupt
			}
			regen, hl = int(b[0]>>4)+int(b[1])<<4, 2
		case 3:
			if len(b) < 3 {
				return nil, nil, errCorrupt
			}
			regen, hl = int(b[0]>>4)+int(b[1])<<4+int(b[2])<<12, 3
		}
		if regen > maxBlock {
			return nil, nil, errCorrupt
		}
		if typ == 0 {
			if len(b) < hl+regen {
				return nil, nil, errCorrupt
			}
			return b[hl : hl+regen], b[hl+regen:], nil
		}
		if len(b) < hl+1 {
			return nil, nil, errCorrupt
		}
		lits = z.lit[:regen]
		for i := range lits {
			lits[i] = b[hl]
		}
		return lits, b[hl+1:], nil
	}
	// Comprimidos con Huffman (2) o con el árbol del bloque anterior (3).
	streams, hl := 4, int(sf)+2
	if sf == 0 {
		streams, hl = 1, 3
	}
	if len(b) < 5 {
		return nil, nil, errCorrupt
	}
	v := uint64(binary.LittleEndian.Uint32(b)) | uint64(b[4])<<32
	var regen, csize int
	switch hl {
	case 3:
		regen, csize = int(v>>4&0x3FF), int(v>>14&0x3FF)
	case 4:
		regen, csize = int(v>>4&0x3FFF), int(v>>18&0x3FFF)
	case 5:
		regen, csize = int(v>>4&0x3FFFF), int(v>>22&0x3FFFF)
	}
	if regen > maxBlock || hl+csize > len(b) {
		return nil, nil, errCorrupt
	}
	data, rest := b[hl:hl+csize], b[hl+csize:]
	if typ == 2 {
		n, err := z.huf.read(data, z.norm[:])
		if err != nil {
			z.hufOK = false
			return nil, nil, err
		}
		z.hufOK = true
		data = data[n:]
	} else if !z.hufOK {
		return nil, nil, errCorrupt
	}
	lits = z.lit[:regen]
	if streams == 1 {
		return lits, rest, z.huf.decode(lits, data)
	}
	if len(data) < 6 {
		return nil, nil, errCorrupt
	}
	seg := (regen + 3) / 4
	if 3*seg > regen {
		return nil, nil, errCorrupt
	}
	jump, data, off := data[:6], data[6:], 0
	for i := 0; i < 4; i++ {
		n := len(data) - off
		if i < 3 {
			n = int(binary.LittleEndian.Uint16(jump[2*i:]))
		}
		if n < 0 || off+n > len(data) {
			return nil, nil, errCorrupt
		}
		dst := lits[i*seg : min((i+1)*seg, regen)]
		if err := z.huf.decode(dst, data[off:off+n]); err != nil {
			return nil, nil, err
		}
		off += n
	}
	return lits, rest, nil
}

// emit añade literales sueltos a la salida del bloque.
func (z *Reader) emit(lits []byte) error {
	n := len(z.hist)
	if n+len(lits) > cap(z.hist) {
		return errCorrupt
	}
	z.hist = z.hist[:n+len(lits)]
	copy(z.hist[n:], lits)
	return nil
}

// sequences decodifica y ejecuta las secuencias del bloque: cada una copia
// literales y luego repite bytes ya escritos.
func (z *Reader) sequences(nseq int, lits, b []byte) error {
	var r revBits
	if err := r.init(b); err != nil {
		return err
	}
	ll, of, ml := z.ll, z.of, z.ml
	sl, so, sm := r.get(ll.log), r.get(of.log), r.get(ml.log)
	r.reload()
	h := z.hist
	n := len(h)
	end := n + maxBlock // room() lo garantizó: cap(h) >= end
	h = h[:end]
	reps := z.reps
	for i := 0; i < nseq; i++ {
		el, eo, em := ll.e[sl], of.e[so], ml.e[sm]
		oc := eo.sym
		off := 1<<oc + r.get(oc)
		r.reload()
		mlen := int(mlBase[em.sym]) + r.get(mlBits[em.sym])
		llen := int(llBase[el.sym]) + r.get(llBits[el.sym])
		r.reload()
		if off > 3 {
			off -= 3
			reps[2], reps[1], reps[0] = reps[1], reps[0], off
		} else {
			idx := off - 1
			if llen == 0 {
				idx++
			}
			switch idx {
			case 0:
				off = reps[0]
			case 1:
				off = reps[1]
				reps[1], reps[0] = reps[0], off
			default:
				if idx == 2 {
					off = reps[2]
				} else {
					off = reps[0] - 1
				}
				if off == 0 {
					return errCorrupt
				}
				reps[2], reps[1], reps[0] = reps[1], reps[0], off
			}
		}
		if i < nseq-1 {
			sl = int(el.base) + r.get(el.nb)
			sm = int(em.base) + r.get(em.nb)
			so = int(eo.base) + r.get(eo.nb)
			r.reload()
		}
		if llen > len(lits) || mlen > end-n-llen || off > n+llen || off > z.window {
			return errCorrupt
		}
		n += copy(h[n:], lits[:llen])
		lits = lits[llen:]
		// Copia que se solapa consigo misma si off < mlen: cada vuelta
		// duplica el trozo de patrón ya escrito.
		src, stop := n-off, n+mlen
		for n < stop {
			n += copy(h[n:stop], h[src:n])
		}
	}
	if !r.done() || len(lits) > end-n {
		return errCorrupt
	}
	n += copy(h[n:], lits)
	z.hist = h[:n]
	z.reps = reps
	return nil
}
