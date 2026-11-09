package durable

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func hexDe(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// lento da b a trozos con una pausa: dos lectores a la vez se solapan.
type lento struct {
	b []byte
}

func (l *lento) Read(p []byte) (int, error) {
	if len(l.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, l.b[:min(len(l.b), 4096)])
	l.b = l.b[n:]
	time.Sleep(200 * time.Microsecond)
	return n, nil
}

func temporales(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		if strings.Contains(e.Name(), ".part") || strings.Contains(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestEscribirVerificadoPublicaSoloLoQueCuadra(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "blob")
	b := bytes.Repeat([]byte("kindling"), 10000)
	sum := hexDe(b)

	// Con el prefijo de los digests OCI o sin él.
	if err := EscribirVerificado(dst, bytes.NewReader(b), "sha256:"+sum, int64(len(b))); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, b) {
		t.Fatal("wrong content")
	}
	if st, _ := os.Stat(dst); st.Mode().Perm() != 0o644 {
		t.Fatalf("perm %v, want 0644", st.Mode().Perm())
	}

	otro := filepath.Join(dir, "otro")
	var eh *ErrSHA256
	if err := EscribirVerificado(otro, bytes.NewReader(b[1:]), sum, 0); !errors.As(err, &eh) || eh.Want != sum {
		t.Fatalf("hash mismatch: %v", err)
	}
	var et *ErrTamaño
	if err := EscribirVerificado(otro, bytes.NewReader(b), sum, int64(len(b))-1); !errors.As(err, &et) {
		t.Fatalf("longer than declared: %v", err)
	}
	if err := EscribirVerificado(otro, bytes.NewReader(b[:10]), sum, int64(len(b))); !errors.As(err, &et) || et.N != 10 {
		t.Fatalf("shorter than declared: %v", err)
	}
	if err := EscribirVerificado(otro, bytes.NewReader(b), "", 0); err == nil {
		t.Fatal("no sha256 to check against, and it wrote anyway")
	}
	var el *ErrLectura
	if err := EscribirVerificado(otro, io.MultiReader(bytes.NewReader(b[:10]), &falla{}), sum, 0); !errors.As(err, &el) {
		t.Fatalf("read error: %v", err)
	}
	if _, err := os.Stat(otro); !os.IsNotExist(err) {
		t.Fatalf("a failed write left %s: %v", otro, err)
	}
	if tmp := temporales(t, dir); len(tmp) > 0 {
		t.Fatalf("leftover temporaries: %v", tmp)
	}
}

type falla struct{}

func (falla) Read([]byte) (int, error) { return 0, errors.New("connection reset") }

// Dos escritores del mismo dst a la vez (dos construcciones que bajan el
// mismo blob) no se pisan el temporal: los dos acaban bien y sin rastro.
func TestEscribirVerificadoConcurrente(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "blob")
	b := bytes.Repeat([]byte("0123456789abcdef"), 16<<10)
	sum := hexDe(b)
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = EscribirVerificado(dst, &lento{b: b}, sum, int64(len(b)))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, b) {
		t.Fatal("published content is not the verified one")
	}
	if tmp := temporales(t, dir); len(tmp) > 0 {
		t.Fatalf("leftover temporaries: %v", tmp)
	}
}

// Recibir deja el temporal verificado sin publicarlo, con permisos y lo que
// haga Preparar, para quien decide el rename bajo un cerrojo propio.
func TestRecibirYPublicarODescartar(t *testing.T) {
	dir := t.TempDir()
	b := []byte("hola")
	var visto string
	r, err := Recibir(dir, bytes.NewReader(b), Opciones{
		Max: 10, Perm: 0o600, Patron: ".x.tmp-*",
		Preparar: func(f *os.File) error { visto = f.Name(); return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.N != 4 || r.SHA256 != hexDe(b) || visto != r.Ruta || !strings.HasPrefix(filepath.Base(r.Ruta), ".x.tmp-") {
		t.Fatalf("%+v (prepared %q)", r, visto)
	}
	dst := filepath.Join(dir, "x")
	if err := r.Publicar(dst); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dst); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", st, err)
	}
	r, err = Recibir(dir, bytes.NewReader(b), Opciones{})
	if err != nil {
		t.Fatal(err)
	}
	r.Descartar()
	if tmp := temporales(t, dir); len(tmp) > 0 {
		t.Fatalf("Descartar left %v", tmp)
	}
	// Max sin Size: tope, no tamaño exacto.
	var ed *ErrDemasiado
	if _, err := Recibir(dir, bytes.NewReader(b), Opciones{Max: 3}); !errors.As(err, &ed) || ed.Max != 3 {
		t.Fatalf("over Max: %v", err)
	}
	// Un Preparar que falla no deja el temporal.
	if _, err := Recibir(dir, bytes.NewReader(b), Opciones{Preparar: func(*os.File) error { return errors.New("chown") }}); err == nil {
		t.Fatal("Preparar's error was dropped")
	}
	if tmp := temporales(t, dir); len(tmp) > 0 {
		t.Fatalf("leftover temporaries: %v", tmp)
	}
}
