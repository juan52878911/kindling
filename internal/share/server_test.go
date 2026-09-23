package share

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// cliente es un agente mínimo: manda una petición y espera su respuesta.
type cliente struct {
	t    *testing.T
	conn net.Conn
	mu   sync.Mutex
	id   uint64
}

func servir(t *testing.T, dir string, ro bool) *cliente {
	t.Helper()
	srv, err := Open(dir, ro)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, a); close(done) }()
	t.Cleanup(func() {
		cancel()
		b.Close()
		<-done
		srv.Close()
	})
	return &cliente{t: t, conn: b}
}

func (c *cliente) call(op byte, build func(*proto.Enc)) (uint32, *proto.Dec) {
	c.t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	e := proto.Request(c.id, op)
	if build != nil {
		build(e)
	}
	if err := proto.WriteFrame(c.conn, e.B); err != nil {
		c.t.Fatal(err)
	}
	body, err := proto.ReadFrame(c.conn)
	if err != nil {
		c.t.Fatal(err)
	}
	d := proto.NewDec(body)
	if id := d.U64(); id != c.id {
		c.t.Fatalf("response id %d, want %d", id, c.id)
	}
	return d.U32(), d
}

func (c *cliente) stat(p string) (uint32, proto.Attr) {
	e, d := c.call(proto.OpStat, func(e *proto.Enc) { e.Str(p) })
	if e != 0 {
		return e, proto.Attr{}
	}
	return 0, proto.GetAttr(d)
}

func (c *cliente) open(p string, flags uint32) (uint32, uint64) {
	e, d := c.call(proto.OpOpen, func(e *proto.Enc) { e.Str(p); e.U32(flags) })
	if e != 0 {
		return e, 0
	}
	return 0, d.U64()
}

func (c *cliente) create(p string, mode uint32) (uint32, uint64) {
	e, d := c.call(proto.OpCreate, func(e *proto.Enc) { e.Str(p); e.U32(proto.FlagRead | proto.FlagWrite | proto.FlagExcl); e.U32(mode) })
	if e != 0 {
		return e, 0
	}
	return 0, d.U64()
}

func (c *cliente) path(op byte, p string) uint32 {
	e, _ := c.call(op, func(e *proto.Enc) { e.Str(p) })
	return e
}

func (c *cliente) mkdir(p string) uint32 {
	e, _ := c.call(proto.OpMkdir, func(e *proto.Enc) { e.Str(p); e.U32(0o755) })
	return e
}

func (c *cliente) rename(a, b string) uint32 {
	e, _ := c.call(proto.OpRename, func(e *proto.Enc) { e.Str(a); e.Str(b); e.U32(0) })
	return e
}

// arbol prepara una carpeta con trampas: un enlace a /etc, uno absoluto, uno
// relativo que sube por encima de la raíz y un secreto fuera de ella.
func arbol(t *testing.T) (dir, fuera string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "share")
	fuera = filepath.Join(base, "secret.txt")
	must(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from the host\n"), 0o644))
	must(t, os.WriteFile(fuera, []byte("host secret"), 0o600))
	must(t, os.Symlink("/etc", filepath.Join(dir, "etc")))
	must(t, os.Symlink(fuera, filepath.Join(dir, "abs")))
	must(t, os.Symlink("../secret.txt", filepath.Join(dir, "up")))
	must(t, os.Symlink("hello.txt", filepath.Join(dir, "inside")))
	return dir, fuera
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestLeerYListar(t *testing.T) {
	dir, _ := arbol(t)
	must(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644))
	c := servir(t, dir, true)

	if e, a := c.stat("hello.txt"); e != 0 || a.Mode&proto.SIFMT != proto.SIFREG || a.Size != 20 {
		t.Fatalf("stat hello.txt: errno %d attr %+v", e, a)
	}
	if e, a := c.stat(""); e != 0 || a.Mode&proto.SIFMT != proto.SIFDIR {
		t.Fatalf("stat root: errno %d attr %+v", e, a)
	}
	e, h := c.open("hello.txt", proto.FlagRead)
	if e != 0 {
		t.Fatalf("open: %d", e)
	}
	e, d := c.call(proto.OpRead, func(e *proto.Enc) { e.U64(h); e.U64(6); e.U32(100) })
	if e != 0 || string(d.Bytes(100)) != "from the host\n" {
		t.Fatalf("read: %d", e)
	}

	// El FIFO no se enseña: ni en el listado, ni por stat, ni por open.
	e, d = c.call(proto.OpList, func(e *proto.Enc) { e.Str(""); e.U64(0); e.U32(0) })
	if e != 0 {
		t.Fatalf("list: %d", e)
	}
	eof, n := d.U8(), d.U32()
	var names []string
	for i := uint32(0); i < n; i++ {
		names = append(names, d.Str())
		d.U32()
	}
	if eof != 1 || d.U64() == 0 {
		t.Fatalf("list not complete")
	}
	got := strings.Join(names, ",")
	for _, want := range []string{"hello.txt", "sub", "etc", "abs", "up", "inside"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing %q lacks %s", got, want)
		}
	}
	if strings.Contains(got, "fifo") {
		t.Errorf("the FIFO is listed: %s", got)
	}
	if e, _ := c.stat("fifo"); e != proto.ENOENT {
		t.Errorf("stat fifo = %d, want ENOENT", e)
	}
	if e, _ := c.open("fifo", proto.FlagRead); e == 0 {
		t.Errorf("a FIFO was opened")
	}
}

// TestNoSeEscapa: ninguna operación sale de la carpeta, ni por rutas sucias ni
// por enlaces del host.
func TestNoSeEscapa(t *testing.T) {
	dir, fuera := arbol(t)
	c := servir(t, dir, false)

	// Rutas que no siguen el protocolo: se rechazan sin mirar el disco.
	for _, p := range []string{"..", "../secret.txt", "sub/../../secret.txt", "/etc/passwd", "sub/", "a//b", "./hello.txt", "x\x00y"} {
		if e, _ := c.stat(p); e != proto.EINVAL {
			t.Errorf("stat %q = %d, want EINVAL", p, e)
		}
	}
	if e, _ := c.stat(strings.Repeat("a", 256)); e != proto.EINVAL {
		t.Errorf("a 256-byte component was accepted")
	}

	// Los enlaces se ven como enlaces, y readlink los lee sin seguirlos.
	if e, a := c.stat("etc"); e != 0 || a.Mode&proto.SIFMT != proto.SIFLNK {
		t.Fatalf("stat etc: %d %+v", e, a)
	}
	e, d := c.call(proto.OpReadlink, func(e *proto.Enc) { e.Str("etc") })
	if e != 0 || d.Str() != "/etc" {
		t.Fatalf("readlink etc: %d", e)
	}

	// Pero nada los atraviesa hacia fuera.
	for _, p := range []string{"etc/passwd", "etc/hosts", "abs", "up"} {
		if e, _ := c.open(p, proto.FlagRead); e == 0 {
			t.Errorf("open %q succeeded: it reads outside the share", p)
		}
	}
	for _, p := range []string{"etc/passwd", "etc"} {
		if e, _ := c.call(proto.OpList, func(e *proto.Enc) { e.Str(p); e.U64(0); e.U32(0) }); e == 0 {
			t.Errorf("list %q succeeded", p)
		}
	}
	if e, _ := c.stat("etc/passwd"); e == 0 {
		t.Errorf("stat through the /etc symlink succeeded")
	}
	// Crear, escribir o renombrar a través de un enlace tampoco.
	if e, _ := c.create("etc/kling-evil", 0o644); e == 0 {
		t.Errorf("created a file through the /etc symlink")
	}
	if e := c.rename("hello.txt", "etc/kling-evil"); e == 0 {
		t.Errorf("renamed a file into /etc through a symlink")
	}
	if e := c.rename("hello.txt", "../moved"); e != proto.EINVAL {
		t.Errorf("rename out of the root = %d, want EINVAL", e)
	}
	if e := c.mkdir("etc/kling-dir"); e == 0 {
		t.Errorf("mkdir through a symlink")
	}
	// Truncar el secreto a través del enlace: nada.
	e, _ = c.call(proto.OpSetattr, func(e *proto.Enc) {
		e.Str("up")
		e.U64(0)
		e.U32(proto.SetSize)
		e.U32(0)
		e.U64(0)
		e.I64(0)
		e.I64(0)
	})
	if e == 0 {
		t.Errorf("setattr size through a symlink succeeded")
	}
	// Borrar un enlace borra el enlace, no lo que apunta.
	if e := c.path(proto.OpUnlink, "abs"); e != 0 {
		t.Fatalf("unlink abs: %d", e)
	}
	if b, err := os.ReadFile(fuera); err != nil || string(b) != "host secret" {
		t.Fatalf("the secret outside was touched: %v %q", err, b)
	}
	if _, err := os.Stat("/etc/kling-evil"); err == nil {
		t.Fatal("/etc/kling-evil exists")
	}
	// La raíz no se borra ni se renombra.
	if e := c.path(proto.OpRmdir, ""); e != proto.EBUSY {
		t.Errorf("rmdir root = %d", e)
	}
	// Los enlaces duros no existen en el protocolo.
	if e, _ := c.call(99, nil); e != proto.ENOSYS {
		t.Errorf("unknown op = %d, want ENOSYS", e)
	}
}

func TestEscrituraYModos(t *testing.T) {
	dir, _ := arbol(t)
	c := servir(t, dir, false)

	e, h := c.create("sub/new.txt", 0o4755) // setuid pedido: se pierde
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	e, d := c.call(proto.OpWrite, func(e *proto.Enc) { e.U64(h); e.U64(0); e.Bytes([]byte("written by the guest")) })
	if e != 0 || d.U32() != 20 {
		t.Fatalf("write: %d", e)
	}
	if e, _ := c.call(proto.OpRelease, func(e *proto.Enc) { e.U64(h) }); e != 0 {
		t.Fatalf("release: %d", e)
	}
	b, err := os.ReadFile(filepath.Join(dir, "sub", "new.txt"))
	if err != nil || string(b) != "written by the guest" {
		t.Fatalf("host sees %q, %v", b, err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "sub", "new.txt"))
	if fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("mode on the host is %v", fi.Mode())
	}
	// El handle ya no existe: ESTALE, que es lo que hace reabrir al agente.
	if e, _ := c.call(proto.OpRead, func(e *proto.Enc) { e.U64(h); e.U64(0); e.U32(1) }); e != proto.ESTALE {
		t.Errorf("read on a released handle = %d, want ESTALE", e)
	}
	if e, _ := c.call(proto.OpRead, func(e *proto.Enc) { e.U64(12345); e.U64(0); e.U32(1) }); e != proto.ESTALE {
		t.Errorf("read on an invented handle = %d, want ESTALE", e)
	}

	if e := c.mkdir("d1"); e != 0 {
		t.Fatalf("mkdir: %d", e)
	}
	if e := c.rename("sub/new.txt", "d1/moved.txt"); e != 0 {
		t.Fatalf("rename: %d", e)
	}
	if e := c.path(proto.OpRmdir, "d1"); e != proto.ENOTEMPTY {
		t.Errorf("rmdir non-empty = %d, want ENOTEMPTY", e)
	}
	if e := c.path(proto.OpUnlink, "d1"); e != proto.EISDIR {
		t.Errorf("unlink dir = %d, want EISDIR", e)
	}
	if e := c.path(proto.OpUnlink, "d1/moved.txt"); e != 0 {
		t.Fatalf("unlink: %d", e)
	}
	if e := c.path(proto.OpRmdir, "d1"); e != 0 {
		t.Fatalf("rmdir: %d", e)
	}
	// NOREPLACE.
	if e, _ := c.call(proto.OpRename, func(e *proto.Enc) { e.Str("inside"); e.Str("hello.txt"); e.U32(proto.RenameNoReplace) }); e != proto.EEXIST {
		t.Errorf("rename noreplace over an existing file = %d", e)
	}
	// Tamaños fuera del protocolo.
	e, h = c.open("hello.txt", proto.FlagRead|proto.FlagWrite)
	if e != 0 {
		t.Fatal(e)
	}
	if e, _ := c.call(proto.OpRead, func(e *proto.Enc) { e.U64(h); e.U64(0); e.U32(proto.MaxIO + 1) }); e != proto.EINVAL {
		t.Errorf("oversized read = %d", e)
	}
	if e, _ := c.call(proto.OpRead, func(e *proto.Enc) { e.U64(h); e.U64(1 << 63); e.U32(10) }); e != proto.EINVAL {
		t.Errorf("negative offset = %d", e)
	}
	// setattr: truncar y cambiar el modo.
	e, d = c.call(proto.OpSetattr, func(e *proto.Enc) {
		e.Str("hello.txt")
		e.U64(0)
		e.U32(proto.SetSize | proto.SetMode)
		e.U32(0o6600)
		e.U64(5)
		e.I64(0)
		e.I64(0)
	})
	if e != 0 {
		t.Fatalf("setattr: %d", e)
	}
	if a := proto.GetAttr(d); a.Size != 5 || a.Mode&0o7777 != 0o600 {
		t.Errorf("setattr result %+v", a)
	}
}

func TestSoloLectura(t *testing.T) {
	dir, _ := arbol(t)
	c := servir(t, dir, true)
	if e, _ := c.create("x", 0o644); e != proto.EROFS {
		t.Errorf("create = %d", e)
	}
	if e, _ := c.open("hello.txt", proto.FlagWrite); e != proto.EROFS {
		t.Errorf("open for write = %d", e)
	}
	if e, _ := c.open("hello.txt", proto.FlagRead|proto.FlagTrunc); e != proto.EROFS {
		t.Errorf("open with trunc = %d", e)
	}
	if e := c.mkdir("newdir"); e != proto.EROFS {
		t.Errorf("mkdir = %d", e)
	}
	for _, op := range []byte{proto.OpUnlink, proto.OpRmdir} {
		if e := c.path(op, "sub"); e != proto.EROFS {
			t.Errorf("op %d = %d", op, e)
		}
	}
	if e := c.rename("hello.txt", "x"); e != proto.EROFS {
		t.Errorf("rename = %d", e)
	}
	e, h := c.open("hello.txt", proto.FlagRead)
	if e != 0 {
		t.Fatal(e)
	}
	// Aunque un agente hostil pida escribir por un handle de lectura.
	if e, _ := c.call(proto.OpWrite, func(e *proto.Enc) { e.U64(h); e.U64(0); e.Bytes([]byte("x")) }); e != proto.EROFS {
		t.Errorf("write = %d", e)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if string(b) != "hello from the host\n" {
		t.Fatalf("the file changed: %q", b)
	}
}

func TestTopeDeHandles(t *testing.T) {
	dir, _ := arbol(t)
	c := servir(t, dir, true)
	for i := 0; i < maxHandles; i++ {
		if e, _ := c.open("hello.txt", proto.FlagRead); e != 0 {
			t.Fatalf("open %d: %d", i, e)
		}
	}
	if e, _ := c.open("hello.txt", proto.FlagRead); e != proto.EMFILE {
		t.Fatalf("open past the cap = %d, want EMFILE", e)
	}
}

// TestMensajesMalFormados: el daemon no se cae con basura, y una trama sin
// cabecera corta la sesión.
func TestMensajesMalFormados(t *testing.T) {
	dir, _ := arbol(t)
	c := servir(t, dir, false)
	// Argumentos de menos o de más.
	if e, _ := c.call(proto.OpStat, nil); e != proto.EINVAL {
		t.Errorf("stat without path = %d", e)
	}
	if e, _ := c.call(proto.OpStat, func(e *proto.Enc) { e.Str("hello.txt"); e.U8(1) }); e != proto.EINVAL {
		t.Errorf("stat with trailing bytes = %d", e)
	}
	if e, _ := c.call(proto.OpWrite, func(e *proto.Enc) { e.U64(1); e.U64(0); e.U32(proto.MaxIO + 1) }); e != proto.EINVAL {
		t.Errorf("write with a lying length = %d", e)
	}
	// Trama de más del tope: se corta.
	var big [4]byte
	big[0] = 0xff
	if _, err := c.conn.Write(big[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := proto.ReadFrame(c.conn); err == nil {
		t.Fatal("the session survived an oversized frame")
	}
}

func TestErrno(t *testing.T) {
	cases := map[error]uint32{
		syscall.ENOENT:    proto.ENOENT,
		syscall.ENOTEMPTY: proto.ENOTEMPTY,
		syscall.EXDEV:     proto.EXDEV,
		syscall.EDQUOT:    proto.ENOSPC,
		os.ErrNotExist:    proto.ENOENT,
		&os.PathError{Op: "openat", Path: "x", Err: syscall.ELOOP}: proto.ELOOP,
	}
	for err, want := range cases {
		if got := Errno(err); got != want {
			t.Errorf("Errno(%v) = %d, want %d", err, got, want)
		}
	}
}
