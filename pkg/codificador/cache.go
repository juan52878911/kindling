package codificador

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"sort"
	"sync"
)

// CACHÉ DE EMBEDDINGS (.jemb).
//
// Entrenar y evaluar la cabeza pasa decenas de miles de frases por el
// codificador. Hacerlo una vez y guardar los vectores tiene dos ventajas: la
// evaluación se repite sin microVM en marcha, y es reproducible (los mismos
// vectores dan las mismas cifras, aunque llama.cpp elija otra variante de CPU
// en otra máquina y cambie el último bit).
//
// Formato (little-endian):
//
//	[8]  magia "\x89JEB\r\n\x1a\n"
//	u16  versión (1)            u16 banderas (0)
//	u32  longitud de la cabecera JSON {model, dim}
//	u32  entradas
//	entradas × (u32 bytes del texto, texto, dim × f32)
//	u32  CRC-32C de todo lo anterior
//
// La clave es el texto EXACTO que se mandó al codificador (con su prefijo).

var cacheMagic = [8]byte{0x89, 'J', 'E', 'B', '\r', '\n', 0x1a, '\n'}

// Topes del fichero de caché: es un artefacto de desarrollo, pero se lee con
// los mismos cuidados que un modelo.
const (
	MaxCacheBytes   = 1 << 30
	maxCacheEntries = 4 << 20
)

// CacheHeader dice de qué codificador son los vectores.
type CacheHeader struct {
	Model string `json:"model"` // p. ej. multilingual-e5-small:q8_0
	// SHA256 del GGUF: dos conversiones distintas del mismo modelo no dan los
	// mismos vectores.
	SHA256 string `json:"sha256,omitempty"`
	Dim    int    `json:"dim"`
}

// Cache son vectores por texto. Segura para lectores concurrentes; Put toma
// el candado.
type Cache struct {
	Header CacheHeader
	mu     sync.RWMutex
	m      map[string][]float32
}

// NewCache crea una caché vacía.
func NewCache(h CacheHeader) *Cache { return &Cache{Header: h, m: map[string][]float32{}} }

// Len son las entradas.
func (c *Cache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}

// Get devuelve el vector de text, si está.
func (c *Cache) Get(text string) ([]float32, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[text]
	return v, ok
}

// Put guarda un vector. La dimensión tiene que ser la de la caché.
func (c *Cache) Put(text string, v []float32) error {
	if len(v) != c.Header.Dim {
		return fmt.Errorf("vector of %d dimensions in a cache of %d", len(v), c.Header.Dim)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[text] = v
	return nil
}

// Save escribe la caché (fichero temporal y rename). Las entradas van
// ordenadas por texto: la misma caché da el mismo fichero.
func (c *Cache) Save(path string) error {
	c.mu.RLock()
	keys := make([]string, 0, len(c.m))
	for k := range c.m {
		keys = append(keys, k)
	}
	c.mu.RUnlock()
	sort.Strings(keys)
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	h := crc32.New(crcTable)
	w := bufio.NewWriterSize(io.MultiWriter(f, h), 1<<20)
	hdr, _ := json.Marshal(c.Header)
	var b [8]byte
	w.Write(cacheMagic[:])
	binary.LittleEndian.PutUint16(b[:2], 1)
	binary.LittleEndian.PutUint16(b[2:4], 0)
	w.Write(b[:4])
	binary.LittleEndian.PutUint32(b[:4], uint32(len(hdr)))
	w.Write(b[:4])
	w.Write(hdr)
	binary.LittleEndian.PutUint32(b[:4], uint32(len(keys)))
	w.Write(b[:4])
	c.mu.RLock()
	for _, k := range keys {
		binary.LittleEndian.PutUint32(b[:4], uint32(len(k)))
		w.Write(b[:4])
		w.WriteString(k)
		for _, x := range c.m[k] {
			binary.LittleEndian.PutUint32(b[:4], math.Float32bits(x))
			w.Write(b[:4])
		}
	}
	c.mu.RUnlock()
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	binary.LittleEndian.PutUint32(b[:4], h.Sum32())
	if _, err := f.Write(b[:4]); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadCache lee una caché: como mucho MaxCacheBytes, CRC comprobado antes de
// interpretar nada y cada longitud validada antes de reservar.
func LoadCache(path string) (*Cache, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxCacheBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCacheBytes {
		return nil, fmt.Errorf("%s: larger than %d bytes", path, MaxCacheBytes)
	}
	c, err := UnmarshalCache(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// UnmarshalCache interpreta una caché ya leída. Ver LoadCache.
func UnmarshalCache(data []byte) (*Cache, error) {
	if len(data) < len(cacheMagic)+16 || !bytes.Equal(data[:8], cacheMagic[:]) {
		return nil, errors.New("not an embedding cache (bad magic)")
	}
	body, sum := data[:len(data)-4], binary.LittleEndian.Uint32(data[len(data)-4:])
	if crc32.Checksum(body, crcTable) != sum {
		return nil, errors.New("embedding cache is corrupt (checksum mismatch)")
	}
	rd := &reader{b: body[8:]}
	if v, fl := rd.u16(), rd.u16(); rd.err == nil && (v != 1 || fl != 0) {
		return nil, fmt.Errorf("unsupported cache version %d / flags %#x", v, fl)
	}
	hlen := rd.u32()
	if hlen > 1<<16 {
		return nil, errors.New("cache header too large")
	}
	var h CacheHeader
	if raw := rd.take(int(hlen)); rd.err == nil {
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("bad cache header: %w", err)
		}
	}
	n := rd.u32()
	if rd.err != nil {
		return nil, rd.err
	}
	if h.Dim < 1 || h.Dim > MaxDim || n > maxCacheEntries {
		return nil, fmt.Errorf("bad cache dimensions (dim %d, %d entries)", h.Dim, n)
	}
	// Cada entrada ocupa al menos 4 + 4·dim bytes: si no caben, mienten.
	if int64(n)*int64(4+4*h.Dim) > int64(len(rd.b)) {
		return nil, errors.New("cache entry count does not fit the file")
	}
	c := NewCache(h)
	for i := uint32(0); i < n; i++ {
		tl := rd.u32()
		if tl > MaxTextBytes {
			return nil, errors.New("cache text too long")
		}
		t := string(rd.take(int(tl)))
		raw := rd.take(4 * h.Dim)
		if rd.err != nil {
			return nil, rd.err
		}
		v := make([]float32, h.Dim)
		for j := range v {
			v[j] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*j:]))
			if math.IsNaN(float64(v[j])) || math.IsInf(float64(v[j]), 0) {
				return nil, errors.New("cache holds a non-finite value")
			}
		}
		c.m[t] = v
	}
	if len(rd.b) != 0 {
		return nil, errors.New("trailing bytes in the cache")
	}
	return c, nil
}

// Cached es un Embedder que mira primero la caché y pide lo que falta a Next
// (y lo guarda). Sin Next, lo que no está es un error: así una evaluación no
// llama a una microVM sin querer.
type Cached struct {
	Cache *Cache
	Next  Embedder
}

// Embed implementa Embedder.
func (c *Cached) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	var miss []string
	var idx []int
	for i, t := range texts {
		if v, ok := c.Cache.Get(t); ok {
			out[i] = v
			continue
		}
		miss = append(miss, t)
		idx = append(idx, i)
	}
	if len(miss) == 0 {
		return out, nil
	}
	if c.Next == nil {
		return nil, fmt.Errorf("%d text(s) not in the embedding cache (first: %q)", len(miss), truncUTF8(miss[0], 60))
	}
	for s := 0; s < len(miss); s += MaxBatch {
		e := min(s+MaxBatch, len(miss))
		vs, err := c.Next.Embed(ctx, miss[s:e])
		if err != nil {
			return nil, err
		}
		for k, v := range vs {
			if err := c.Cache.Put(miss[s+k], v); err != nil {
				return nil, err
			}
			out[idx[s+k]] = v
		}
	}
	return out, nil
}
