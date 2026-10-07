package oci

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/internal/oci/ocitest"
)

// verificadaDe deja en <dir>/verified/oci una copia de los blobs de la caché
// de c, como la deja el daemon: directorios 0755 y ficheros 0644, del dueño
// que se espera (en los tests, quien los corre).
func verificadaDe(t *testing.T, c *Client) string {
	t.Helper()
	viejo := dueñoVerificada
	dueñoVerificada = uint32(os.Getuid())
	t.Cleanup(func() { dueñoVerificada = viejo })
	v := filepath.Join(t.TempDir(), "verified", "oci")
	if err := os.MkdirAll(filepath.Join(v, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Dir(v), v, filepath.Join(v, "sha256")} {
		os.Chmod(d, 0o755)
	}
	es, _ := os.ReadDir(filepath.Join(c.Cache, "sha256"))
	for _, e := range es {
		b, err := os.ReadFile(filepath.Join(c.Cache, "sha256", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(v, "sha256", e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return v
}

// Con SiempreRehash (el constructor sin root), un blob de la caché verificada
// se usa tal cual, sin rehashear ni pasar por la caché propia; lo propio se
// sigue rehasheando. Usados dice todo lo que se tocó.
func TestVerificadaSinRehash(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: strings.Repeat("x", 4096)}})
	man, idx := r.Image("arm64", nil, layer)
	ref := r.Host() + "/x/y"
	primero := &Client{Cache: t.TempDir(), Log: io.Discard, SiempreRehash: true}
	if _, err := primero.Pull(context.Background(), ref, idx, "arm64"); err != nil {
		t.Fatal(err)
	}
	if got := primero.Usados(); len(got) != 4 { // índice, manifiesto, config y capa
		t.Fatalf("usados %v", got)
	}
	v := verificadaDe(t, primero)

	// Se cambia la capa verificada sin cambiar su tamaño: si se rehasheara,
	// no se usaría. Que se use es la prueba de que no se rehashea (en la
	// vida real no la puede cambiar nadie más que root).
	capa := filepath.Join(v, "sha256", strings.TrimPrefix(r.Put(layer, ""), "sha256:"))
	b, _ := os.ReadFile(capa)
	os.WriteFile(capa, bytes.Repeat([]byte{'z'}, len(b)), 0o644)

	propia := t.TempDir()
	c := &Client{Cache: propia, Verificada: v, Log: io.Discard, SiempreRehash: true}
	hits := r.Hits
	img, err := c.Pull(context.Background(), ref, man, "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if r.Hits != hits {
		t.Fatalf("con todo verificado, %d peticiones al registro", r.Hits-hits)
	}
	if img.Layers[0].Path != capa {
		t.Fatalf("la capa sale de %s, no de la verificada", img.Layers[0].Path)
	}
	if es, _ := os.ReadDir(filepath.Join(propia, "sha256")); len(es) != 0 {
		t.Fatalf("se escribió en la caché propia: %v", es)
	}
	if got := c.Usados(); len(got) != 3 {
		t.Fatalf("usados %v", got)
	}
}

// Lo que no se puede dar por verificado cuenta como si no estuviera: con
// SiempreRehash se baja a la caché propia.
func TestVerificadaDesconfia(t *testing.T) {
	r := ocitest.New()
	defer r.Close()
	layer := ocitest.TarGz([]ocitest.File{{Name: "a", Body: strings.Repeat("y", 4096)}})
	man, _ := r.Image("arm64", nil, layer)
	ref := r.Host() + "/x/y"
	capaHex := strings.TrimPrefix(r.Put(layer, ""), "sha256:")
	casos := map[string]func(t *testing.T, v string){
		"directorio con escritura para otros": func(t *testing.T, v string) { os.Chmod(filepath.Join(v, "sha256"), 0o777) },
		"padre con escritura para el grupo":   func(t *testing.T, v string) { os.Chmod(filepath.Dir(v), 0o775) },
		"fichero con escritura para otros":    func(t *testing.T, v string) { os.Chmod(filepath.Join(v, "sha256", capaHex), 0o666) },
		"de otro dueño":                       func(t *testing.T, v string) { dueñoVerificada++ },
		"otro tamaño": func(t *testing.T, v string) {
			os.WriteFile(filepath.Join(v, "sha256", capaHex), []byte("corto"), 0o644)
		},
		"un enlace": func(t *testing.T, v string) {
			p := filepath.Join(v, "sha256", capaHex)
			os.Rename(p, p+".real")
			os.Symlink(p+".real", p)
		},
		"sha256 es un enlace": func(t *testing.T, v string) {
			s := filepath.Join(v, "sha256")
			os.Rename(s, s+".real")
			os.Symlink(s+".real", s)
		},
	}
	for nombre, estropear := range casos {
		t.Run(nombre, func(t *testing.T) {
			primero := &Client{Cache: t.TempDir(), Log: io.Discard}
			if _, err := primero.Pull(context.Background(), ref, man, "arm64"); err != nil {
				t.Fatal(err)
			}
			v := verificadaDe(t, primero)
			estropear(t, v)
			propia := t.TempDir()
			c := &Client{Cache: propia, Verificada: v, Log: io.Discard, SiempreRehash: true}
			img, err := c.Pull(context.Background(), ref, man, "arm64")
			if err != nil {
				t.Fatal(err)
			}
			if want := filepath.Join(propia, "sha256", capaHex); img.Layers[0].Path != want {
				t.Fatalf("capa de %s, quiero la propia %s", img.Layers[0].Path, want)
			}
		})
	}
}
