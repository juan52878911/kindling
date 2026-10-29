package guest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/share"
)

func writeIn(fh, off uint64, size int) []byte {
	w := make([]byte, 40)
	le.PutUint64(w[0:], fh)
	le.PutUint64(w[8:], off)
	le.PutUint32(w[16:], uint32(size))
	return w
}

// montarWB es un montaje rw con hello: el daemon anuncia CapWriteBehind.
func montarWB(t *testing.T) (*montaje, string) {
	t.Helper()
	dir := carpeta(t)
	m := montar(t, dir, false)
	m.stop()
	s, stop := m.conectarConHello(t)
	m.stop = stop
	if s.caps&share.CapWriteBehind == 0 {
		t.Fatal("the daemon did not offer write-behind")
	}
	return m, dir
}

// Escrituras de 4 KiB contiguas: vuelven en seguida, se juntan, y lo que pide
// el kernel después (atributos, lectura, flush) ya lo ve todo.
func TestWriteBehindJuntaYDrena(t *testing.T) {
	m, dir := montarWB(t)
	e, out := m.dev.pedir(t, opCreate, rootID, u32s(linuxORdwr|linuxOCreat, 0o644, 0, 0), cstr("w4k"))
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	node, fh := le.Uint64(out[0:]), le.Uint64(out[entryOutSize:])
	const n = 600 // 2,4 MiB: más de un trozo, y con contrapresión
	var want []byte
	for i := 0; i < n; i++ {
		b := bytes.Repeat([]byte{byte('a' + i%26)}, 4096)
		want = append(want, b...)
		if e, out := m.dev.pedir(t, opWrite, node, writeIn(fh, uint64(i*4096), 4096), b); e != 0 || le.Uint32(out) != 4096 {
			t.Fatalf("write %d: %d", i, e)
		}
	}
	// GETATTR espera a lo pendiente: el tamaño es el de todo lo escrito.
	e, out = m.dev.pedir(t, opGetattr, node, u32s(0, 0, 0, 0))
	if e != 0 {
		t.Fatalf("getattr: %d", e)
	}
	if a := leerAttr(out[16:]); a.size != uint64(len(want)) {
		t.Fatalf("getattr size %d, want %d", a.size, len(want))
	}
	e, got := m.dev.pedir(t, opRead, node, readIn(fh, 0, share.MaxIO))
	if e != 0 || !bytes.Equal(got, want[:share.MaxIO]) {
		t.Fatalf("read back: %d, %d bytes", e, len(got))
	}
	if e, _ := m.dev.pedir(t, opFlush, node, u64s(fh, 0, 0)); e != 0 {
		t.Fatalf("flush: %d", e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "w4k")); !bytes.Equal(b, want) {
		t.Fatalf("the host has %d bytes, want %d", len(b), len(want))
	}
	m.dev.pedir(t, opRelease, node, u64s(fh, 0, 0))
}

// Dos escrituras al mismo rango llegan en el orden en que se hicieron, aunque
// la primera esté en vuelo cuando sale la segunda.
func TestWriteBehindRespetaElOrden(t *testing.T) {
	m, dir := montarWB(t)
	e, out := m.dev.pedir(t, opCreate, rootID, u32s(linuxORdwr|linuxOCreat, 0o644, 0, 0), cstr("o"))
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	node, fh := le.Uint64(out[0:]), le.Uint64(out[entryOutSize:])
	for i := 0; i < 20; i++ {
		b := bytes.Repeat([]byte{byte('a' + i)}, share.MaxIO)
		if e, _ := m.dev.pedir(t, opWrite, node, writeIn(fh, 0, len(b)), b); e != 0 {
			t.Fatalf("write %d: %d", i, e)
		}
		small := []byte{byte('A' + i)}
		if e, _ := m.dev.pedir(t, opWrite, node, writeIn(fh, 10, 1), small); e != 0 {
			t.Fatalf("small write %d: %d", i, e)
		}
	}
	if e, _ := m.dev.pedir(t, opFlush, node, u64s(fh, 0, 0)); e != 0 {
		t.Fatalf("flush: %d", e)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "o"))
	if len(b) != share.MaxIO || b[0] != 'a'+19 || b[10] != 'A'+19 || b[11] != 'a'+19 {
		t.Fatalf("host: len %d, b[0]=%q b[10]=%q b[11]=%q", len(b), b[0], b[10], b[11])
	}
	m.dev.pedir(t, opRelease, node, u64s(fh, 0, 0))
}

// Un error del host en un trozo ya contestado llega con el flush (close(2)),
// una sola vez.
func TestWriteBehindErrorEnFlush(t *testing.T) {
	m, _ := montarWB(t)
	_, node, _ := m.lookup(t, rootID, "hello.txt")
	// Abierto solo para leer: el daemon rechaza el write (EBADF), el kernel
	// nunca lo mandaría, pero sirve para provocar un fallo en el host.
	e, fh := m.open(t, node, linuxORdonly)
	if e != 0 {
		t.Fatalf("open: %d", e)
	}
	if e, _ := m.dev.pedir(t, opWrite, node, writeIn(fh, 0, 3), []byte("xyz")); e != 0 {
		t.Fatalf("write (deferred): %d", e)
	}
	if e, _ := m.dev.pedir(t, opFlush, node, u64s(fh, 0, 0)); e != share.EBADF {
		t.Fatalf("flush: %d, want EBADF", e)
	}
	if e, _ := m.dev.pedir(t, opFlush, node, u64s(fh, 0, 0)); e != 0 {
		t.Fatalf("second flush: %d, want 0 (reported once)", e)
	}
	m.dev.pedir(t, opRelease, node, u64s(fh, 0, 0))
}

// Sin hello (un daemon anterior) cada write llega al host antes de volver.
func TestWriteBehindNoConDaemonAnterior(t *testing.T) {
	dir := carpeta(t)
	m := montar(t, dir, false)
	e, out := m.dev.pedir(t, opCreate, rootID, u32s(linuxORdwr|linuxOCreat, 0o644, 0, 0), cstr("s"))
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	node, fh := le.Uint64(out[0:]), le.Uint64(out[entryOutSize:])
	if e, _ := m.dev.pedir(t, opWrite, node, writeIn(fh, 0, 3), []byte("abc")); e != 0 {
		t.Fatalf("write: %d", e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "s")); string(b) != "abc" {
		t.Fatalf("host has %q right after the write, want abc", b)
	}
	m.dev.pedir(t, opRelease, node, u64s(fh, 0, 0))
}
