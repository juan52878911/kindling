// Package zstd descomprime flujos Zstandard (RFC 8878), lo justo para leer
// las capas tar+zstd de las imágenes OCI sin dependencias fuera de la
// biblioteca estándar.
//
// Solo descomprime y no admite diccionarios (las capas no los usan). Lee
// varios marcos seguidos y salta los marcos saltables. La memoria está
// acotada: la ventana que declara un marco no puede pasar de MaxWindow
// (128 MiB, la de zstd --long), y lo que se guarda de historia nunca pasa
// de vez y media la ventana más un bloque. Una entrada mala da un error, no
// un pánico ni un bucle: cada longitud y cada desplazamiento se comprueban
// antes de usarlos. Si el marco lleva su xxhash64, se comprueba al final;
// de todos modos, quien llama ya verificó el sha256 de la capa entera.
package zstd

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxWindow es la ventana más grande que se acepta.
const MaxWindow = 128 << 20

const (
	maxBlock      = 128 << 10 // Block_Maximum_Size
	magic         = 0xFD2FB528
	skippableMask = 0xFFFFFFF0
	skippable     = 0x184D2A50
)

var errCorrupt = errors.New("zstd: corrupt input")

// Reader descomprime un flujo zstd.
type Reader struct {
	in  *bufio.Reader
	err error
	out []byte // salida pendiente de entregar (un trozo de hist)

	// hist guarda la salida del marco: los últimos window bytes, como poco,
	// a los que pueden apuntar las secuencias.
	hist   []byte
	window int

	inFrame  bool
	sawFrame bool
	check    bool
	hasSize  bool
	size     uint64 // Frame_Content_Size
	produced uint64
	xx       xxh64

	// Estado de entropía que se hereda de un bloque al siguiente del marco.
	reps       [3]int
	huf        huffTable
	hufOK      bool
	ll, of, ml *fseTable   // las tablas en uso (modo repetir)
	own        [3]fseTable // las de los modos FSE y RLE, por tipo
	blk        []byte      // el bloque comprimido
	lit        []byte      // los literales descomprimidos
	norm       [maxSyms]int16
}

// NewReader devuelve un lector que descomprime r. No lee nada hasta el
// primer Read.
func NewReader(r io.Reader) *Reader {
	return &Reader{in: bufio.NewReaderSize(r, 1<<17)}
}

func (z *Reader) Read(p []byte) (int, error) {
	for len(z.out) == 0 {
		if z.err != nil {
			return 0, z.err
		}
		z.err = z.step()
	}
	n := copy(p, z.out)
	z.out = z.out[n:]
	return n, nil
}

// step lee la cabecera de un marco o un bloque. Un fin de la entrada que no
// cae entre dos marcos es io.ErrUnexpectedEOF.
func (z *Reader) step() error {
	var err error
	if z.inFrame {
		err = z.block()
	} else {
		err = z.frame()
	}
	if err == io.EOF && (z.inFrame || !z.sawFrame) {
		err = io.ErrUnexpectedEOF
	}
	return err
}

func (z *Reader) frame() error {
	var b [4]byte
	n, err := io.ReadFull(z.in, b[:])
	if n == 0 && err == io.EOF {
		return io.EOF
	}
	if err != nil {
		return io.ErrUnexpectedEOF
	}
	m := binary.LittleEndian.Uint32(b[:])
	if m&skippableMask == skippable {
		if _, err := io.ReadFull(z.in, b[:]); err != nil {
			return io.ErrUnexpectedEOF
		}
		sz := int64(binary.LittleEndian.Uint32(b[:]))
		if n, _ := io.CopyN(io.Discard, z.in, sz); n != sz {
			return io.ErrUnexpectedEOF
		}
		z.sawFrame = true
		return nil
	}
	if m != magic {
		return errors.New("zstd: invalid magic number")
	}
	fhd, err := z.in.ReadByte()
	if err != nil {
		return io.ErrUnexpectedEOF
	}
	if fhd&0x08 != 0 {
		return errCorrupt
	}
	single := fhd&0x20 != 0
	var window uint64
	if !single {
		wd, err := z.in.ReadByte()
		if err != nil {
			return io.ErrUnexpectedEOF
		}
		base := uint64(1) << (10 + wd>>3)
		window = base + base/8*uint64(wd&7)
	}
	var h [8]byte
	if dl := [4]int{0, 1, 2, 4}[fhd&3]; dl > 0 {
		if _, err := io.ReadFull(z.in, h[:dl]); err != nil {
			return io.ErrUnexpectedEOF
		}
		if binary.LittleEndian.Uint32(h[:4]) != 0 {
			return errors.New("zstd: dictionaries are not supported")
		}
	}
	fl := [4]int{0, 2, 4, 8}[fhd>>6]
	if fl == 0 && single {
		fl = 1
	}
	z.hasSize = fl > 0
	if fl > 0 {
		h = [8]byte{}
		if _, err := io.ReadFull(z.in, h[:fl]); err != nil {
			return io.ErrUnexpectedEOF
		}
		z.size = binary.LittleEndian.Uint64(h[:])
		if fl == 2 {
			z.size += 256
		}
	}
	if single {
		window = z.size
	}
	if window > MaxWindow {
		return fmt.Errorf("zstd: window of %d MiB is over the %d MiB limit", window>>20, MaxWindow>>20)
	}
	z.window = int(window)
	z.check = fhd&0x04 != 0
	z.xx.reset()
	z.produced = 0
	z.hist = z.hist[:0]
	z.reps = [3]int{1, 4, 8}
	z.hufOK = false
	z.ll, z.of, z.ml = nil, nil, nil
	z.inFrame, z.sawFrame = true, true
	return nil
}

// room deja sitio para un bloque más en hist. La historia más allá de la
// ventana no la puede usar nadie: cuando sobra más de media ventana (o de 1
// MiB), se descarta moviendo la ventana al principio.
func (z *Reader) room() {
	if len(z.hist)+maxBlock <= cap(z.hist) {
		return
	}
	extra := max(z.window/2, 1<<20)
	if len(z.hist) > z.window+extra {
		n := copy(z.hist, z.hist[len(z.hist)-z.window:])
		z.hist = z.hist[:n]
	}
	if need := len(z.hist) + maxBlock; need > cap(z.hist) {
		c := min(max(2*cap(z.hist), need), z.window+extra+maxBlock)
		h := make([]byte, len(z.hist), max(c, need))
		copy(h, z.hist)
		z.hist = h
	}
}

func (z *Reader) block() error {
	var h [3]byte
	if _, err := io.ReadFull(z.in, h[:]); err != nil {
		return io.ErrUnexpectedEOF
	}
	v := uint32(h[0]) | uint32(h[1])<<8 | uint32(h[2])<<16
	last := v&1 != 0
	size := int(v >> 3)
	if size > maxBlock {
		return errCorrupt
	}
	z.room()
	start := len(z.hist)
	switch (v >> 1) & 3 {
	case 0: // raw
		z.hist = z.hist[:start+size]
		if _, err := io.ReadFull(z.in, z.hist[start:]); err != nil {
			return io.ErrUnexpectedEOF
		}
	case 1: // RLE
		c, err := z.in.ReadByte()
		if err != nil {
			return io.ErrUnexpectedEOF
		}
		z.hist = z.hist[:start+size]
		for i := start; i < len(z.hist); i++ {
			z.hist[i] = c
		}
	case 2:
		if z.blk == nil {
			z.blk, z.lit = make([]byte, maxBlock), make([]byte, maxBlock)
		}
		b := z.blk[:size]
		if _, err := io.ReadFull(z.in, b); err != nil {
			return io.ErrUnexpectedEOF
		}
		if err := z.compressed(b); err != nil {
			return err
		}
	default:
		return errCorrupt
	}
	out := z.hist[start:]
	z.produced += uint64(len(out))
	if z.hasSize && z.produced > z.size {
		return errors.New("zstd: frame is longer than its declared size")
	}
	if z.check {
		z.xx.write(out)
	}
	z.out = out
	if !last {
		return nil
	}
	z.inFrame = false
	if z.hasSize && z.produced != z.size {
		return errors.New("zstd: frame is shorter than its declared size")
	}
	if z.check {
		var c [4]byte
		if _, err := io.ReadFull(z.in, c[:]); err != nil {
			return io.ErrUnexpectedEOF
		}
		if binary.LittleEndian.Uint32(c[:]) != uint32(z.xx.sum()) {
			return errors.New("zstd: checksum mismatch")
		}
	}
	return nil
}
