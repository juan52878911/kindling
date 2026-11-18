package xz

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"runtime"
	"strings"
	"testing"
)

// Regresiones de entradas hostiles: cada una tiene que acabar en error (o
// en EOF) sin pánico y sin reservar más memoria de la cuenta.

// streamHeader es la cabecera de un flujo sin comprobación por bloque.
func streamHeader() []byte {
	h := []byte{0xFD, '7', 'z', 'X', 'Z', 0, 0, 0}
	return binary.LittleEndian.AppendUint32(h, crc32.ChecksumIEEE(h[6:8]))
}

// blockHeader arma una cabecera de bloque con el CRC bien puesto a partir
// de los bytes que van entre el byte de tamaño y el relleno.
func blockHeader(body []byte) []byte {
	n := 1 + len(body) + 4
	n = (n + 3) &^ 3
	h := make([]byte, n-4)
	h[0] = byte(n/4 - 1)
	copy(h[1:], body)
	return binary.LittleEndian.AppendUint32(h, crc32.ChecksumIEEE(h))
}

func varint(v uint64) []byte {
	var b []byte
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func decodeAll(b []byte) error {
	r, err := NewReader(bytes.NewReader(b))
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, r)
	return err
}

// El tamaño de las propiedades del filtro es un varint de 63 bits: no se
// puede usar para reservar antes de validarlo.
func TestHugeFilterProps(t *testing.T) {
	body := append([]byte{0x00, 0x21}, varint(1<<62)...)
	b := append(streamHeader(), blockHeader(body)...)
	if err := decodeAll(b); err == nil {
		t.Fatal("block with huge filter properties accepted")
	}
}

func TestDictTooLarge(t *testing.T) {
	// prop 30: 2<<26 = 128 MiB, por encima de MaxDict.
	b := append(streamHeader(), blockHeader([]byte{0x00, 0x21, 0x01, 30})...)
	err := decodeAll(b)
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("128 MiB dictionary: %v", err)
	}
}

// Bloques vacíos que declaran el diccionario máximo: la ventana no puede
// reservarse entera en cada uno.
func TestEmptyBlocksBigDict(t *testing.T) {
	b := streamHeader()
	for i := 0; i < 64; i++ {
		// prop 28: 2<<25 = 64 MiB; datos LZMA2: solo el fin (0x00) y relleno.
		b = append(b, blockHeader([]byte{0x00, 0x21, 0x01, 28})...)
		b = append(b, 0, 0, 0, 0)
	}
	b = append(b, 0) // índice
	var m0, m1 runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&m0)
	if err := decodeAll(b); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	if got := m1.TotalAlloc - m0.TotalAlloc; got > 16<<20 {
		t.Fatalf("64 empty blocks allocated %d MiB", got>>20)
	}
}

// Con la ventana llena, una distancia mayor que el diccionario no está
// dentro: antes has() decía que sí y get() se salía del búfer.
func TestWindowDistanceBeyondDict(t *testing.T) {
	w := window{size: 4096}
	for i := 0; i < 5000; i++ {
		w.put(byte(i))
	}
	if !w.full {
		t.Fatal("window not full")
	}
	if !w.has(4095) {
		t.Fatal("distance 4095 should be inside a full 4096-byte window")
	}
	if w.has(4096) || w.has(1<<20) {
		t.Fatal("distance beyond the dictionary accepted")
	}
	// Lo último escrito fue 4999.
	if got := w.get(0); got != byte(4999%256) {
		t.Fatalf("get(0) = %d", got)
	}
	if got := w.get(4095); got != byte((5000-4096)%256) {
		t.Fatalf("get(4095) = %d", got)
	}
}
