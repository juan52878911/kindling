package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/oci/ocitest"
	"github.com/juan52878911/kindling/internal/upgrade"
)

// releaseImagenes sirve /v0.20.0/ con los assets dados y un SHA256SUMS que
// lista sumas (o el de cada asset si sumas es nil).
func releaseImagenes(t *testing.T, assets map[string][]byte, sumas map[string]string) *upgrade.Fuente {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, ok := strings.CutPrefix(r.URL.Path, "/releases/download/v0.20.0/")
		switch {
		case !ok:
			http.NotFound(w, r)
		case n == "SHA256SUMS":
			for a, b := range assets {
				s := sumas[a]
				if sumas == nil {
					h := sha256.Sum256(b)
					s = hex.EncodeToString(h[:])
				}
				fmt.Fprintf(w, "%s  %s\n", s, a)
			}
		case assets[n] != nil:
			w.Write(assets[n])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &upgrade.Fuente{Repo: srv.URL, Client: srv.Client()}
}

func TestBajarImagenesMac(t *testing.T) {
	kernel := bytes.Repeat([]byte("K"), 3000)
	// Un ext4 de mentira con un hueco de ceros en medio: sale igual.
	min := append(append(bytes.Repeat([]byte("E"), 1000), make([]byte, 200<<10)...), []byte("fin")...)
	f := releaseImagenes(t, map[string][]byte{
		"vmlinux-arm64.zst":   ocitest.Zstd(kernel),
		"min-arm64.ext4.zst":  ocitest.Zstd(min),
		"kling-darwin-arm64":  []byte("x"),
		"vmlinux-amd64.zst":   ocitest.Zstd([]byte("otro")),
		"min-amd64.ext4.zst":  ocitest.Zstd([]byte("otro")),
		"kling-hv-darwin-x64": []byte("x"),
	}, nil)
	root := t.TempDir()
	var out bytes.Buffer
	if err := bajarImagenesMac(context.Background(), f, "v0.20.0", root, "arm64", &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	for nombre, want := range map[string][]byte{"vmlinux": kernel, "min.ext4": min} {
		if b, _ := os.ReadFile(filepath.Join(root, "images", nombre)); !bytes.Equal(b, want) {
			t.Errorf("%s: %d bytes, quería %d", nombre, len(b), len(want))
		}
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "images")); len(ents) != 2 {
		t.Errorf("images/ tiene %d entradas (¿quedó el temporal?)", len(ents))
	}
	// Lo que ya está no se toca: ni se baja.
	os.WriteFile(filepath.Join(root, "images", "min.ext4"), []byte("mía"), 0o644)
	os.Remove(filepath.Join(root, "images", "vmlinux"))
	if err := bajarImagenesMac(context.Background(), f, "v0.20.0", root, "arm64", io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "images", "min.ext4")); string(b) != "mía" {
		t.Errorf("min.ext4 reemplazada: %q", b)
	}
}

// Un asset cuyo hash no es el de SHA256SUMS no deja nada en images/.
func TestBajarImagenesMacHashMalo(t *testing.T) {
	f := releaseImagenes(t, map[string][]byte{
		"vmlinux-arm64.zst":  ocitest.Zstd([]byte("k")),
		"min-arm64.ext4.zst": ocitest.Zstd([]byte("m")),
	}, map[string]string{"vmlinux-arm64.zst": strings.Repeat("0", 64), "min-arm64.ext4.zst": strings.Repeat("0", 64)})
	root := t.TempDir()
	err := bajarImagenesMac(context.Background(), f, "v0.20.0", root, "arm64", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v", err)
	}
	if ents, _ := os.ReadDir(filepath.Join(root, "images")); len(ents) != 0 {
		t.Errorf("images/ tiene %d entradas tras un hash malo", len(ents))
	}
}

// Una release que no los publica, o una compilación sin etiqueta.
func TestBajarImagenesMacSinEllas(t *testing.T) {
	f := releaseImagenes(t, map[string][]byte{"kling-darwin-arm64": []byte("x")}, nil)
	for _, tag := range []string{"v0.20.0", "vdev"} {
		if err := bajarImagenesMac(context.Background(), f, tag, t.TempDir(), "arm64", io.Discard); !errors.Is(err, errSinImagenes) {
			t.Errorf("%s: err = %v", tag, err)
		}
	}
}

// Más de lo permitido una vez descomprimido es un error, no un truncado.
func TestDescomprimirImagenTope(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "x.zst")
	os.WriteFile(src, ocitest.Zstd(make([]byte, 5000)), 0o644)
	if err := descomprimirImagen(src, filepath.Join(dir, "x"), 4096); err == nil {
		t.Fatal("aceptó más del tope")
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); err == nil {
		t.Error("quedó el fichero a medias")
	}
	if err := descomprimirImagen(src, filepath.Join(dir, "x"), 5000); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "x")); st == nil || st.Size() != 5000 {
		t.Errorf("tamaño %v, quería 5000 (todo ceros: disperso)", st)
	}
}
