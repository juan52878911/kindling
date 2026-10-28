package guest

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/share"
)

// conectarConHello es conectar haciendo lo que hace el attach de verdad:
// negociar el tamaño de E/S antes de publicar la sesión.
func (m *montaje) conectarConHello(t *testing.T) (*shareSession, func()) {
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = m.srv.Serve(ctx, a); close(done) }()
	s := newShareSession(b)
	s.hello()
	m.fs.attach(s)
	return s, func() { cancel(); b.Close(); <-done }
}

// El kernel pide hasta share.MaxIO por operación (max_write/max_read). Con un
// daemon que solo habla BaseIO —sin hello, como uno anterior— el agente
// trocea; con hello, va de una vez. En los dos casos lo leído y lo escrito es
// lo mismo.
func TestFUSEIOGrandeTroceado(t *testing.T) {
	for _, conHello := range []bool{false, true} {
		dir := carpeta(t)
		m := montar(t, dir, false)
		if conHello {
			m.stop()
			s, stop := m.conectarConHello(t)
			m.stop = stop
			if s.maxIO != share.MaxIO {
				t.Fatalf("hello negotiated %d, want %d", s.maxIO, share.MaxIO)
			}
		}

		datos := make([]byte, share.MaxIO)
		for i := range datos {
			datos[i] = byte(i * 7)
		}
		e, out := m.dev.pedir(t, opCreate, rootID, u32s(linuxORdwr|linuxOCreat, 0o644, 0, 0), cstr("big.bin"))
		if e != 0 {
			t.Fatalf("create: %d", e)
		}
		node, fh := le.Uint64(out[0:]), le.Uint64(out[entryOutSize:])
		w := make([]byte, 40)
		le.PutUint64(w[0:], fh)
		le.PutUint32(w[16:], uint32(len(datos)))
		if e, out := m.dev.pedir(t, opWrite, node, w, datos); e != 0 || le.Uint32(out) != uint32(len(datos)) {
			t.Fatalf("hello=%v write: %d", conHello, e)
		}
		if b, _ := os.ReadFile(filepath.Join(dir, "big.bin")); !bytes.Equal(b, datos) {
			t.Fatalf("hello=%v: the host sees %d bytes, not what was written", conHello, len(b))
		}
		// Lectura entera y lectura que cruza el final del fichero.
		e, got := m.dev.pedir(t, opRead, node, readIn(fh, 0, share.MaxIO))
		if e != 0 || !bytes.Equal(got, datos) {
			t.Fatalf("hello=%v read: %d, %d bytes", conHello, e, len(got))
		}
		off := uint64(len(datos) - 1000)
		e, got = m.dev.pedir(t, opRead, node, readIn(fh, off, share.MaxIO))
		if e != 0 || !bytes.Equal(got, datos[off:]) {
			t.Fatalf("hello=%v read past EOF: %d, %d bytes", conHello, e, len(got))
		}
		m.dev.pedir(t, opRelease, node, u64s(fh, 0, 0))
	}
}

// En un montaje ro se conserva la caché de páginas al abrir SOLO si el fichero
// no cambió desde el open anterior; en rw, nunca.
func TestFUSEKeepCache(t *testing.T) {
	for _, ro := range []bool{true, false} {
		dir := carpeta(t)
		m := montar(t, dir, false)
		m.fs.readOnly = ro
		_, node, _ := m.lookup(t, rootID, "hello.txt")
		abrir := func() bool {
			t.Helper()
			e, out := m.dev.pedir(t, opOpen, node, u32s(linuxORdonly, 0))
			if e != 0 {
				t.Fatalf("open: %d", e)
			}
			m.dev.pedir(t, opRelease, node, u64s(le.Uint64(out), 0, 0))
			return le.Uint32(out[8:])&fopenKeepCache != 0
		}
		if abrir() {
			t.Errorf("ro=%v: the first open kept the cache", ro)
		}
		if got := abrir(); got != ro {
			t.Errorf("ro=%v: unchanged reopen keep_cache=%v", ro, got)
		}
		// El host cambia el fichero (mismo tamaño): el open siguiente tira la
		// caché, y el de después ya puede conservarla.
		p := filepath.Join(dir, "hello.txt")
		if err := os.WriteFile(p, []byte("HELLO FROM THE HOST\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cuando := time.Now().Add(time.Hour)
		if err := os.Chtimes(p, cuando, cuando); err != nil {
			t.Fatal(err)
		}
		if abrir() {
			t.Errorf("ro=%v: kept the cache of a file the host changed", ro)
		}
		if got := abrir(); got != ro {
			t.Errorf("ro=%v: reopen after the change keep_cache=%v", ro, got)
		}
	}
}

// Un daemon anterior no conoce OpHello y contesta ENOSYS: la sesión se queda
// en BaseIO, que es lo que él acepta.
func TestHelloConDaemonAnterior(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() {
		for {
			body, err := share.ReadFrame(a)
			if err != nil {
				return
			}
			d := share.NewDec(body)
			id := d.U64()
			if err := share.WriteFrame(a, share.Response(id, share.ENOSYS).B); err != nil {
				return
			}
		}
	}()
	s := newShareSession(b)
	defer s.close()
	s.hello()
	if s.maxIO != share.BaseIO {
		t.Fatalf("maxIO = %d, want BaseIO %d", s.maxIO, share.BaseIO)
	}
	if !s.alive() {
		t.Fatal("ENOSYS closed the session")
	}
}
