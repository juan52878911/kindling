package chispa

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

// Formato del fichero .chispa (todo little-endian):
//
//	[8]  magia "\x89CHI\r\n\x1a\n" (como PNG: detecta transferencias en modo texto)
//	u16  versión del formato (2)
//	u16  banderas: bit0 = binario, bit1 = pesos dispersos
//	u32  longitud de la cabecera JSON (<= MaxHeaderBytes)
//	...  cabecera JSON: {spec, labels, meta}
//	u64  hash de la especificación de características
//	u32  cubos (debe coincidir con spec.buckets)
//	u32  salidas (1 en binario, nº de etiquetas si no)
//	f64  temperatura
//	f64  × salidas: escala por salida
//	f64  × salidas: sesgo por salida
//	f64  × etiquetas: umbral τ por etiqueta
//	pesos densos:    cubos × salidas × i16, ordenados por cubo
//	pesos dispersos: u32 filas; filas × (u32 cubo, salidas × i16), cubos crecientes
//	u32  CRC-32C de todo lo anterior
//
// La cabecera es JSON porque es lo que evoluciona (metadatos, métricas) y lo que
// una persona quiere leer; lo numérico va en binario para que sea exacto.

const (
	FormatVersion  = 2
	MaxLabels      = 256
	MaxLabelBytes  = 128
	MaxHeaderBytes = 1 << 20
	// MaxWeightBytes acota la tabla de pesos en memoria: un fichero hostil no
	// puede pedir más que esto aunque declare cubos y salidas enormes.
	MaxWeightBytes = 64 << 20
	MaxFileBytes   = MaxWeightBytes + MaxHeaderBytes + 1<<20

	flagBinary = 1 << 0
	flagSparse = 1 << 1
)

var magic = [8]byte{0x89, 'C', 'H', 'I', '\r', '\n', 0x1a, '\n'}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type header struct {
	Spec   FeatureSpec `json:"spec"`
	Labels []string    `json:"labels"`
	Meta   Meta        `json:"meta"`
}

// Marshal serializa el modelo. Elige pesos dispersos si ocupan menos: con
// pocos datos de entrenamiento la mayoría de los cubos quedan a cero.
func (m *Model) Marshal() ([]byte, error) {
	if err := m.Init(); err != nil {
		return nil, err
	}
	hdr, err := json.Marshal(header{Spec: m.Spec, Labels: m.Labels, Meta: m.Meta})
	if err != nil {
		return nil, err
	}
	if len(hdr) > MaxHeaderBytes {
		return nil, errors.New("header too large")
	}
	K, B := m.nOut, int(m.Spec.Buckets)
	rows := 0
	for b := 0; b < B; b++ {
		if !zeroRow(m.W[b*K : b*K+K]) {
			rows++
		}
	}
	sparse := 4+rows*(4+2*K) < B*K*2
	var flags uint16
	if m.Binary {
		flags |= flagBinary
	}
	if sparse {
		flags |= flagSparse
	}

	var buf bytes.Buffer
	le := binary.LittleEndian
	w := func(v any) { _ = binary.Write(&buf, le, v) } // bytes.Buffer no falla
	buf.Write(magic[:])
	w(uint16(FormatVersion))
	w(flags)
	w(uint32(len(hdr)))
	buf.Write(hdr)
	w(m.SpecHash)
	w(m.Spec.Buckets)
	w(uint32(K))
	w(m.Temperature)
	w(m.Scales)
	w(m.Bias)
	w(m.Thresholds)
	if sparse {
		w(uint32(rows))
		for b := 0; b < B; b++ {
			row := m.W[b*K : b*K+K]
			if !zeroRow(row) {
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

// Save escribe el modelo en path de forma atómica (temporal + rename): un
// servicio que recargue el modelo nunca ve un fichero a medias.
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

// LoadFile carga un .chispa del disco.
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

// Load lee y valida un modelo. Lee como mucho MaxFileBytes, comprueba el CRC
// antes de interpretar nada y valida cada longitud contra los topes ANTES de
// reservar: un fichero corrupto u hostil devuelve error, nunca pánico ni una
// reserva desmedida.
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

// Unmarshal interpreta un modelo ya leído. Ver Load.
func Unmarshal(data []byte) (*Model, error) {
	if len(data) < len(magic)+8+4 || !bytes.Equal(data[:len(magic)], magic[:]) {
		return nil, errors.New("not a Chispa model (bad magic)")
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.Checksum(body, crcTable) != sum {
		return nil, errors.New("model file is corrupt (checksum mismatch)")
	}
	rd := &reader{b: body[len(magic):]}
	ver, flags := rd.u16(), rd.u16()
	if rd.err == nil && ver != FormatVersion {
		return nil, fmt.Errorf("unsupported model format version %d (this build reads %d)", ver, FormatVersion)
	}
	if flags&^(flagBinary|flagSparse) != 0 {
		return nil, fmt.Errorf("unknown flags %#x", flags)
	}
	hlen := rd.u32()
	if hlen > MaxHeaderBytes {
		return nil, errors.New("header too large")
	}
	var h header
	if raw := rd.bytes(int(hlen)); rd.err == nil {
		dec := json.NewDecoder(bytes.NewReader(raw))
		if err := dec.Decode(&h); err != nil {
			return nil, fmt.Errorf("bad header: %w", err)
		}
	}
	if rd.err != nil {
		return nil, rd.err
	}
	if err := h.Spec.Validate(); err != nil {
		return nil, err
	}
	if len(h.Labels) < 2 || len(h.Labels) > MaxLabels {
		return nil, fmt.Errorf("need between 2 and %d labels, got %d", MaxLabels, len(h.Labels))
	}
	m := &Model{Spec: h.Spec, Labels: h.Labels, Meta: h.Meta, Binary: flags&flagBinary != 0}
	m.SpecHash = rd.u64()
	buckets, K := rd.u32(), int(rd.u32())
	if rd.err != nil {
		return nil, rd.err
	}
	want := len(h.Labels)
	if m.Binary {
		want = 1
	}
	if buckets != h.Spec.Buckets || K != want {
		return nil, errors.New("buckets/outputs in body do not match header")
	}
	if int64(buckets)*int64(K)*2 > MaxWeightBytes {
		return nil, fmt.Errorf("weight table larger than %d bytes", MaxWeightBytes)
	}
	m.Temperature = rd.f64()
	m.Scales = rd.f64s(K)
	m.Bias = rd.f64s(K)
	m.Thresholds = rd.f64s(len(h.Labels))
	if rd.err != nil {
		return nil, rd.err
	}
	B := int(buckets)
	if flags&flagSparse != 0 {
		rows := int(rd.u32())
		// Cada fila ocupa 4+2K bytes: si no caben en lo que queda, mienten.
		if rd.err != nil || rows > B || rows*(4+2*K) != len(rd.b) {
			return nil, errors.New("sparse weights: bad row count")
		}
		m.W = make([]int16, B*K)
		prev := -1
		for i := 0; i < rows; i++ {
			b := int(rd.u32())
			if b <= prev || b >= B {
				return nil, errors.New("sparse weights: bucket out of order or range")
			}
			prev = b
			rd.i16s(m.W[b*K : b*K+K])
		}
	} else {
		if len(rd.b) != B*K*2 {
			return nil, errors.New("dense weights: wrong size")
		}
		m.W = make([]int16, B*K)
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

// reader es un lector de bytes que recuerda el primer error: si falta algo,
// todas las lecturas siguientes devuelven cero y el llamador mira err una vez.
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

func (r *reader) bytes(n int) []byte { return r.take(n) }

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

func (r *reader) f64() float64 { return math.Float64frombits(r.u64()) }

// f64s lee n float64 comprobando antes que caben: n viene del propio fichero.
func (r *reader) f64s(n int) []float64 {
	if r.err != nil || n < 0 || n*8 > len(r.b) {
		r.err = errShort
		return nil
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = r.f64()
	}
	return out
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
