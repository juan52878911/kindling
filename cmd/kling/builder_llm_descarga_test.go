package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// datosAleatorios da n bytes que no se comprimen ni son todo ceros.
func datosAleatorios(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// limitado escribe a como mucho bps bytes por segundo: un flujo HTTP de un CDN
// que tiene un techo por conexión (57 MiB/s medidos desde EC2 a HuggingFace).
type limitado struct {
	http.ResponseWriter
	bps int
}

func (l limitado) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		c := 256 << 10
		if c > len(p) {
			c = len(p)
		}
		t0 := time.Now()
		k, err := l.ResponseWriter.Write(p[:c])
		n += k
		if err != nil {
			return n, err
		}
		if d := time.Duration(float64(c)/float64(l.bps)*float64(time.Second)) - time.Since(t0); d > 0 {
			time.Sleep(d)
		}
		p = p[c:]
	}
	return n, nil
}

// servidorDe sirve data (con rangos, como un CDN) a bps por conexión.
func servidorDe(data []byte, bps int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(limitado{w, bps}, r, "m.gguf", time.Time{}, bytes.NewReader(data))
	}))
}

// La descarga de un modelo con un techo por conexión: lo que cuesta bajar 64
// MiB a 64 MiB/s por flujo.
func BenchmarkDescargaFlujoLimitado(b *testing.B) {
	data := datosAleatorios(64 << 20)
	want := sha256Hex(data)
	srv := servidorDe(data, 64<<20)
	defer srv.Close()
	dir := b.TempDir()
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dst := filepath.Join(dir, "m.gguf")
		os.Remove(dst)
		os.Remove(dst + ".verified")
		if err := fetchVerified(srv.URL, want, dst); err != nil {
			b.Fatal(err)
		}
	}
}

// Reconstruir con el modelo ya en la caché: lo que cuesta comprobarlo (256
// MiB, caché de páginas caliente).
func BenchmarkFetchVerificadoEnCache(b *testing.B) {
	data := datosAleatorios(256 << 20)
	want := sha256Hex(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no debería bajar nada", http.StatusTeapot)
	}))
	defer srv.Close()
	dir := b.TempDir()
	dst := filepath.Join(dir, "m.gguf")
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		b.Fatal(err)
	}
	data = nil
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := fetchVerified(srv.URL, want, dst); err != nil {
			b.Fatal(err)
		}
	}
}
