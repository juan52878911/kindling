package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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

// La base glibc ya no necesita debootstrap en el host: la hace el constructor
// debian en Go, con el lock fijado. Dos construcciones a la vez la hacen una
// sola vez (en fila, con un cerrojo de fichero), y una que falla no deja una
// base a medias con el nombre definitivo.
func TestBaseLLMSinDebootstrap(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // ni debootstrap ni nada
	root := t.TempDir()
	lib := t.TempDir()
	var mu sync.Mutex
	veces := 0
	antes := baseGlibcEnGo
	t.Cleanup(func() { baseGlibcEnGo = antes })
	baseGlibcEnGo = func(root, lib, dst string) error {
		mu.Lock()
		veces++
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		return os.WriteFile(dst, []byte("base"), 0o600)
	}
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = buildLLMBase(root, lib, "glibc-trixie") }(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "images", "glibc-trixie.ext4")); string(b) != "base" || veces != 1 {
		t.Fatalf("base %q, construida %d veces; quería una", b, veces)
	}

	baseGlibcEnGo = func(root, lib, dst string) error {
		os.WriteFile(dst, []byte("a medias"), 0o600)
		return fmt.Errorf("corte")
	}
	if err := buildLLMBase(root, lib, "otra"); err == nil {
		t.Fatal("una base que falla no dio error")
	}
	if left, _ := filepath.Glob(filepath.Join(root, "images", "*otra*ext4*")); len(left) > 0 {
		t.Fatalf("una base fallida dejó %v", left)
	}
}

// 71-build-glibc-base.sh (el camino de antes, KLING_LLM_BASE=debootstrap, y
// el de las bases de MCP) tampoco deja una base a medias con su nombre: con
// falsos que hacen fallar el formateo, no queda ningún <nombre>.ext4.
func TestScriptBaseGlibcCortadoNoDejaFichero(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("sin bash")
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "root")
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	for name, body := range map[string]string{
		"id":          "echo 0\n",
		"uname":       "echo x86_64\n",
		"debootstrap": "exit 0\n",
		"curl":        "exit 0\n",
		"truncate":    "for a in \"$@\"; do f=\"$a\"; done; : > \"$f\"\n",
		"mkfs.ext4":   "exit 1\n",
	} {
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755)
	}
	cmd := exec.Command("bash", "../../scripts/71-build-glibc-base.sh", "glibc-x")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "KLING_ROOT="+root)
	if b, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("el script siguió tras un mkfs fallido:\n%s", b)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "images", "*glibc-x*")); len(left) > 0 {
		t.Fatalf("un build cortado dejó %v", left)
	}
}
