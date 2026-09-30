package xz

import (
	"errors"
	"io"
)

// Decodificador LZMA2/LZMA, según la especificación del SDK de LZMA (el
// LzmaDec.c de Igor Pavlov y el xz-embedded de Lasse Collin).

var errData = errors.New("xz: corrupt LZMA2 data")

const (
	numStates     = 12
	posStatesMax  = 16
	lenLowBits    = 3
	lenMidBits    = 3
	lenHighBits   = 8
	lenLowSyms    = 1 << lenLowBits
	lenMidSyms    = 1 << lenMidBits
	matchMinLen   = 2
	distStates    = 4
	distSlotBits  = 6
	distModelEnd  = 14
	fullDistances = 1 << (distModelEnd / 2)
	alignBits     = 4
	probInit      = 1024
)

type prob uint16

type rangeDec struct {
	r     io.ByteReader
	rng   uint32
	code  uint32
	err   error
	limit int // bytes comprimidos que quedan del trozo
}

func (d *rangeDec) byte() byte {
	if d.limit <= 0 {
		d.err = errData
		return 0
	}
	d.limit--
	b, err := d.r.ReadByte()
	if err != nil {
		d.err = io.ErrUnexpectedEOF
	}
	return b
}

func (d *rangeDec) init() {
	d.rng = 0xFFFFFFFF
	d.code = 0
	if d.byte() != 0 {
		d.err = errData
	}
	for i := 0; i < 4; i++ {
		d.code = d.code<<8 | uint32(d.byte())
	}
	if d.code == d.rng {
		d.err = errData
	}
}

func (d *rangeDec) normalize() {
	if d.rng < 1<<24 {
		d.rng <<= 8
		d.code = d.code<<8 | uint32(d.byte())
	}
}

func (d *rangeDec) bit(p *prob) uint32 {
	d.normalize()
	bound := (d.rng >> 11) * uint32(*p)
	if d.code < bound {
		d.rng = bound
		*p += (2048 - *p) >> 5
		return 0
	}
	d.rng -= bound
	d.code -= bound
	*p -= *p >> 5
	return 1
}

func (d *rangeDec) tree(probs []prob, bits int) uint32 {
	m := uint32(1)
	for i := 0; i < bits; i++ {
		m = m<<1 | d.bit(&probs[m])
	}
	return m - 1<<bits
}

func (d *rangeDec) reverse(probs []prob, bits int) uint32 {
	m, sym := uint32(1), uint32(0)
	for i := 0; i < bits; i++ {
		b := d.bit(&probs[m])
		m = m<<1 | b
		sym |= b << i
	}
	return sym
}

// reverse1 es reverse con los índices desplazados uno (probs[m-1]): así se
// puede pasar el trozo de las distancias especiales que empieza en el
// primer bloque que se usa (en el SDK el puntero base queda uno antes).
func (d *rangeDec) reverse1(probs []prob, bits int) uint32 {
	m, sym := uint32(1), uint32(0)
	for i := 0; i < bits; i++ {
		b := d.bit(&probs[m-1])
		m = m<<1 | b
		sym |= b << i
	}
	return sym
}

func (d *rangeDec) direct(bits int) uint32 {
	var res uint32
	for ; bits > 0; bits-- {
		d.normalize()
		d.rng >>= 1
		t := uint32(0)
		if d.code >= d.rng {
			d.code -= d.rng
			t = 1
		}
		res = res<<1 | t
	}
	return res
}

type lenDec struct {
	choice, choice2 prob
	low             [posStatesMax][lenLowSyms]prob
	mid             [posStatesMax][lenMidSyms]prob
	high            [1 << lenHighBits]prob
}

func (l *lenDec) reset() {
	l.choice, l.choice2 = probInit, probInit
	for i := range l.low {
		for j := range l.low[i] {
			l.low[i][j] = probInit
			l.mid[i][j] = probInit
		}
	}
	for i := range l.high {
		l.high[i] = probInit
	}
}

func (l *lenDec) decode(d *rangeDec, posState uint32) uint32 {
	if d.bit(&l.choice) == 0 {
		return d.tree(l.low[posState][:], lenLowBits)
	}
	if d.bit(&l.choice2) == 0 {
		return lenLowSyms + d.tree(l.mid[posState][:], lenMidBits)
	}
	return lenLowSyms + lenMidSyms + d.tree(l.high[:], lenHighBits)
}

// window es el diccionario: los últimos size bytes descomprimidos.
type window struct {
	buf  []byte
	pos  int
	full bool
	// total desde el último reinicio del diccionario (para pb y lp).
	total uint64
}

func (w *window) put(b byte) {
	w.buf[w.pos] = b
	w.pos++
	w.total++
	if w.pos == len(w.buf) {
		w.pos = 0
		w.full = true
	}
}

func (w *window) get(dist uint32) byte {
	i := w.pos - int(dist) - 1
	if i < 0 {
		i += len(w.buf)
	}
	return w.buf[i]
}

func (w *window) has(dist uint32) bool {
	return w.full || int(dist) < w.pos
}

type lzma2 struct {
	r     io.ByteReader
	win   window
	rc    rangeDec
	lc    int
	lp    int
	pb    int
	state uint32
	reps  [4]uint32

	isMatch    [numStates * posStatesMax]prob
	isRep      [numStates]prob
	isRepG0    [numStates]prob
	isRepG1    [numStates]prob
	isRepG2    [numStates]prob
	isRep0Long [numStates * posStatesMax]prob
	distSlot   [distStates][1 << distSlotBits]prob
	distSpec   [fullDistances - distModelEnd]prob
	align      [1 << alignBits]prob
	lenD, rep  lenDec
	literal    []prob

	needDictReset, needProps bool
	out                      []byte
}

func newLZMA2(r io.ByteReader, dict int) *lzma2 {
	if dict < 4096 {
		dict = 4096
	}
	return &lzma2{r: r, win: window{buf: make([]byte, dict)}, needDictReset: true, needProps: true}
}

func (z *lzma2) resetState() {
	z.state = 0
	z.reps = [4]uint32{}
	for _, a := range [][]prob{z.isMatch[:], z.isRep[:], z.isRepG0[:], z.isRepG1[:], z.isRepG2[:], z.isRep0Long[:], z.distSpec[:], z.align[:], z.literal} {
		for i := range a {
			a[i] = probInit
		}
	}
	for i := range z.distSlot {
		for j := range z.distSlot[i] {
			z.distSlot[i][j] = probInit
		}
	}
	z.lenD.reset()
	z.rep.reset()
}

func (z *lzma2) readByte() (byte, error) {
	b, err := z.r.ReadByte()
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return b, err
}

// chunk descomprime el siguiente trozo de LZMA2 y devuelve sus bytes (io.EOF
// en el marcador de fin).
func (z *lzma2) chunk() ([]byte, error) {
	ctl, err := z.readByte()
	if err != nil {
		return nil, err
	}
	if ctl == 0 {
		return nil, io.EOF
	}
	z.out = z.out[:0]
	if ctl == 1 || ctl == 2 {
		if ctl == 1 {
			z.dictReset()
		} else if z.needDictReset {
			return nil, errData
		}
		hi, err1 := z.readByte()
		lo, err2 := z.readByte()
		if err1 != nil || err2 != nil {
			return nil, io.ErrUnexpectedEOF
		}
		n := int(hi)<<8 | int(lo) + 1
		for i := 0; i < n; i++ {
			b, err := z.readByte()
			if err != nil {
				return nil, err
			}
			z.win.put(b)
			z.out = append(z.out, b)
		}
		return z.out, nil
	}
	if ctl < 0x80 {
		return nil, errData
	}
	var h [4]byte
	for i := range h {
		if h[i], err = z.readByte(); err != nil {
			return nil, err
		}
	}
	unpacked := (int(ctl&0x1F)<<16 | int(h[0])<<8 | int(h[1])) + 1
	packed := (int(h[2])<<8 | int(h[3])) + 1
	reset := (ctl >> 5) & 3
	if reset == 3 {
		z.dictReset()
	} else if z.needDictReset {
		return nil, errData
	}
	if reset >= 2 {
		p, err := z.readByte()
		if err != nil {
			return nil, err
		}
		if p >= 9*5*5 {
			return nil, errData
		}
		z.lc = int(p % 9)
		p /= 9
		z.lp = int(p % 5)
		z.pb = int(p / 5)
		if z.lc+z.lp > 4 {
			return nil, errData
		}
		z.literal = make([]prob, 0x300<<(z.lc+z.lp))
		z.needProps = false
	} else if z.needProps {
		return nil, errData
	}
	if reset >= 1 {
		z.resetState()
	}
	z.rc = rangeDec{r: z.r, limit: packed}
	z.rc.init()
	if err := z.decode(unpacked); err != nil {
		return nil, err
	}
	if z.rc.err != nil {
		return nil, z.rc.err
	}
	// El trozo tiene que acabar justo donde dice su cabecera.
	for z.rc.limit > 0 {
		z.rc.byte()
	}
	return z.out, nil
}

func (z *lzma2) dictReset() {
	z.win.pos, z.win.full, z.win.total = 0, false, 0
	z.needDictReset = false
}

func (z *lzma2) emit(b byte) {
	z.win.put(b)
	z.out = append(z.out, b)
}

func (z *lzma2) decode(n int) error {
	rc := &z.rc
	pbMask := uint32(1)<<z.pb - 1
	lpMask := uint32(1)<<z.lp - 1
	end := len(z.out) + n
	for len(z.out) < end {
		if rc.err != nil {
			return rc.err
		}
		posState := uint32(z.win.total) & pbMask
		st := z.state
		if rc.bit(&z.isMatch[st*posStatesMax+posState]) == 0 {
			prev := uint32(0)
			if z.win.total > 0 {
				prev = uint32(z.win.get(0))
			}
			lit := z.literal[0x300*((uint32(z.win.total)&lpMask)<<z.lc+prev>>(8-z.lc)):]
			sym := uint32(1)
			if st >= 7 {
				if !z.win.has(z.reps[0]) {
					return errData
				}
				match := uint32(z.win.get(z.reps[0]))
				for sym < 0x100 {
					mb := (match >> 7) & 1
					match <<= 1
					b := rc.bit(&lit[(1+mb)<<8+sym])
					sym = sym<<1 | b
					if mb != b {
						break
					}
				}
			}
			for sym < 0x100 {
				sym = sym<<1 | rc.bit(&lit[sym])
			}
			z.emit(byte(sym))
			switch {
			case st < 4:
				z.state = 0
			case st < 10:
				z.state = st - 3
			default:
				z.state = st - 6
			}
			continue
		}
		var length uint32
		if rc.bit(&z.isRep[st]) == 0 {
			z.reps[3], z.reps[2], z.reps[1] = z.reps[2], z.reps[1], z.reps[0]
			length = z.lenD.decode(rc, posState)
			if st < 7 {
				z.state = 7
			} else {
				z.state = 10
			}
			ls := length
			if ls > distStates-1 {
				ls = distStates - 1
			}
			slot := rc.tree(z.distSlot[ls][:], distSlotBits)
			dist := slot
			if slot >= 4 {
				nd := int(slot>>1) - 1
				dist = (2 | slot&1) << nd
				if slot < distModelEnd {
					dist += rc.reverse1(z.distSpec[dist-slot:], nd)
				} else {
					dist += rc.direct(nd-alignBits) << alignBits
					dist += rc.reverse(z.align[:], alignBits)
				}
			}
			if dist == 0xFFFFFFFF {
				return errData // marcador de fin: no vale dentro de LZMA2
			}
			z.reps[0] = dist
		} else {
			if rc.bit(&z.isRepG0[st]) == 0 {
				if rc.bit(&z.isRep0Long[st*posStatesMax+posState]) == 0 {
					if st < 7 {
						z.state = 9
					} else {
						z.state = 11
					}
					if !z.win.has(z.reps[0]) {
						return errData
					}
					z.emit(z.win.get(z.reps[0]))
					continue
				}
			} else {
				var dist uint32
				if rc.bit(&z.isRepG1[st]) == 0 {
					dist = z.reps[1]
				} else {
					if rc.bit(&z.isRepG2[st]) == 0 {
						dist = z.reps[2]
					} else {
						dist = z.reps[3]
						z.reps[3] = z.reps[2]
					}
					z.reps[2] = z.reps[1]
				}
				z.reps[1] = z.reps[0]
				z.reps[0] = dist
			}
			length = z.rep.decode(rc, posState)
			if st < 7 {
				z.state = 8
			} else {
				z.state = 11
			}
		}
		l := int(length) + matchMinLen
		if !z.win.has(z.reps[0]) {
			return errData
		}
		if len(z.out)+l > end {
			return errData
		}
		d := z.reps[0]
		for i := 0; i < l; i++ {
			z.emit(z.win.get(d))
		}
	}
	return rc.err
}
