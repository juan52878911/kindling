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

func TestParseImageRef(t *testing.T) {
	for _, c := range []struct{ in, out string }{
		{"postgres:17-alpine", "registry-1.docker.io/library/postgres:17-alpine"},
		{"redis", "registry-1.docker.io/library/redis:latest"},
		{"docker.io/timescale/timescaledb:latest-pg16", "registry-1.docker.io/timescale/timescaledb:latest-pg16"},
		{"localhost:5000/x/y", "localhost:5000/x/y:latest"},
		{"ghcr.io/o/r@sha256:" + strings.Repeat("a", 64), "ghcr.io/o/r@sha256:" + strings.Repeat("a", 64)},
		{"ghcr.io/o/r:v1@sha256:" + strings.Repeat("b", 64), "ghcr.io/o/r:v1@sha256:" + strings.Repeat("b", 64)},
	} {
		r, err := oci.ParseImageRef(c.in)
		if err != nil || r.String() != c.out {
			t.Errorf("ParseImageRef(%q) = %s, %v; want %s", c.in, r, err, c.out)
		}
	}
	for _, bad := range []string{"", "Postgres", "x:", "x@latest", "x y", "x:-a", "ghcr.io/o/r@sha256:abc", "a/../b"} {
		if _, err := oci.ParseImageRef(bad); err == nil {
			t.Errorf("ParseImageRef(%q) accepted", bad)
		}
	}
}

func TestResolveAndConfig(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: "x"}})
	man, idx := r.ImageConfig("amd64", map[string]any{
		"Entrypoint": []string{"docker-entrypoint.sh"}, "Cmd": []string{"postgres"},
		"User": "70:70", "WorkingDir": "/srv", "StopSignal": "SIGINT",
		"ExposedPorts": map[string]any{"5432/tcp": map[string]any{}},
		"Volumes":      map[string]any{"/var/lib/postgresql/data": map[string]any{}},
		"Healthcheck":  map[string]any{"Test": []string{"CMD-SHELL", "pg_isready"}, "Interval": 5e9},
	}, layer)
	r.Tag("17-alpine", idx)
	ref, err := oci.ParseImageRef(r.Host() + "/library/postgres:17-alpine")
	if err != nil {
		t.Fatal(err)
	}
	c := &oci.Client{Cache: t.TempDir()}
	d, err := c.Resolve(context.Background(), ref)
	if err != nil || d != idx {
		t.Fatalf("Resolve = %s, %v; want %s", d, err, idx)
	}
	// El manifiesto resuelto queda en la caché: Pull no lo vuelve a pedir.
	hits := r.Hits
	img, err := c.Pull(context.Background(), ref.Name(), d, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if img.ManifestDigest != man {
		t.Fatalf("manifest %s, want %s", img.ManifestDigest, man)
	}
	if r.Hits-hits != 3 { // manifiesto de amd64, config y capa; el índice no
		t.Fatalf("pull after resolve hit the registry %d times, want 3", r.Hits-hits)
	}
	cfg := img.Config.Config
	if cfg.User != "70:70" || cfg.WorkingDir != "/srv" || cfg.StopSignal != "SIGINT" ||
		len(cfg.ExposedPorts) != 1 || len(cfg.Volumes) != 1 || cfg.Healthcheck == nil || cfg.Healthcheck.Test[1] != "pg_isready" {
		t.Fatalf("config: %+v", cfg)
	}

	// Si el registro dice un digest y manda otra cosa, no vale.
	r.LieDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := (&oci.Client{Cache: t.TempDir()}).Resolve(context.Background(), ref); err == nil || !strings.Contains(err.Error(), "registry says") {
		t.Fatalf("lying Docker-Content-Digest: %v", err)
	}
}

func TestPullMaxBytes(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: strings.Repeat("x", 1<<16)}})
	man, _ := r.Image("arm64", nil, layer)
	c := &oci.Client{Cache: t.TempDir(), MaxBytes: int64(len(layer)) - 1}
	if _, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "arm64"); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("over the limit: %v", err)
	}
	c.MaxBytes = int64(len(layer))
	if _, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "arm64"); err != nil {
		t.Fatal(err)
	}
}
