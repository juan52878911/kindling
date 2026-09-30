// Package xz descomprime ficheros .xz (LZMA2), lo justo para leer paquetes
// .deb de Debian (data.tar.xz y control.tar.xz) sin dependencias fuera de la
// biblioteca estándar.
//
// Solo descomprime, y solo el filtro LZMA2 (sin BCJ ni delta), que es lo que
// escribe dpkg-deb. Comprueba el CRC32, CRC64 o SHA-256 de cada bloque: un
// error del descompresor sale como fallo de la comprobación, no como datos
// malos. De todos modos, quien llama ya verificó el sha256 del paquete
// entero antes de abrirlo.
package xz

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
)

var (
	errFormat = errors.New("xz: invalid format")
	crc64Tab  = crc64.MakeTable(crc64.ECMA)
)

// Reader descomprime un flujo .xz.
type Reader struct {
	r       *bufio.Reader
	check   byte
	block   *blockReader
	done    bool
	started bool
}

// NewReader lee la cabecera del flujo.
func NewReader(r io.Reader) (*Reader, error) {
	x := &Reader{r: bufio.NewReaderSize(r, 1<<16)}
	h := make([]byte, 12)
	if _, err := io.ReadFull(x.r, h); err != nil {
		return nil, err
	}
	if !bytes.Equal(h[:6], []byte{0xFD, '7', 'z', 'X', 'Z', 0}) {
		return nil, errFormat
	}
	if h[6] != 0 || h[7]&0xF0 != 0 || crc32.ChecksumIEEE(h[6:8]) != binary.LittleEndian.Uint32(h[8:]) {
		return nil, errFormat
	}
	x.check = h[7] & 0x0F
	switch x.check {
	case 0, 1, 4, 10:
	default:
		return nil, fmt.Errorf("xz: unsupported check type %d", x.check)
	}
	return x, nil
}

func (x *Reader) Read(p []byte) (int, error) {
	for {
		if x.done {
			return 0, io.EOF
		}
		if x.block == nil {
			b, err := x.r.ReadByte()
			if err != nil {
				return 0, io.ErrUnexpectedEOF
			}
			if b == 0 {
				// Índice: fin de los bloques. El índice y el pie no se
				// comprueban (cada bloque ya lo hizo).
				x.done = true
				return 0, io.EOF
			}
			blk, err := x.newBlock(b)
			if err != nil {
				return 0, err
			}
			x.block = blk
		}
		n, err := x.block.Read(p)
		if err == io.EOF {
			if err := x.block.finish(); err != nil {
				return n, err
			}
			x.block = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

type blockReader struct {
	x    *Reader
	dec  *lzma2
	h    hash.Hash
	out  []byte
	eof  bool
	n    int64
	comp *countReader
}

type countReader struct {
	r *bufio.Reader
	n int64
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func readVarint(r io.ByteReader) (uint64, error) {
	var v uint64
	for i := 0; i < 9; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint64(b&0x7F) << (7 * i)
		if b&0x80 == 0 {
			return v, nil
		}
	}
	return 0, errFormat
}

func (x *Reader) newBlock(first byte) (*blockReader, error) {
	size := (int(first) + 1) * 4
	h := make([]byte, size)
	h[0] = first
	if _, err := io.ReadFull(x.r, h[1:]); err != nil {
		return nil, err
	}
	if crc32.ChecksumIEEE(h[:size-4]) != binary.LittleEndian.Uint32(h[size-4:]) {
		return nil, errors.New("xz: block header CRC mismatch")
	}
	br := bytes.NewReader(h[1 : size-4])
	flags, _ := br.ReadByte()
	nf := int(flags&3) + 1
	if flags&0x3C != 0 {
		return nil, errFormat
	}
	if flags&0x40 != 0 {
		if _, err := readVarint(br); err != nil {
			return nil, err
		}
	}
	if flags&0x80 != 0 {
		if _, err := readVarint(br); err != nil {
			return nil, err
		}
	}
	var dict uint32
	for i := 0; i < nf; i++ {
		id, err := readVarint(br)
		if err != nil {
			return nil, err
		}
		psize, err := readVarint(br)
		if err != nil {
			return nil, err
		}
		props := make([]byte, psize)
		if _, err := io.ReadFull(br, props); err != nil {
			return nil, err
		}
		if id != 0x21 || i != nf-1 || psize != 1 {
			return nil, fmt.Errorf("xz: unsupported filter %#x (only LZMA2)", id)
		}
		b := props[0] & 0x3F
		if b > 40 {
			return nil, errFormat
		}
		if b == 40 {
			dict = 0xFFFFFFFF
		} else {
			dict = (2 | uint32(b)&1) << (b/2 + 11)
		}
	}
	var hh hash.Hash
	switch x.check {
	case 1:
		hh = crc32.NewIEEE()
	case 4:
		hh = crc64.New(crc64Tab)
	case 10:
		hh = sha256.New()
	}
	cr := &countReader{r: x.r}
	if dict > 1<<28 {
		return nil, fmt.Errorf("xz: dictionary of %d bytes is too large", dict)
	}
	return &blockReader{x: x, dec: newLZMA2(cr, int(dict)), h: hh, comp: cr}, nil
}

func (b *blockReader) Read(p []byte) (int, error) {
	for len(b.out) == 0 {
		if b.eof {
			return 0, io.EOF
		}
		out, err := b.dec.chunk()
		if err == io.EOF {
			b.eof = true
			continue
		}
		if err != nil {
			return 0, err
		}
		b.out = out
	}
	n := copy(p, b.out)
	if b.h != nil {
		b.h.Write(b.out[:n])
	}
	b.out = b.out[n:]
	b.n += int64(n)
	return n, nil
}

// finish lee el relleno y la comprobación del bloque.
func (b *blockReader) finish() error {
	for b.comp.n%4 != 0 {
		c, err := b.comp.ReadByte()
		if err != nil {
			return err
		}
		if c != 0 {
			return errFormat
		}
	}
	var want []byte
	switch b.x.check {
	case 1:
		want = make([]byte, 4)
	case 4:
		want = make([]byte, 8)
	case 10:
		want = make([]byte, 32)
	}
	if len(want) == 0 {
		return nil
	}
	if _, err := io.ReadFull(b.x.r, want); err != nil {
		return err
	}
	got := b.h.Sum(nil)
	if b.x.check == 1 || b.x.check == 4 {
		// CRC32 y CRC64 van en little endian; Sum los da en big endian.
		for i, j := 0, len(got)-1; i < j; i, j = i+1, j-1 {
			got[i], got[j] = got[j], got[i]
		}
	}
	if !bytes.Equal(got, want) {
		return errors.New("xz: block check mismatch (corrupt data or decoder bug)")
	}
	return nil
}
