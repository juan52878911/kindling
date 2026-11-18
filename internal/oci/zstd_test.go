package oci_test

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
)

// names recorre la capa como tar y da sus entradas y el cuerpo de la última.
func names(t *testing.T, l oci.Layer) (string, string) {
	t.Helper()
	rc, err := oci.OpenLayer(l)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	var ns []string
	var body []byte
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("layer %s: %v", l.MediaType, err)
		}
		ns = append(ns, h.Name)
		body, _ = io.ReadAll(tr)
	}
	return strings.Join(ns, ","), string(body)
}

// Las capas tar+zstd se aceptan y se leen como las gzip, con Unpack y sin él.
func TestPullZstd(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	big := strings.Repeat("kindling ", 2000)
	zl := ocitest.Zstd(ocitest.Tar([]ocitest.File{{Name: "etc/", Dir: true, Mode: 0o755}, {Name: "etc/z", Body: big}}))
	gl := ocitest.TarGz([]ocitest.File{{Name: "g", Body: "gzip"}})
	man, _ := r.Image("amd64", nil, zl, gl)
	for _, unpack := range []string{"", filepath.Join(t.TempDir(), "u")} {
		c := &oci.Client{Cache: t.TempDir(), Unpack: unpack, MaxBytes: 1 << 20, Log: io.Discard}
		img, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "amd64")
		if err != nil {
			t.Fatal(err)
		}
		if mt := img.Layers[0].MediaType; mt != "application/vnd.oci.image.layer.v1.tar+zstd" {
			t.Fatalf("media type %s", mt)
		}
		if unpack != "" && img.Layers[0].Tar == "" {
			t.Fatal("zstd layer not unpacked")
		}
		if ns, body := names(t, img.Layers[0]); ns != "etc/,etc/z" || body != big {
			t.Fatalf("zstd layer: %s, %d bytes", ns, len(body))
		}
		if ns, body := names(t, img.Layers[1]); ns != "g" || body != "gzip" {
			t.Fatalf("gzip layer: %s %q", ns, body)
		}
	}
}

// Lo que se descomprime lo deciden los primeros bytes: una capa zstd que
// dice ser gzip se lee, y una que no es ni una cosa ni otra es un error.
func TestPullCompressionByMagic(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	r.LayerType = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	zl := ocitest.Zstd(ocitest.Tar([]ocitest.File{{Name: "a", Body: "zstd"}}))
	plain := ocitest.Tar([]ocitest.File{{Name: "b", Body: "plain"}})
	man, _ := r.Image("amd64", nil, zl, plain)
	c := &oci.Client{Cache: t.TempDir(), Log: io.Discard}
	img, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if ns, body := names(t, img.Layers[0]); ns != "a" || body != "zstd" {
		t.Fatalf("zstd labelled gzip: %s %q", ns, body)
	}
	if _, err := oci.OpenLayer(img.Layers[1]); err == nil || !strings.Contains(err.Error(), "neither gzip nor zstd") {
		t.Fatalf("plain tar labelled gzip: %v", err)
	}
}

// Una bomba zstd (bloques RLE: 4 bytes dan 128 KiB) choca con el tope de lo
// descomprimido igual que una gzip.
func TestPullZstdBomb(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	bomb := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x38} // ventana de 8 MiB
	for i := 0; i < 64; i++ {
		h := 128<<10<<3 | 1<<1 // RLE
		if i == 63 {
			h |= 1
		}
		bomb = append(bomb, byte(h), byte(h>>8), byte(h>>16), 0)
	}
	man, _ := r.Image("amd64", nil, bomb)
	dir := filepath.Join(t.TempDir(), "layers")
	c := &oci.Client{Cache: t.TempDir(), Unpack: dir, MaxBytes: 64 << 10, Log: io.Discard}
	if _, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "amd64"); err == nil || !strings.Contains(err.Error(), "unpacks to more than") {
		t.Fatalf("zstd bomb: %v", err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("left %d files in the unpack dir", len(es))
	}
}

// picoDeDescompresion baja una imagen con capas y devuelve cuántas se
// descomprimieron a la vez como mucho. Cada capa se queda dentro hasta que se
// ve el pico objetivo (o vence plazo): con un sueño fijo, en un runner de 3 núcleos
// cargado y con -race, la última llegaba tarde y el pico salía corto.
//
// Las zstd y las gzip van en tests separados: las dos comparten los turnos de
// CPU, y con 3 núcleos una zstd que espera a la segunda zstd más dos gzip que
// esperan a la tercera ocupan los tres turnos y se bloquean entre sí hasta el
// plazo (el pico de gzip salía 2 en el job de APFS).
func picoDeDescompresion(t *testing.T, zst bool, objetivo int, plazo time.Duration) int {
	t.Helper()
	r := ocitest.New()
	defer r.Close()
	var layers [][]byte
	for i := 0; i < 6; i++ {
		f := []ocitest.File{{Name: fmt.Sprintf("f%d", i), Body: strings.Repeat("z", 100*i)}}
		if zst {
			layers = append(layers, ocitest.Zstd(ocitest.Tar(f)))
		} else {
			layers = append(layers, ocitest.TarGz(f))
		}
	}
	var mu sync.Mutex
	var now, peak int
	oci.SetUnpackHook(func(esZstd bool) func() {
		if esZstd != zst {
			t.Errorf("capa zstd=%v en una imagen zstd=%v", esZstd, zst)
		}
		mu.Lock()
		now++
		peak = max(peak, now)
		mu.Unlock()
		for fin := time.Now().Add(plazo); time.Now().Before(fin); {
			mu.Lock()
			n := peak
			mu.Unlock()
			if n >= objetivo {
				break
			}
			time.Sleep(time.Millisecond)
		}
		return func() { mu.Lock(); now--; mu.Unlock() }
	})
	defer oci.SetUnpackHook(nil)
	man, _ := r.Image("amd64", nil, layers...)
	c := &oci.Client{Cache: t.TempDir(), Unpack: filepath.Join(t.TempDir(), "layers"), MaxBytes: 1 << 20, Log: io.Discard}
	if _, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "amd64"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	return peak
}

// Las capas zstd se descomprimen de dos en dos como mucho (cada una guarda
// ~230 MiB con la ventana más grande). Se espera a ver 3, que no debe llegar:
// si el código dejara pasar más de 2, la tercera entraría dentro del plazo.
func TestPullZstdParallel(t *testing.T) {
	if runtime.NumCPU() < 3 {
		t.Skip("needs 3 CPUs")
	}
	if p := picoDeDescompresion(t, true, 3, 300*time.Millisecond); p != 2 {
		t.Fatalf("%d zstd layers unpacked at once, want 2", p)
	}
}

// Las gzip, tantas como núcleos: con 3 o más, tres a la vez.
func TestPullGzipParallel(t *testing.T) {
	if runtime.NumCPU() < 3 {
		t.Skip("needs 3 CPUs")
	}
	if p := picoDeDescompresion(t, false, 3, 2*time.Second); p < 3 {
		t.Fatalf("%d gzip layers unpacked at once, want >= 3", p)
	}
}
