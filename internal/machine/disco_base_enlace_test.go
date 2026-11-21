package machine

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func nuevaImagen(t *testing.T) (*Manager, string) {
	t.Helper()
	m := newTestManager(t)
	// Los tests no corren como root: se da por buena la huella salvo en el
	// test que prueba lo contrario.
	antes := huellaConfiable
	huellaConfiable = func(os.FileInfo) bool { return true }
	t.Cleanup(func() { huellaConfiable = antes })
	img := filepath.Join(t.TempDir(), "img.ext4")
	if err := os.WriteFile(img, []byte("imagen"), 0o644); err != nil {
		t.Fatal(err)
	}
	return m, img
}

func enlazarYBorrar(m *Manager, img, otro string) error {
	return m.enlaceEstable(img, func() error {
		if err := os.Link(img, otro); err != nil {
			return err
		}
		return os.Remove(otro)
	})
}

// Hipótesis 1.1: enlazar y desenlazar la imagen (cada restauración con
// jailer) movía su ctime y obligaba a rehashearla; con enlaceEstable no.
func TestEnlaceEstableConservaDigest(t *testing.T) {
	m, img := nuevaImagen(t)
	n := contarDigests(t, 0)
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	for i := range 3 {
		if err := enlazarYBorrar(m, img, img+".lnk"); err != nil {
			t.Fatal(i, err)
		}
		if _, err := m.digestDiscoBase(img); err != nil {
			t.Fatal(err)
		}
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("lecturas = %d, quiero 1", got)
	}
}

func TestEnlaceEstableConcurrente(t *testing.T) {
	m, img := nuevaImagen(t)
	n := contarDigests(t, 0)
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := enlazarYBorrar(m, img, img+"."+strconv.Itoa(i)); err != nil {
				t.Error(err)
			}
			if _, err := m.digestDiscoBase(img); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := n.Load(); got != 1 {
		t.Fatalf("lecturas = %d, quiero 1", got)
	}
}

// Una escritura real del mismo tamaño con el mtime devuelto entre dos
// enlaces mueve el ctime y debe rehashear.
func TestEnlaceEstableEscrituraRealRehashea(t *testing.T) {
	m, img := nuevaImagen(t)
	n := contarDigests(t, 0)
	h1, _ := m.digestDiscoBase(img)
	st, _ := os.Stat(img)
	if err := enlazarYBorrar(m, img, img+".lnk"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(img, []byte("IMAGEN"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(img, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := enlazarYBorrar(m, img, img+".lnk"); err != nil {
		t.Fatal(err)
	}
	h2, err := m.digestDiscoBase(img)
	if err != nil || h2 == h1 || n.Load() != 2 {
		t.Fatalf("h1=%s h2=%s err=%v lecturas=%d", h1, h2, err, n.Load())
	}
}

// Un rename de otro fichero encima (otro inodo) también rehashea.
func TestEnlaceEstableRenameRehashea(t *testing.T) {
	m, img := nuevaImagen(t)
	n := contarDigests(t, 0)
	h1, _ := m.digestDiscoBase(img)
	err := m.enlaceEstable(img, func() error {
		nuevo := img + ".new"
		if err := os.WriteFile(nuevo, []byte("IMAGEN"), 0o644); err != nil {
			return err
		}
		st, _ := os.Stat(img)
		_ = os.Chtimes(nuevo, st.ModTime(), st.ModTime())
		return os.Rename(nuevo, img)
	})
	if err != nil {
		t.Fatal(err)
	}
	h2, _ := m.digestDiscoBase(img)
	if h2 == h1 || n.Load() != 2 {
		t.Fatalf("h1=%s h2=%s lecturas=%d", h1, h2, n.Load())
	}
}

func TestEnlaceEstableOpFallidaNoAnota(t *testing.T) {
	m, img := nuevaImagen(t)
	n := contarDigests(t, 0)
	_, _ = m.digestDiscoBase(img)
	time.Sleep(10 * time.Millisecond)
	errX := errors.New("boom")
	err := m.enlaceEstable(img, func() error {
		if e := os.Link(img, img+".lnk"); e != nil {
			return e
		}
		return errX
	})
	if !errors.Is(err, errX) {
		t.Fatal(err)
	}
	_, _ = m.digestDiscoBase(img)
	if n.Load() != 2 {
		t.Fatalf("lecturas = %d, quiero 2 (la op fallida no anota)", n.Load())
	}
}

// Un op que reescribe el contenido y restaura el mtime no debe conservar el
// digest cuando el fichero no es de confianza (no es de root o escribible).
func TestEnlaceEstableInvalidaSiOpReescribe(t *testing.T) {
	m, img := nuevaImagen(t)
	antes := huellaConfiable
	huellaConfiable = func(os.FileInfo) bool { return false }
	t.Cleanup(func() { huellaConfiable = antes })
	n := contarDigests(t, 0)
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(img)
	time.Sleep(20 * time.Millisecond) // que el ctime cambie de tick
	err := m.enlaceEstable(img, func() error {
		if err := os.WriteFile(img, []byte("OTRA!!"), 0o644); err != nil {
			return err
		}
		return os.Chtimes(img, fi.ModTime(), fi.ModTime())
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.digestDiscoBase(img); err != nil {
		t.Fatal(err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("lecturas = %d, quiero 2 (la huella debía invalidarse)", got)
	}
}
