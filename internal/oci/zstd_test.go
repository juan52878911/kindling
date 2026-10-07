package oci_test

import (
	"archive/tar"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
