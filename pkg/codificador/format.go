package codificador

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
)

// Formato del fichero .jenc, hermano del .jev (little-endian):
//
//	[8]  magia "\x89JEC\r\n\x1a\n"
//	u16  versión (1)            u16 banderas (0)
//	u32  longitud de la cabecera JSON {labels, meta, dim, hidden}
//	f32 × dim  media           f32 × dim  1/desviación
//	si hidden > 0:  f64 S1, i16 × hidden × dim, f64 × hidden
//	f64 S2, i16 × etiquetas × (hidden o dim), f64 × etiquetas
//	f64 temperatura            f64 × etiquetas: umbral τ
//	u32  CRC-32C de todo lo anterior
//
// El cargador lee como mucho MaxFileBytes, comprueba el CRC antes de
// interpretar nada y valida cada tamaño contra su tope ANTES de reservar.

const (
	FormatVersion  = 1
	MaxFileBytes   = 64 << 20
	maxHeaderBytes = 1 << 20
	maxHidden      = 4096
)

var headMagic = [8]byte{0x89, 'J', 'E', 'C', '\r', '\n', 0x1a, '\n'}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type fileHeader struct {
	Labels []string `json:"labels"`
	Meta   Meta     `json:"meta"`
	Dim    int      `json:"dim"`
	Hidden int      `json:"hidden"`
}

// Marshal serializa la cabeza.
func (h *Head) Marshal() ([]byte, error) {
	if err := h.Validate(); err != nil {
		return nil, err
	}
	hdr, err := json.Marshal(fileHeader{Labels: h.Labels, Meta: h.Meta, Dim: h.Dim, Hidden: h.Hidden})
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	b.Write(headMagic[:])
	w(uint16(FormatVersion))
	w(uint16(0))
	w(uint32(len(hdr)))
	b.Write(hdr)
	w(h.Mean)
	w(h.InvStd)
	if h.Hidden > 0 {
		w(h.S1)
		w(h.W1)
		w(h.B1)
	}
	w(h.S2)
	w(h.W2)
	w(h.B2)
	w(h.Temperature)
	w(h.Thresholds)
	w(crc32.Checksum(b.Bytes(), crcTable))
	return b.Bytes(), nil
}

// Save escribe la cabeza (fichero temporal y rename).
func (h *Head) Save(path string) error {
	b, err := h.Marshal()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// LoadFile lee una cabeza de disco.
func LoadFile(path string) (*Head, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	h, err := Load(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return h, nil
}

// Load lee y valida una cabeza (como mucho MaxFileBytes).
func Load(r io.Reader) (*Head, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("head file larger than %d bytes", MaxFileBytes)
	}
	return Unmarshal(data)
}

// Unmarshal interpreta una cabeza ya leída. Ver Load.
func Unmarshal(data []byte) (*Head, error) {
	if len(data) < len(headMagic)+12 || !bytes.Equal(data[:8], headMagic[:]) {
		return nil, errors.New("not an encoder head (bad magic)")
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.Checksum(body, crcTable) != sum {
		return nil, errors.New("head file is corrupt (checksum mismatch)")
	}
	rd := &reader{b: body[8:]}
	ver, flags := rd.u16(), rd.u16()
	if rd.err == nil && (ver != FormatVersion || flags != 0) {
		return nil, fmt.Errorf("unsupported head format version %d / flags %#x (this build reads %d)", ver, flags, FormatVersion)
	}
	hlen := rd.u32()
	if hlen > maxHeaderBytes {
		return nil, errors.New("header too large")
	}
	var fh fileHeader
	if raw := rd.take(int(hlen)); rd.err == nil {
		if err := json.Unmarshal(raw, &fh); err != nil {
			return nil, fmt.Errorf("bad header: %w", err)
		}
	}
	if rd.err != nil {
		return nil, rd.err
	}
	K, D, H := len(fh.Labels), fh.Dim, fh.Hidden
	if K < 2 || K > 256 || D < 1 || D > MaxDim || H < 0 || H > maxHidden {
		return nil, fmt.Errorf("bad sizes (labels %d, dim %d, hidden %d)", K, D, H)
	}
	in := D
	if H > 0 {
		in = H
	}
	// Lo que el cuerpo tiene que medir, calculado antes de reservar nada.
	need := 8*D + 8 + 2*K*in + 8*K + 8 + 8*K
	if H > 0 {
		need += 8 + 2*H*D + 8*H
	}
	if need != len(rd.b) {
		return nil, errors.New("head body does not match its declared sizes")
	}
	h := &Head{Labels: fh.Labels, Meta: fh.Meta, Dim: D, Hidden: H}
	h.Mean, h.InvStd = rd.f32s(D), rd.f32s(D)
	if H > 0 {
		h.S1 = rd.f64()
		h.W1 = rd.i16s(H * D)
		h.B1 = rd.f64s(H)
	}
	h.S2 = rd.f64()
	h.W2 = rd.i16s(K * in)
	h.B2 = rd.f64s(K)
	h.Temperature = rd.f64()
	h.Thresholds = rd.f64s(K)
	if rd.err != nil {
		return nil, rd.err
	}
	if err := h.Validate(); err != nil {
		return nil, err
	}
	return h, nil
}

// reader recuerda el primer error: si falta algo, las lecturas siguientes
// devuelven cero y quien llama mira err una vez.
type reader struct {
	b   []byte
	err error
}

var errShort = errors.New("file truncated")

func (r *reader) take(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		if r.err == nil {
			r.err = errShort
		}
		return nil
	}
	out := r.b[:n]
	r.b = r.b[n:]
	return out
}

func (r *reader) u16() uint16 {
	if b := r.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (r *reader) u32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *reader) f64() float64 {
	if b := r.take(8); b != nil {
		return math.Float64frombits(binary.LittleEndian.Uint64(b))
	}
	return 0
}

func (r *reader) f64s(n int) []float64 {
	b := r.take(8 * n)
	if b == nil {
		return nil
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[8*i:]))
	}
	return out
}

func (r *reader) f32s(n int) []float32 {
	b := r.take(4 * n)
	if b == nil {
		return nil
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

func (r *reader) i16s(n int) []int16 {
	b := r.take(2 * n)
	if b == nil {
		return nil
	}
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out
}
