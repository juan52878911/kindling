package oci

import (
	"bytes"
	"compress/gzip"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func escribe(t *testing.T, dir, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func gz(t *testing.T, data []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write(data)
	w.Close()
	return b.Bytes()
}

// abre descomprime path con decompress tal cual (con el ayudante que diga el
// entorno) y devuelve lo leído y el error de leerlo entero.
func abre(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := decompress(f)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func soloUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ayudantes de prueba en sh")
	}
}

func TestUnpackHelperEleccion(t *testing.T) {
	soloUnix(t)
	dir := t.TempDir()
	ok := escribe(t, dir, "kling-unpack", []byte("#!/bin/sh\ncat\n"), 0o755)
	noexec := escribe(t, dir, "noexec", []byte("#!/bin/sh\ncat\n"), 0o644)

	t.Setenv("KLING_UNPACK", "0")
	if h := unpackHelper(); h != "" {
		t.Errorf("KLING_UNPACK=0 has to turn it off, got %q", h)
	}
	t.Setenv("KLING_UNPACK", noexec)
	if h := unpackHelper(); h != "" {
		t.Errorf("a non-executable helper can't be used, got %q", h)
	}
	t.Setenv("KLING_UNPACK", ok)
	if h := unpackHelper(); h != ok {
		t.Errorf("KLING_UNPACK=%s gives %q", ok, h)
	}
	os.Unsetenv("KLING_UNPACK")
	t.Setenv("KLING_LIB_DIR", dir)
	SetUnpackHelper("")
	if h := unpackHelper(); h != ok {
		t.Errorf("KLING_LIB_DIR/kling-unpack should be the default, got %q", h)
	}
	SetUnpackHelper(filepath.Join(dir, "missing"))
	defer SetUnpackHelper("")
	if h := unpackHelper(); h != "" {
		t.Errorf("a missing helper means Go, got %q", h)
	}
}

// Un ayudante que muere a mitad no puede pasar por la capa entera: lo que dio
// antes de morir no acaba en io.EOF.
func TestUnpackHelperFallaAMedias(t *testing.T) {
	soloUnix(t)
	dir := t.TempDir()
	h := escribe(t, dir, "h", []byte("#!/bin/sh\nprintf partial\necho 'kling-unpack: crc mismatch' >&2\nexit 1\n"), 0o755)
	layer := escribe(t, dir, "l.gz", gz(t, []byte("hello")), 0o644)
	t.Setenv("KLING_UNPACK", h)
	got, err := abre(t, layer)
	if err == nil || !strings.Contains(err.Error(), "crc mismatch") {
		t.Fatalf("a helper that exits 1 has to fail the read with its message, got %q, %v", got, err)
	}
}

// Si el ayudante está pero no arranca (otra arquitectura), se sigue en Go.
func TestUnpackHelperNoArranca(t *testing.T) {
	soloUnix(t)
	dir := t.TempDir()
	h := escribe(t, dir, "h", []byte{0x7f, 'E', 'L', 'F', 0, 1, 2, 3}, 0o755)
	layer := escribe(t, dir, "l.gz", gz(t, []byte("hello")), 0o644)
	t.Setenv("KLING_UNPACK", h)
	got, err := abre(t, layer)
	if err != nil || string(got) != "hello" {
		t.Fatalf("a helper that doesn't start has to fall back to Go: %q, %v", got, err)
	}
}

// Cerrar antes del final para al ayudante y no se queda colgado.
func TestUnpackHelperCierraAntes(t *testing.T) {
	soloUnix(t)
	dir := t.TempDir()
	h := escribe(t, dir, "h", []byte("#!/bin/sh\nexec yes\n"), 0o755)
	layer := escribe(t, dir, "l.gz", gz(t, []byte("hello")), 0o644)
	t.Setenv("KLING_UNPACK", h)
	f, _ := os.Open(layer)
	rc, err := decompress(f)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(rc, make([]byte, 4096)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- rc.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close didn't stop the helper")
	}
	if _, err := rc.Read(make([]byte, 1)); err == nil || err == io.EOF {
		t.Errorf("reading after an early Close can't look like the end of the layer: %v", err)
	}
}

// Con el kling-unpack de verdad (cargo build --release en rust/kling-unpack, o
// KLING_UNPACK_TEST): da los mismos bytes que Go y falla donde Go falla.
func TestUnpackHelperIgualQueGo(t *testing.T) {
	// Con KLING_UNPACK_TEST (el CI) tiene que estar: no se salta en silencio.
	h := os.Getenv("KLING_UNPACK_TEST")
	if h == "" {
		h, _ = filepath.Abs("../../rust/kling-unpack/target/release/kling-unpack")
		if _, err := os.Stat(h); err != nil {
			t.Skip("kling-unpack is not built (cargo build --release in rust/kling-unpack)")
		}
	} else if _, err := os.Stat(h); err != nil {
		t.Fatalf("KLING_UNPACK_TEST=%s: %v", h, err)
	}
	dir := t.TempDir()
	rnd := rand.New(rand.NewSource(1))
	big := make([]byte, 3<<20)
	for i := range big {
		big[i] = byte('a' + rnd.Intn(4)) // comprimible pero no trivial
	}
	g := gz(t, big)
	casos := map[string][]byte{
		"gzip":        g,
		"multimember": append(gz(t, []byte("first ")), gz(t, []byte("second"))...),
		"empty-gzip":  gz(t, nil),
		"truncated":   g[:len(g)/2],
		"no-trailer":  g[:len(g)-4],
		"garbage":     []byte("this is not a layer at all"),
		"tail-junk":   append(gz(t, []byte("x")), "junk after the member"...),
	}
	bad := append([]byte(nil), g...)
	bad[len(bad)-6] ^= 0xff // el CRC32
	casos["bad-crc"] = bad
	// Ventanas por encima del tope de 128 MiB: un marco que pide 1 GiB y uno de
	// un solo segmento que dice traer 200 MiB. Ninguno de los dos se abre.
	raw := append([]byte{41, 0, 0}, "hello"...) // bloque raw de 5 bytes, el último
	casos["zstd-window-1g"] = append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 20 << 3}, raw...)
	casos["zstd-single-200m"] = append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0xa0, 0, 0, 0x80, 0x0c}, raw...)
	casos["zstd-ok-small"] = append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x00}, raw...)
	zs, _ := filepath.Glob("../zstd/testdata/*.zst")
	for _, z := range zs {
		if strings.HasPrefix(filepath.Base(z), ".") {
			continue // un ._ de macOS al copiar el árbol, no un caso
		}
		b, err := os.ReadFile(z)
		if err != nil {
			t.Fatal(err)
		}
		casos["zstd-"+filepath.Base(z)] = b
		casos["zstd-truncated-"+filepath.Base(z)] = b[:len(b)*2/3]
	}
	for name, data := range casos {
		p := escribe(t, dir, name, data, 0o644)
		t.Setenv("KLING_UNPACK", "0")
		want, werr := abre(t, p)
		t.Setenv("KLING_UNPACK", h)
		got, gerr := abre(t, p)
		malo := strings.Contains(name, "truncated") || strings.Contains(name, "window") ||
			strings.Contains(name, "200m") || name == "no-trailer" || name == "garbage" || name == "bad-crc"
		if malo && gerr == nil {
			t.Errorf("%s: kling-unpack opened a bad layer", name)
		}
		if !malo && name != "tail-junk" && gerr != nil {
			t.Errorf("%s: kling-unpack failed on a good layer: %v", name, gerr)
		}
		switch {
		case (werr == nil) != (gerr == nil):
			t.Errorf("%s: Go says %v, kling-unpack says %v", name, werr, gerr)
		case werr == nil && !bytes.Equal(got, want):
			t.Errorf("%s: kling-unpack gives %d bytes that differ from Go's %d", name, len(got), len(want))
		}
	}
}
