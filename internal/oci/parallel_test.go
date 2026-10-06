package oci_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
)

// Las capas se bajan a la vez (con tope), quedan en orden, una capa repetida
// se baja una vez, y con Unpack cada una queda descomprimida tal cual.
func TestPullParallelUnpack(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	r.BlobDelay = 50 * time.Millisecond
	var layers [][]byte
	for i := 0; i < 7; i++ {
		layers = append(layers, ocitest.TarGz([]ocitest.File{{Name: fmt.Sprintf("f%d", i), Body: strings.Repeat("x", 1000*i)}}))
	}
	layers = append(layers, layers[2]) // repetida, como las capas vacías
	man, _ := r.Image("amd64", nil, layers...)
	dir := filepath.Join(t.TempDir(), "layers")
	var log bytes.Buffer
	c := &oci.Client{Cache: t.TempDir(), Unpack: dir, MaxBytes: 1 << 20, Log: &log}
	img, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxInFlight < 2 || r.MaxInFlight > 4 {
		t.Fatalf("%d downloads at once, want 2..4", r.MaxInFlight)
	}
	if n := strings.Count(log.String(), "downloaded "); n != 8 { // 7 capas distintas + la configuración
		t.Fatalf("%d downloads, want 8:\n%s", n, log.String())
	}
	if len(img.Layers) != len(layers) {
		t.Fatalf("%d layers", len(img.Layers))
	}
	for i, l := range img.Layers {
		if l.Tar == "" || filepath.Dir(l.Tar) != dir {
			t.Fatalf("layer %d not unpacked: %+v", i, l)
		}
		got, err := os.ReadFile(l.Tar)
		if err != nil {
			t.Fatal(err)
		}
		zr, _ := gzip.NewReader(bytes.NewReader(layers[i]))
		want, _ := io.ReadAll(zr)
		if !bytes.Equal(got, want) {
			t.Fatalf("layer %d: unpacked tar differs", i)
		}
		rc, err := oci.OpenLayer(l)
		if err != nil {
			t.Fatal(err)
		}
		h, err := tar.NewReader(rc).Next()
		rc.Close()
		name := fmt.Sprintf("f%d", i)
		if i == 7 {
			name = "f2"
		}
		if err != nil || h.Name != name {
			t.Fatalf("layer %d: %v %v", i, h, err)
		}
	}
}

// Una capa corrupta para todo el Pull, no queda en la caché ni descomprimida.
func TestPullParallelCorrupt(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	var layers [][]byte
	for i := 0; i < 5; i++ {
		layers = append(layers, ocitest.TarGz([]ocitest.File{{Name: fmt.Sprintf("f%d", i), Body: strings.Repeat("y", 500+i)}}))
	}
	man, _ := r.Image("amd64", nil, layers...)
	bad := r.Put(layers[3], "")
	r.Corrupt = bad
	dir := filepath.Join(t.TempDir(), "layers")
	c := &oci.Client{Cache: t.TempDir(), Unpack: dir, Log: io.Discard}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := c.Pull(ctx, r.Host()+"/x/y", man, "amd64"); err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("corrupted layer: %v", err)
	}
	if _, err := os.Stat(c.BlobPath(bad)); !os.IsNotExist(err) {
		t.Fatalf("the corrupted blob stayed in the cache: %v", err)
	}
	if _, err := os.Stat(c.BlobPath(bad) + ".part"); !os.IsNotExist(err) {
		t.Fatalf("a .part stayed in the cache: %v", err)
	}
	if es, _ := os.ReadDir(dir); len(es) > 0 {
		for _, e := range es {
			if e.Name() == "layer-3.tar" {
				t.Fatal("the corrupted layer was unpacked")
			}
		}
	}
}

// Un blob ya en la caché (verificado al bajarlo) no se vuelve a hashear;
// uno que no cumple las condiciones (tamaño, permisos) sí, y si no cuadra
// se baja otra vez.
func TestPullCachedNotRehashed(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: strings.Repeat("z", 4000)}})
	man, _ := r.Image("amd64", nil, layer)
	ref := r.Host() + "/x/y"
	c := &oci.Client{Cache: t.TempDir(), Log: io.Discard}
	img, err := c.Pull(context.Background(), ref, man, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	p := img.Layers[0].Path
	orig, _ := os.ReadFile(p)
	// Mismo tamaño, contenido cambiado: si se hasheara, se volvería a bajar.
	flipped := append([]byte{}, orig...)
	flipped[len(flipped)-3] ^= 0xff
	if err := os.WriteFile(p, flipped, 0o644); err != nil {
		t.Fatal(err)
	}
	hits := r.Hits
	if _, err := (&oci.Client{Cache: c.Cache}).Pull(context.Background(), ref, man, "amd64"); err != nil {
		t.Fatal(err)
	}
	if r.Hits != hits {
		t.Fatalf("a cached blob was re-checked (%d requests)", r.Hits-hits)
	}
	// El CRC del gzip la caza al descomprimirla.
	cu := &oci.Client{Cache: c.Cache, Unpack: filepath.Join(t.TempDir(), "u")}
	if _, err := cu.Pull(context.Background(), ref, man, "amd64"); err == nil || !strings.Contains(err.Error(), "unpacking") {
		t.Fatalf("damaged cached gzip unpacked: %v", err)
	}
	// Escribible por el grupo: no se confía, se hashea, no cuadra y se baja.
	if err := os.Chmod(p, 0o664); err != nil {
		t.Fatal(err)
	}
	if _, err := (&oci.Client{Cache: c.Cache, Log: io.Discard}).Pull(context.Background(), ref, man, "amd64"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, orig) || r.Hits == hits {
		t.Fatal("a group-writable cached blob was trusted")
	}
	// Cortado: tampoco.
	hits = r.Hits
	if err := os.WriteFile(p, orig[:100], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (&oci.Client{Cache: c.Cache, Log: io.Discard}).Pull(context.Background(), ref, man, "amd64"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, orig) || r.Hits == hits {
		t.Fatal("a truncated cached blob was trusted")
	}
}

// Lo descomprimido tiene tope (MaxBytes × 8): una bomba gzip no llena el
// disco, y no deja tars a medias.
func TestPullUnpackLimit(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	bomb := ocitest.TarGz([]ocitest.File{{Name: "zeros", Body: string(make([]byte, 4<<20))}})
	man, _ := r.Image("amd64", nil, bomb)
	dir := filepath.Join(t.TempDir(), "layers")
	c := &oci.Client{Cache: t.TempDir(), Unpack: dir, MaxBytes: 64 << 10, Log: io.Discard}
	if int64(len(bomb)) > c.MaxBytes {
		t.Fatalf("the bomb is %d bytes compressed", len(bomb))
	}
	if _, err := c.Pull(context.Background(), r.Host()+"/x/y", man, "amd64"); err == nil || !strings.Contains(err.Error(), "unpacks to more than") {
		t.Fatalf("gzip bomb: %v", err)
	}
	if es, _ := os.ReadDir(dir); len(es) != 0 {
		t.Fatalf("left %d files in the unpack dir", len(es))
	}
}

// Con SiempreRehash (el constructor sin privilegios), un blob de la caché se
// rehashea: uno cambiado se vuelve a bajar.
func TestPullSiempreRehash(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: strings.Repeat("z", 4000)}})
	man, _ := r.Image("amd64", nil, layer)
	ref := r.Host() + "/x/y"
	c := &oci.Client{Cache: t.TempDir(), Log: io.Discard}
	img, err := c.Pull(context.Background(), ref, man, "amd64")
	if err != nil {
		t.Fatal(err)
	}
	p := img.Layers[0].Path
	orig, _ := os.ReadFile(p)
	flipped := append([]byte{}, orig...)
	flipped[len(flipped)-3] ^= 0xff
	if err := os.WriteFile(p, flipped, 0o644); err != nil {
		t.Fatal(err)
	}
	hits := r.Hits
	if _, err := (&oci.Client{Cache: c.Cache, SiempreRehash: true}).Pull(context.Background(), ref, man, "amd64"); err != nil {
		t.Fatal(err)
	}
	if r.Hits == hits {
		t.Fatal("a cached blob was trusted without rehashing")
	}
	if b, _ := os.ReadFile(p); string(b) != string(orig) {
		t.Fatal("the tampered blob stayed in the cache")
	}
}
