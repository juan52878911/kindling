package xz

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// FuzzReader comprueba que ninguna entrada haga entrar en pánico al
// descompresor ni le haga reservar memoria sin tope. Las semillas son los
// ficheros buenos de testdata; los casos que fallaron están en
// testdata/fuzz/FuzzReader y go test los repite siempre.
func FuzzReader(f *testing.F) {
	files, _ := filepath.Glob("testdata/*.xz")
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := NewReader(bytes.NewReader(b))
		if err != nil {
			return
		}
		// Tope de salida: una bomba de compresión válida no es un fallo,
		// pero no hace falta descomprimirla entera.
		io.Copy(io.Discard, io.LimitReader(r, 64<<20))
	})
}
