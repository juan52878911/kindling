package digest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// referencia hace el digest en árbol a mano, leyendo el fichero entero: lo
// que Tree tiene que dar salte huecos o no.
func referencia(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	top := sha256.New()
	top.Write([]byte("kindling " + TreePrefix + "\n"))
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(len(b)))
	top.Write(sz[:])
	for off := 0; off < len(b); off += treeChunk {
		s := sha256.Sum256(b[off:min(off+treeChunk, len(b))])
		top.Write(s[:])
	}
	return TreePrefix + hex.EncodeToString(top.Sum(nil))
}

// disperso crea un fichero de size bytes, todo hueco salvo los datos en sus
// posiciones.
func disperso(t *testing.T, size int64, datos map[int64]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for off, s := range datos {
		if _, err := f.WriteAt([]byte(s), off); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func TestTreeIgualQueLaReferencia(t *testing.T) {
	casos := map[string]struct {
		size  int64
		datos map[int64]string
	}{
		"vacío":             {0, nil},
		"todo hueco":        {64 << 20, nil},
		"datos sueltos":     {64 << 20, map[int64]string{0: "ext4", 10<<20 + 7: "x", 63<<20 + 5: "fin"}},
		"cruza dos trozos":  {16 << 20, map[int64]string{treeChunk - 2: "abcd"}},
		"último a medias":   {10<<20 + 123, map[int64]string{10<<20 + 100: "cola"}},
		"menos de un trozo": {1000, map[int64]string{3: "hola"}},
	}
	for nombre, c := range casos {
		p := disperso(t, c.size, c.datos)
		got, err := Tree(p)
		if err != nil {
			t.Fatal(err)
		}
		if want := referencia(t, p); got != want {
			t.Errorf("%s: Tree = %s, reference %s", nombre, got, want)
		}
	}
}

// Lo que cuenta es el contenido: ceros escritos de verdad dan lo mismo que un
// hueco, y un byte distinto en lo que era hueco cambia el digest.
func TestTreeContenido(t *testing.T) {
	hueco := disperso(t, 32<<20, map[int64]string{0: "a"})
	ceros := disperso(t, 32<<20, map[int64]string{0: "a", 8 << 20: strings.Repeat("\x00", treeChunk)})
	otro := disperso(t, 32<<20, map[int64]string{0: "a", 20 << 20: "!"})
	h1, _ := Tree(hueco)
	h2, _ := Tree(ceros)
	h3, _ := Tree(otro)
	if h1 != h2 {
		t.Errorf("explicit zeros and a hole must hash the same: %s vs %s", h1, h2)
	}
	if h1 == h3 {
		t.Error("a byte written into a hole didn't change the digest")
	}
}

func TestMatches(t *testing.T) {
	p := disperso(t, 5<<20, map[int64]string{1: "z"})
	plano, _ := File(p)
	arbol, _ := Tree(p)
	for _, want := range []string{plano, arbol} {
		if _, ok, err := Matches(p, want); err != nil || !ok {
			t.Errorf("Matches(%s) = %v, %v", want, ok, err)
		}
	}
	if _, ok, _ := Matches(p, TreePrefix+strings.Repeat("0", 64)); ok {
		t.Error("a wrong tree digest matched")
	}
	if _, _, err := Matches(p, "blake9:abc"); err == nil {
		t.Error("an unknown algorithm has to be an error, not a mismatch")
	}
}

func BenchmarkTree512MiBDisperso(b *testing.B) {
	p := filepath.Join(b.TempDir(), "f")
	f, _ := os.Create(p)
	f.Truncate(512 << 20)
	buf := make([]byte, 40<<20)
	for i := range buf {
		buf[i] = byte(i)
	}
	f.WriteAt(buf, 0)
	f.Close()
	b.SetBytes(512 << 20)
	for i := 0; i < b.N; i++ {
		Tree(p)
	}
}
