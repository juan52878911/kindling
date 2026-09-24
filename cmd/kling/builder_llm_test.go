package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func tarball(t *testing.T, entries []tar.Header, bodies map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range entries {
		h := h
		body := bodies[h.Name]
		h.Size = int64(len(body))
		if h.Typeflag == tar.TypeSymlink || h.Typeflag == tar.TypeDir {
			h.Size = 0
		}
		if h.Mode == 0 {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write([]byte(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	tw.Close()
	gz.Close()
	p := filepath.Join(t.TempDir(), "l.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExtractLlama(t *testing.T) {
	tb := tarball(t, []tar.Header{
		{Name: "llama-b1/", Typeflag: tar.TypeDir},
		{Name: "llama-b1/llama-server", Typeflag: tar.TypeReg},
		{Name: "llama-b1/llama-cli", Typeflag: tar.TypeReg},
		{Name: "llama-b1/libllama.so.0.5.0", Typeflag: tar.TypeReg},
		{Name: "llama-b1/libllama.so.0", Typeflag: tar.TypeSymlink, Linkname: "libllama.so.0.5.0"},
		{Name: "llama-b1/libggml-cpu-armv8.2_1.so", Typeflag: tar.TypeReg},
		{Name: "llama-b1/libggml-rpc.so", Typeflag: tar.TypeReg},
		{Name: "llama-b1/sub/llama-server", Typeflag: tar.TypeReg},
	}, map[string]string{
		"llama-b1/llama-server": "ELF", "llama-b1/llama-cli": "x", "llama-b1/libllama.so.0.5.0": "so",
		"llama-b1/libggml-cpu-armv8.2_1.so": "cpu", "llama-b1/libggml-rpc.so": "rpc", "llama-b1/sub/llama-server": "evil",
	})
	dst := t.TempDir()
	n, err := extractLlama(tb, dst)
	if err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dst)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := "libggml-cpu-armv8.2_1.so libllama.so.0 libllama.so.0.5.0 llama-server"
	if strings.Join(got, " ") != want || n != 4 {
		t.Fatalf("extraído %v (%d), quería %s", got, n, want)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "llama-server")); string(b) != "ELF" {
		t.Fatalf("llama-server = %q: el de un subdirectorio no debe pisarlo", b)
	}
	if l, _ := os.Readlink(filepath.Join(dst, "libllama.so.0")); l != "libllama.so.0.5.0" {
		t.Fatalf("enlace = %q", l)
	}

	// Un enlace que sale del directorio no se sigue: se rechaza el tarball.
	malo := tarball(t, []tar.Header{
		{Name: "llama-b1/libllama.so.0", Typeflag: tar.TypeSymlink, Linkname: "../../etc/shadow"},
	}, nil)
	if _, err := extractLlama(malo, t.TempDir()); err == nil {
		t.Fatal("un enlace fuera del directorio debería rechazarse")
	}
}

func TestFetchVerified(t *testing.T) {
	body := "gguf bytes"
	sum := sha256.Sum256([]byte(body))
	want := hex.EncodeToString(sum[:])
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "m.gguf")
	if err := fetchVerified(srv.URL, want, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != body {
		t.Fatalf("contenido %q", b)
	}
	// En caché y bueno: no se vuelve a bajar.
	if err := fetchVerified(srv.URL, want, dst); err != nil || hits != 1 {
		t.Fatalf("segunda vez: err %v, %d descargas", err, hits)
	}
	// Hash que no cuadra: error, y nada en el destino (ni el .part).
	otro := filepath.Join(t.TempDir(), "x.gguf")
	if err := fetchVerified(srv.URL, strings.Repeat("0", 64), otro); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("hash malo: %v", err)
	}
	if _, err := os.Stat(otro); err == nil {
		t.Fatal("un fichero con hash malo no debe quedar en la caché")
	}
	if _, err := os.Stat(otro + ".part"); err == nil {
		t.Fatal("quedó el .part")
	}
}

// TestFetchVerifiedStalledDownload comprueba que una descarga que deja de
// mandar bytes a mitad de camino se corta sola en vez de colgar el builder
// para siempre (era el bug: http.Get sin ningún timeout).
func TestFetchVerifiedStalledDownload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("first bytes, then silence"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Simula un HuggingFace/GitHub que se cuelga a mitad de descarga: no
		// manda nada más hasta mucho después de que el test se dé por vencido.
		// Si el cliente cancela antes (que es justo lo que probamos), el
		// contexto de la petición se cierra y el handler no se queda esperando
		// los 2s completos.
		select {
		case <-time.After(2 * time.Second):
			_, _ = w.Write([]byte("bytes that arrive too late"))
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	dst := filepath.Join(t.TempDir(), "stalled.gguf")
	start := time.Now()
	err := fetchVerifiedTimeout(srv.URL, strings.Repeat("0", 64), dst, 200*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil || !strings.Contains(err.Error(), "stalled") {
		t.Fatalf("err = %v, quería un error de estancamiento", err)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("tardó %s en cortar: el detector de estancamiento no actuó a tiempo", elapsed)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("una descarga estancada no debe dejar el fichero final")
	}
	if _, err := os.Stat(dst + ".part"); err == nil {
		t.Fatal("quedó el .part de una descarga estancada")
	}
}
