package machine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// La copia desde un descriptor conserva el contenido, el tamaño lógico y la
// dispersión: ni los huecos ni los bloques a cero ocupan en el destino.
func TestCopiarDisperso(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	const tam = 64 << 20
	f, err := os.Create(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(tam); err != nil {
		t.Fatal(err)
	}
	trozos := map[int64][]byte{
		0:             bytes.Repeat([]byte("a"), 10000),
		5<<20 + 123:   []byte("en medio de un bloque"),
		20 << 20:      make([]byte, 8<<20), // datos escritos, pero a cero
		40<<20 + 4096: bytes.Repeat([]byte{0xff}, 3<<20),
		tam - 3:       []byte("fin"),
	}
	for off, b := range trozos {
		if _, err := f.WriteAt(b, off); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()

	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	dst := filepath.Join(d, "dst")
	out, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := copiarDisperso(context.Background(), in, out); err != nil {
		t.Fatal(err)
	}
	out.Close()

	a, _ := os.ReadFile(src)
	b, _ := os.ReadFile(dst)
	if len(b) != tam || !bytes.Equal(a, b) {
		t.Fatalf("contenido distinto (len %d)", len(b))
	}
	var st syscall.Stat_t
	if err := syscall.Stat(dst, &st); err != nil {
		t.Fatal(err)
	}
	// Datos de verdad: ~3 MiB y unos pocos bloques; los 8 MiB a cero no se
	// escriben. El margen es por APFS, que reserva de más según cómo lleguen
	// las escrituras (ext4 y XFS se quedan en ~3 MiB); lo que no puede es
	// llegar a los 11 MiB de escribir también los ceros.
	if ocupa := int64(st.Blocks) * 512; ocupa >= 10<<20 {
		t.Errorf("el destino ocupa %d KiB: escribió los ceros o los huecos", ocupa>>10)
	}
}

// Un fichero vacío, uno sin huecos y uno todo a cero también.
func TestCopiarDispersoBordes(t *testing.T) {
	d := t.TempDir()
	for nombre, contenido := range map[string][]byte{
		"vacio": {},
		"denso": bytes.Repeat([]byte("xyz"), 1<<20),
		"ceros": make([]byte, 1<<20),
	} {
		src := filepath.Join(d, nombre)
		if err := os.WriteFile(src, contenido, 0o600); err != nil {
			t.Fatal(err)
		}
		in, err := os.Open(src)
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.Create(src + ".copia")
		if err != nil {
			t.Fatal(err)
		}
		if err := copiarDisperso(context.Background(), in, out); err != nil {
			t.Fatalf("%s: %v", nombre, err)
		}
		in.Close()
		out.Close()
		if b, _ := os.ReadFile(src + ".copia"); !bytes.Equal(b, contenido) {
			t.Errorf("%s: contenido distinto (%d bytes, quería %d)", nombre, len(b), len(contenido))
		}
	}
}

// Una copia cancelada se para.
func TestCopiarDispersoCancelado(t *testing.T) {
	d := t.TempDir()
	src := filepath.Join(d, "src")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(filepath.Join(d, "dst"))
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := copiarDisperso(ctx, in, out); err == nil {
		t.Error("siguió copiando con el contexto cancelado")
	}
}
