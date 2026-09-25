package slots

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

// Formato del fichero .jevs (little-endian), hermano del .jev:
//
//	[8]  magia "\x89JVS\r\n\x1a\n"
//	u16  versión (1)
//	u16  banderas: bit1 = pesos dispersos
//	u32  longitud de la cabecera JSON (<= MaxHeaderBytes)
//	...  cabecera JSON: {spec, tags, lexicon, meta}
//	u64  hash de spec + etiquetas + léxico
//	u32  cubos (= spec.buckets)
//	u32  etiquetas (= len(tags))
//	f64  escala
//	i16  × (etiquetas+1) × etiquetas: transiciones (fila 0 = inicio)
//	pesos densos:    cubos × etiquetas × i16
//	pesos dispersos: u32 filas; filas × (u32 cubo, etiquetas × i16), cubos crecientes
//	u32  CRC-32C de todo lo anterior
//
// El léxico va en la cabecera porque es parte de la extracción: sin él las
// características «l0=area» no significan nada.

const (
	FormatVersion  = 1
	MaxHeaderBytes = 4 << 20
	MaxWeightBytes = 64 << 20
	MaxFileBytes   = MaxWeightBytes + MaxHeaderBytes + 1<<20
	flagSparse     = 1 << 1
)

var magic = [8]byte{0x89, 'J', 'V', 'S', '\r', '\n', 0x1a, '\n'}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type header struct {
	Spec    Spec              `json:"spec"`
	Tags    []string          `json:"tags"`
	Lexicon map[string]string `json:"lexicon,omitempty"`
	Meta    Meta              `json:"meta"`
}

// Marshal serializa el modelo (disperso si ocupa menos).
func (m *Model) Marshal() ([]byte, error) {
	if err := m.Init(); err != nil {
		return nil, err
	}
	hdr, err := json.Marshal(header{Spec: m.Spec, Tags: m.Tags, Lexicon: m.Lexicon, Meta: m.Meta})
	if err != nil {
		return nil, err
	}
	if len(hdr) > MaxHeaderBytes {
		return nil, errors.New("header too large")
	}
	T, B := m.nT, int(m.Spec.Buckets)
	rows := 0
	for b := 0; b < B; b++ {
		if !zeroRow(m.W[b*T : b*T+T]) {
			rows++
		}
	}
	sparse := 4+rows*(4+2*T) < B*T*2
	var flags uint16
	if sparse {
		flags |= flagSparse
	}
	var buf bytes.Buffer
	w := func(v any) { _ = binary.Write(&buf, binary.LittleEndian, v) }
	buf.Write(magic[:])
	w(uint16(FormatVersion))
	w(flags)
	w(uint32(len(hdr)))
	buf.Write(hdr)
	w(m.SpecHash)
	w(m.Spec.Buckets)
	w(uint32(T))
	w(m.Scale)
	w(m.Trans)
	if sparse {
		w(uint32(rows))
		for b := 0; b < B; b++ {
			if row := m.W[b*T : b*T+T]; !zeroRow(row) {
				w(uint32(b))
				w(row)
			}
		}
	} else {
		w(m.W)
	}
	w(crc32.Checksum(buf.Bytes(), crcTable))
	return buf.Bytes(), nil
}

func zeroRow(r []int16) bool {
	for _, v := range r {
		if v != 0 {
			return false
		}
	}
	return true
}

// Save escribe de forma atómica (temporal + rename).
func (m *Model) Save(path string) error {
	b, err := m.Marshal()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadFile carga un .jevs.
func LoadFile(path string) (*Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	m, err := Load(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Load lee como mucho MaxFileBytes y valida (ver Unmarshal).
func Load(r io.Reader) (*Model, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxFileBytes {
		return nil, fmt.Errorf("model file larger than %d bytes", MaxFileBytes)
	}
	return Unmarshal(data)
}

// Unmarshal comprueba el CRC antes de interpretar nada y cada longitud contra
// los topes ANTES de reservar: un fichero hostil da error, no pánico.
func Unmarshal(data []byte) (*Model, error) {
	if len(data) < len(magic)+8+4 || !bytes.Equal(data[:len(magic)], magic[:]) {
		return nil, errors.New("not a JEV slots model (bad magic)")
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.Checksum(body, crcTable) != sum {
		return nil, errors.New("model file is corrupt (checksum mismatch)")
	}
	rd := &reader{b: body[len(magic):]}
	ver, flags := rd.u16(), rd.u16()
	if rd.err == nil && ver != FormatVersion {
		return nil, fmt.Errorf("unsupported slots format version %d (this build reads %d)", ver, FormatVersion)
	}
	if flags&^uint16(flagSparse) != 0 {
		return nil, fmt.Errorf("unknown flags %#x", flags)
	}
	hlen := rd.u32()
	if hlen > MaxHeaderBytes {
		return nil, errors.New("header too large")
	}
	var h header
	if raw := rd.take(int(hlen)); rd.err == nil {
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("bad header: %w", err)
		}
	}
	if rd.err != nil {
		return nil, rd.err
	}
	if err := h.Spec.Validate(); err != nil {
		return nil, err
	}
	if err := validateTags(h.Tags); err != nil {
		return nil, err
	}
	m := &Model{Spec: h.Spec, Tags: h.Tags, Lexicon: h.Lexicon, Meta: h.Meta}
	m.SpecHash = rd.u64()
	buckets, T := rd.u32(), int(rd.u32())
	if rd.err != nil {
		return nil, rd.err
	}
	if buckets != h.Spec.Buckets || T != len(h.Tags) {
		return nil, errors.New("buckets/tags in body do not match header")
	}
	if int64(buckets)*int64(T)*2 > MaxWeightBytes {
		return nil, fmt.Errorf("weight table larger than %d bytes", MaxWeightBytes)
	}
	m.Scale = math.Float64frombits(rd.u64())
	m.Trans = make([]int16, (T+1)*T) // T <= MaxTags: tope pequeño
	rd.i16s(m.Trans)
	if rd.err != nil {
		return nil, rd.err
	}
	B := int(buckets)
	if flags&flagSparse != 0 {
		rows := int(rd.u32())
		if rd.err != nil || rows > B || rows*(4+2*T) != len(rd.b) {
			return nil, errors.New("sparse weights: bad row count")
		}
		m.W = make([]int16, B*T)
		prev := -1
		for i := 0; i < rows; i++ {
			b := int(rd.u32())
			if b <= prev || b >= B {
				return nil, errors.New("sparse weights: bucket out of order or range")
			}
			prev = b
			rd.i16s(m.W[b*T : b*T+T])
		}
	} else {
		if len(rd.b) != B*T*2 {
			return nil, errors.New("dense weights: wrong size")
		}
		m.W = make([]int16, B*T)
		rd.i16s(m.W)
	}
	if rd.err != nil {
		return nil, rd.err
	}
	if len(rd.b) != 0 {
		return nil, errors.New("trailing bytes after weights")
	}
	if err := m.Init(); err != nil {
		return nil, err
	}
	return m, nil
}

type reader struct {
	b   []byte
	err error
}

var errShort = errors.New("model file truncated")

func (r *reader) take(n int) []byte {
	if r.err != nil || n < 0 || n > len(r.b) {
		r.err = errShort
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

func (r *reader) u64() uint64 {
	if b := r.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (r *reader) i16s(dst []int16) {
	b := r.take(2 * len(dst))
	if b == nil {
		return
	}
	for i := range dst {
		dst[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
}
