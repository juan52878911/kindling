package ext4

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
)

func tarDe(t *testing.T, n int) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for i := 0; i < n; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: fmt.Sprintf("f%d", i), Mode: 0o644, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	return &b
}

// El tope de entradas se comparte entre capas y corta en cuanto se pasa.
func TestAddTarMaxEntries(t *testing.T) {
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	var n int64
	o := TarOptions{Entries: &n, MaxEntries: 5}
	if err := root.AddTar(tarDe(t, 3), o); err != nil {
		t.Fatal(err)
	}
	err := root.AddTar(tarDe(t, 3), o)
	if err == nil || !strings.Contains(err.Error(), "more than 5") {
		t.Fatalf("segunda capa por encima del tope: %v", err)
	}
	if n != 6 {
		t.Fatalf("contó %d entradas, quiero que pare en la 6.ª", n)
	}
}

// Sin tope propio (ni contador), AddTar pone defaultMaxEntries.
func TestAddTarMaxEntriesPorDefecto(t *testing.T) {
	defer func(v int64) { defaultMaxEntries = v }(defaultMaxEntries)
	defaultMaxEntries = 4
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	if err := root.AddTar(tarDe(t, 4), TarOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := root.AddTar(tarDe(t, 5), TarOptions{}); err == nil || !strings.Contains(err.Error(), "more than 4") {
		t.Fatalf("5 entradas con el tope por defecto en 4: %v", err)
	}
}

// lectorConFinal da r y, al acabarse, err en vez de io.EOF: lo que hace el
// lector de una capa OCI cuyo diff_id no casa.
type lectorConFinal struct {
	r   io.Reader
	err error
}

func (l *lectorConFinal) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err == io.EOF {
		err = l.err
	}
	return n, err
}

// AddTar lee hasta el final del flujo, no solo hasta el fin del tar: el
// error que da el lector ahí (un diff_id que no casa) sale de AddTar. Y lo
// que sigue al fin del tar tiene tope.
func TestAddTarDrenaHastaElFinal(t *testing.T) {
	root := NewDir(0o755, 0, 0, time.Unix(0, 0))
	falla := errors.New("the layer unpacks to sha256:x, but the config says its diff_id is sha256:y")
	if err := root.AddTar(&lectorConFinal{tarDe(t, 2), falla}, TarOptions{}); !errors.Is(err, falla) {
		t.Fatalf("el error del final del flujo: %v", err)
	}
	cola := io.MultiReader(tarDe(t, 1), io.LimitReader(zeros{}, maxCola+1))
	if err := root.AddTar(cola, TarOptions{}); err == nil || !strings.Contains(err.Error(), "after the end of the tar") {
		t.Fatalf("%d MiB después del tar: %v", maxCola>>20, err)
	}
	if err := root.AddTar(io.MultiReader(tarDe(t, 1), bytes.NewReader(make([]byte, 10240))), TarOptions{}); err != nil {
		t.Fatalf("relleno normal después del tar: %v", err)
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
