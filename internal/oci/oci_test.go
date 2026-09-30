package oci_test

import (
	"archive/tar"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
)

func TestParseRef(t *testing.T) {
	for _, c := range []struct{ in, reg, repo string }{
		{"debian", "registry-1.docker.io", "library/debian"},
		{"redroid/redroid", "registry-1.docker.io", "redroid/redroid"},
		{"docker.io/redroid/redroid", "registry-1.docker.io", "redroid/redroid"},
		{"ghcr.io/o/r", "ghcr.io", "o/r"},
		{"127.0.0.1:5000/x/y", "127.0.0.1:5000", "x/y"},
	} {
		if reg, repo := oci.ParseRef(c.in); reg != c.reg || repo != c.repo {
			t.Errorf("ParseRef(%q) = %s %s", c.in, reg, repo)
		}
	}
}

func TestPull(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "init", Body: "\x7fELF"}, {Name: "etc/", Dir: true, Mode: 0o755}})
	man, idx := r.Image("arm64", []string{"/init", "qemu=1"}, layer)
	c := &oci.Client{Cache: t.TempDir()}
	ref := r.Host() + "/redroid/redroid"

	// Por el índice: elige arm64 y lo comprueba con el digest del índice.
	img, err := c.Pull(context.Background(), ref, idx, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if img.ManifestDigest != man || len(img.Layers) != 1 || img.Config.Config.Entrypoint[1] != "qemu=1" {
		t.Fatalf("%+v", img)
	}
	rc, err := oci.OpenLayer(img.Layers[0])
	if err != nil {
		t.Fatal(err)
	}
	h, err := tar.NewReader(rc).Next()
	rc.Close()
	if err != nil || h.Name != "init" {
		t.Fatalf("layer: %v %v", h, err)
	}
	if _, err := c.Pull(context.Background(), ref, idx, "amd64"); err == nil || !strings.Contains(err.Error(), "no linux/amd64") {
		t.Fatalf("amd64 from an arm64-only index: %v", err)
	}

	// Todo en la caché: otra descarga no toca el registro.
	hits := r.Hits
	if _, err := (&oci.Client{Cache: c.Cache}).Pull(context.Background(), ref, man, "arm64"); err != nil {
		t.Fatal(err)
	}
	if r.Hits != hits {
		t.Fatalf("a cached pull hit the registry %d times", r.Hits-hits)
	}
}

func TestPullRejectsBadBlobs(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: strings.Repeat("x", 1000)}})
	man, _ := r.Image("arm64", nil, layer)
	ref := r.Host() + "/x/y"
	// Una capa que llega cambiada no pasa, ni se queda en la caché.
	r.Corrupt = r.Put(layer, "")
	c := &oci.Client{Cache: t.TempDir(), Log: io.Discard}
	if _, err := c.Pull(context.Background(), ref, man, "arm64"); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("corrupted layer: %v", err)
	}
	// Un manifiesto que no es el del digest pedido tampoco.
	r.Corrupt = man
	if _, err := (&oci.Client{Cache: t.TempDir()}).Pull(context.Background(), ref, man, "arm64"); err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("corrupted manifest: %v", err)
	}
	// La arquitectura se comprueba en la configuración.
	r.Corrupt = ""
	if _, err := c.Pull(context.Background(), ref, man, "amd64"); err == nil {
		t.Fatal("arm64 image accepted as amd64")
	}
	if _, err := c.Pull(context.Background(), ref, "latest", "arm64"); err == nil {
		t.Fatal("a tag was accepted as a digest")
	}
}
