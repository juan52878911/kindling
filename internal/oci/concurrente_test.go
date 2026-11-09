package oci_test

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/oci"
	"github.com/juan52878911/kindling/internal/oci/ocitest"
)

// Dos construcciones que bajan a la vez el mismo blob a la misma caché (en
// macOS no hay cerrojo de constructor que las ponga en fila) no pueden
// pisarse el temporal: con un nombre fijo (<hex>.part y O_TRUNC) la segunda
// truncaba lo que llevaba la primera, la primera renombraba un fichero con
// un hueco de ceros, lo daba por verificado y la descompresión fallaba (o la
// segunda no encontraba ya su .part para renombrarlo).
func TestDescargasConcurrentesDelMismoBlob(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	// Incompresible, para que la capa ocupe de verdad y la descarga dure.
	rnd := make([]byte, 384<<10)
	_, _ = rand.Read(rnd)
	layer := ocitest.TarGz([]ocitest.File{{Name: "datos", Body: hex.EncodeToString(rnd)}})
	man, _ := r.Image("arm64", nil, layer)
	r.BlobTrickle = 4 * time.Millisecond
	cache := t.TempDir()
	ref := r.Host() + "/x/y"

	var wg sync.WaitGroup
	var log logSeguro
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// La segunda empieza cuando la primera lleva más de media
			// descarga: el hueco que le dejaba al truncarla ya no se rellena
			// antes de que la primera la publique.
			time.Sleep(time.Duration(i) * 60 * time.Millisecond)
			c := &oci.Client{Cache: cache, Log: &log, Unpack: filepath.Join(t.TempDir(), "u")}
			img, err := c.Pull(context.Background(), ref, man, "arm64")
			if err != nil {
				errs[i] = err
				return
			}
			rc, err := oci.OpenLayer(img.Layers[0])
			if err != nil {
				errs[i] = err
				return
			}
			defer rc.Close()
			tr := tar.NewReader(rc)
			if _, err := tr.Next(); err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = io.Copy(io.Discard, tr)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("pull %d: %v", i, err)
		}
	}
	// Sin reintentos: el rename de la segunda no puede encontrarse sin su
	// temporal (lo renombró la primera).
	if l := log.String(); strings.Contains(l, "retrying") {
		t.Errorf("a download was retried:\n%s", l)
	}
	// Y no queda ningún temporal en la caché.
	if m, _ := filepath.Glob(filepath.Join(cache, "sha256", "*.part*")); len(m) > 0 {
		t.Errorf("leftover temporaries: %v", m)
	}
}

type logSeguro struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logSeguro) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logSeguro) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Una capa de la caché que mide lo que debe pero no descomprime (cached() no
// rehashea) se saca de la caché al fallar: la construcción siguiente la baja
// otra vez en vez de tropezar con el mismo CRC para siempre.
func TestCapaDañadaSeSacaDeLaCache(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: strings.Repeat("hola ", 20000)}})
	man, _ := r.Image("arm64", nil, layer)
	ref := r.Host() + "/x/y"
	cache := t.TempDir()
	nuevo := func() *oci.Client {
		return &oci.Client{Cache: cache, Log: io.Discard, Unpack: filepath.Join(t.TempDir(), "u")}
	}
	if _, err := nuevo().Pull(context.Background(), ref, man, "arm64"); err != nil {
		t.Fatal(err)
	}
	// Dañada en la caché después de verificarla: mismo tamaño, un byte del
	// final cambiado (el CRC32 del gzip).
	c := nuevo()
	p := c.BlobPath(r.Put(layer, ""))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-6] ^= 0xff
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Pull(context.Background(), ref, man, "arm64"); err == nil {
		t.Fatal("a damaged cached layer unpacked")
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("the damaged layer is still in the cache: %v", err)
	}
	if _, err := nuevo().Pull(context.Background(), ref, man, "arm64"); err != nil {
		t.Fatalf("after removing it, it is downloaded again: %v", err)
	}
}
