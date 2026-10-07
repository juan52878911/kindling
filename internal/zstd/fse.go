package zstd

import "math/bits"

// maxSyms es el número de símbolos del alfabeto más grande (las longitudes
// de coincidencia, 0..52).
const maxSyms = 53

type fseEntry struct {
	sym  uint8
	nb   uint8  // bits que se leen para el estado siguiente
	base uint16 // estado siguiente = base + esos bits
}

// fseTable es una tabla de decodificación FSE de 1<<log estados.
type fseTable struct {
	log uint8
	e   []fseEntry
}

// readNorm lee una descripción de tabla FSE (las probabilidades
// normalizadas) de b y devuelve cuántos símbolos trae, su Accuracy_Log y los
// bytes que ocupa. norm debe tener sitio para maxSym+1 símbolos.
func readNorm(b []byte, maxLog uint, maxSym int, norm []int16) (n int, log uint, used int, err error) {
	r := fwdBits{b: b}
	log = uint(r.get(4)) + 5
	if log > maxLog {
		return 0, 0, 0, errCorrupt
	}
	remaining := 1<<log + 1
	threshold := 1 << log
	nb := log + 1
	prev0 := false
	for remaining > 1 && n <= maxSym {
		if prev0 {
			// Tras un 0, cada 2 bits dicen cuántos ceros más siguen; un 3,
			// que hay otros 2 bits detrás. n > maxSym corta la repetición.
			for {
				rep := int(r.get(2))
				for k := 0; k < rep; k++ {
					if n > maxSym {
						return 0, 0, 0, errCorrupt
					}
					norm[n] = 0
					n++
				}
				if rep != 3 {
					break
				}
			}
			if n > maxSym {
				return 0, 0, 0, errCorrupt
			}
		}
		lim := 2*threshold - 1 - remaining
		v := int(r.peek())
		var c int
		if v&(threshold-1) < lim {
			c = v & (threshold - 1)
			r.pos += nb - 1
		} else {
			c = v & (2*threshold - 1)
			if c >= threshold {
				c -= lim
			}
			r.pos += nb
		}
		c--
		if c < 0 {
			remaining += c
		} else {
			remaining -= c
		}
		norm[n] = int16(c)
		n++
		prev0 = c == 0
		for remaining < threshold {
			nb--
			threshold >>= 1
		}
	}
	used = int((r.pos + 7) / 8)
	if remaining != 1 || used > len(b) {
		return 0, 0, 0, errCorrupt
	}
	return n, log, used, nil
}

// build hace la tabla de decodificación de las probabilidades norm.
func (t *fseTable) build(norm []int16, log uint) error {
	size := 1 << log
	if cap(t.e) < size {
		t.e = make([]fseEntry, size, 1<<9)
	}
	t.e = t.e[:size]
	t.log = uint8(log)
	var next [256]uint16
	high := size - 1
	total := 0
	for s, c := range norm {
		switch {
		case c == -1:
			if high < 0 {
				return errCorrupt
			}
			t.e[high].sym = uint8(s)
			high--
			next[s] = 1
			total++
		case c > 0:
			next[s] = uint16(c)
			total += int(c)
		}
	}
	if total != size {
		return errCorrupt
	}
	pos, step, mask := 0, size>>1+size>>3+3, size-1
	for s, c := range norm {
		for i := 0; i < int(c); i++ {
			t.e[pos].sym = uint8(s)
			pos = (pos + step) & mask
			for pos > high {
				pos = (pos + step) & mask
			}
		}
	}
	if pos != 0 {
		return errCorrupt
	}
	for u := range t.e {
		s := t.e[u].sym
		n := int(next[s])
		next[s]++
		nb := int(log) - (bits.Len(uint(n)) - 1)
		t.e[u].nb = uint8(nb)
		t.e[u].base = uint16(n<<nb - size)
	}
	return nil
}

// rle hace la tabla de un solo símbolo (modo RLE): no lee bits.
func (t *fseTable) rle(sym uint8) {
	if cap(t.e) < 1 {
		t.e = make([]fseEntry, 1, 1<<9)
	}
	t.e = t.e[:1]
	t.e[0] = fseEntry{sym: sym}
	t.log = 0
}
