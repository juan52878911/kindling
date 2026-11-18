package machine

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/digest"
)

// contarDigests cuenta las lecturas de digestArbol, cada una tardando
// pausa (una imagen grande), mientras dura la prueba.
func contarDigests(t *testing.T, pausa time.Duration) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	antes := digestArbol
	digestArbol = func(p string) (string, error) {
		n.Add(1)
		time.Sleep(pausa)
		return digest.Tree(p)
	}
	t.Cleanup(func() { digestArbol = antes })
	return &n
}

// P5: N run -from simultáneos tras arrancar el daemon leen la imagen una
// vez, no N.
func TestDigestDiscoBaseUnoPorRuta(t *testing.T) {
	m := newTestManager(t)
	img := filepath.Join(t.TempDir(), "img.ext4")
	if err := os.WriteFile(img, []byte("imagen"), 0o644); err != nil {
		t.Fatal(err)
	}
	n := contarDigests(t, 50*time.Millisecond)
	const N = 8
	var wg sync.WaitGroup
	hashes := make([]string, N)
	for i := range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := m.digestDiscoBase(img)
			if err != nil {
				t.Error(err)
			}
			hashes[i] = h
		}()
	}
	wg.Wait()
	if got := n.Load(); got != 1 {
		t.Fatalf("%d lecturas de la imagen para %d peticiones simultáneas; quería 1", got, N)
	}
	for _, h := range hashes {
		if h != hashes[0] || h == "" {
			t.Fatalf("digests distintos: %v", hashes)
		}
	}
	// Cambiada la imagen, la siguiente vuelve a leerla.
	futuro := time.Now().Add(time.Hour)
	if err := os.Chtimes(img, futuro, futuro); err != nil {
		t.Fatal(err)
	}
	if _, err := m.digestDiscoBase(img); err != nil || n.Load() != 2 {
		t.Fatalf("con la imagen cambiada: %d lecturas, %v", n.Load(), err)
	}
}

// El arranque del daemon deja calculados los digests de las imágenes de sus
// dorados: el primer run -from ya no lee la imagen.
func TestPrecalentarDiscosBase(t *testing.T) {
	m := annotTestManager(t)
	doradoEnDisco(t, m, "svc", "v3")
	doradoEnDisco(t, m, "otro", "v3")
	m.discosBase.Clear()
	n := contarDigests(t, 0)
	m.precalentarDiscosBase(t.Context())
	if got := n.Load(); got != 1 {
		t.Fatalf("%d lecturas para una imagen con dos dorados; quería 1", got)
	}
	if _, _, err := m.digestsDeImagen("toolchain"); err != nil || n.Load() != 1 {
		t.Fatalf("tras precalentar, el digest se volvió a leer (%d, %v)", n.Load(), err)
	}
}

// Medición de P5: 8 run -from simultáneos sobre una imagen de 256 MiB con
// datos, con la caché fría. go test -run x -bench DigestDiscoBaseFrio.
func BenchmarkDigestDiscoBaseFrio(b *testing.B) {
	dir := b.TempDir()
	img := filepath.Join(dir, "img.ext4")
	f, err := os.Create(img)
	if err != nil {
		b.Fatal(err)
	}
	bloque := make([]byte, 1<<20)
	for i := range bloque {
		bloque[i] = byte(i)
	}
	for range 256 {
		if _, err := f.Write(bloque); err != nil {
			b.Fatal(err)
		}
	}
	f.Close()
	for b.Loop() {
		m := &Manager{}
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := m.digestDiscoBase(img); err != nil {
					b.Error(err)
				}
			}()
		}
		wg.Wait()
	}
}
