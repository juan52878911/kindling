package ext4

import (
	"archive/tar"
	"bytes"
	"fmt"
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
