package share

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// servirConCuota es servir con la cuota q puesta.
func servirConCuota(t *testing.T, dir string, q *Quota) *cliente {
	t.Helper()
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	srv.SetQuota(q)
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

func (c *cliente) write(h uint64, off uint64, data []byte) uint32 {
	e, _ := c.call(proto.OpWrite, func(e *proto.Enc) { e.U64(h); e.U64(off); e.Bytes(data) })
	return e
}

func (c *cliente) setSize(p string, h uint64, size uint64) uint32 {
	e, _ := c.call(proto.OpSetattr, func(e *proto.Enc) {
		e.Str(p)
		e.U64(h)
		e.U32(proto.SetSize)
		e.U32(0)
		e.U64(size)
		e.I64(0)
		e.I64(0)
	})
	return e
}

func (c *cliente) release(h uint64) {
	c.t.Helper()
	if e, _ := c.call(proto.OpRelease, func(e *proto.Enc) { e.U64(h) }); e != 0 {
		c.t.Fatalf("release: %d", e)
	}
}

func tamaño(t *testing.T, p string) int64 {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// Pasarse del presupuesto de bytes da ENOSPC y el fichero no crece; encoger y
// borrar devuelven presupuesto.
func TestCuotaBytes(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MaxBytes: 1000})
	c := servirConCuota(t, dir, q)

	e, h := c.create("a", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	if e := c.write(h, 0, make([]byte, 600)); e != 0 {
		t.Fatalf("write 600: %d", e)
	}
	// Sobrescribir lo que ya hay no alarga: no cuenta.
	if e := c.write(h, 0, make([]byte, 600)); e != 0 {
		t.Fatalf("rewrite 600: %d", e)
	}
	if e := c.write(h, 600, make([]byte, 500)); e != proto.ENOSPC {
		t.Fatalf("write past the budget = %d, want ENOSPC", e)
	}
	if n := tamaño(t, filepath.Join(dir, "a")); n != 600 {
		t.Fatalf("the file grew to %d after ENOSPC", n)
	}
	if b, _ := q.Usage(); b != 600 {
		t.Fatalf("usage %d, want 600", b)
	}
	// Una escritura lejos del final cuenta el hueco entero (tamaño aparente).
	if e := c.write(h, 5000, []byte("x")); e != proto.ENOSPC {
		t.Fatalf("sparse write past the budget = %d, want ENOSPC", e)
	}
	// Encoger devuelve; lo que vuelve se puede escribir.
	if e := c.setSize("a", h, 100); e != 0 {
		t.Fatalf("shrink: %d", e)
	}
	if b, _ := q.Usage(); b != 100 {
		t.Fatalf("usage after shrink %d, want 100", b)
	}
	if e := c.write(h, 100, make([]byte, 900)); e != 0 {
		t.Fatalf("write into the returned budget: %d", e)
	}
	c.release(h)
	// Borrar (cerrado, último nombre) devuelve todo.
	if e := c.path(proto.OpUnlink, "a"); e != 0 {
		t.Fatalf("unlink: %d", e)
	}
	if b, f := q.Usage(); b != 0 || f != 0 {
		t.Fatalf("usage after unlink %d bytes %d files, want 0 0", b, f)
	}
}

// Un SetSize enorme da ENOSPC y no cambia el fichero.
func TestCuotaSetSizeEnorme(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MaxBytes: 1 << 20})
	c := servirConCuota(t, dir, q)
	e, h := c.create("a", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	if e := c.setSize("a", h, 1<<40); e != proto.ENOSPC {
		t.Fatalf("SetSize 1 TiB with handle = %d, want ENOSPC", e)
	}
	c.release(h)
	if e := c.setSize("a", 0, 1<<40); e != proto.ENOSPC {
		t.Fatalf("SetSize 1 TiB by path = %d, want ENOSPC", e)
	}
	if n := tamaño(t, filepath.Join(dir, "a")); n != 0 {
		t.Fatalf("the file is %d bytes after ENOSPC", n)
	}
}

// Borrar algo que sigue abierto no devuelve hasta el último cierre: el disco
// no se libera antes.
func TestCuotaBorrarAbierto(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MaxBytes: 1000})
	c := servirConCuota(t, dir, q)
	e, h := c.create("a", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	if e := c.write(h, 0, make([]byte, 800)); e != 0 {
		t.Fatalf("write: %d", e)
	}
	if e := c.path(proto.OpUnlink, "a"); e != 0 {
		t.Fatalf("unlink: %d", e)
	}
	if b, _ := q.Usage(); b != 800 {
		t.Fatalf("usage after unlinking an open file %d, want 800", b)
	}
	e, h2 := c.create("b", 0o644)
	if e != 0 {
		t.Fatalf("create b: %d", e)
	}
	if e := c.write(h2, 0, make([]byte, 800)); e != proto.ENOSPC {
		t.Fatalf("write while the unlinked file is open = %d, want ENOSPC", e)
	}
	c.release(h)
	if b, _ := q.Usage(); b != 0 {
		t.Fatalf("usage after the last close %d, want 0", b)
	}
	if e := c.write(h2, 0, make([]byte, 800)); e != 0 {
		t.Fatalf("write after the last close: %d", e)
	}
}

// Borrar lo que ya estaba en la carpeta no da presupuesto.
func TestCuotaBorrarPrevioNoDa(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "viejo"), make([]byte, 5000), 0o644); err != nil {
		t.Fatal(err)
	}
	q := NewQuota(Limits{MaxBytes: 1000})
	c := servirConCuota(t, dir, q)
	if e := c.path(proto.OpUnlink, "viejo"); e != 0 {
		t.Fatalf("unlink: %d", e)
	}
	e, h := c.create("a", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	if e := c.write(h, 0, make([]byte, 2000)); e != proto.ENOSPC {
		t.Fatalf("write after deleting a pre-existing file = %d, want ENOSPC", e)
	}
}

// El tope de entradas: ficheros, directorios y enlaces nuevos.
func TestCuotaFicheros(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MaxFiles: 3})
	c := servirConCuota(t, dir, q)
	e, h := c.create("a", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	c.release(h)
	if e := c.mkdir("d"); e != 0 {
		t.Fatalf("mkdir: %d", e)
	}
	if e, _ := c.call(proto.OpSymlink, func(e *proto.Enc) { e.Str("l"); e.Str("a") }); e != 0 {
		t.Fatalf("symlink: %d", e)
	}
	if e, _ := c.create("b", 0o644); e != proto.ENOSPC {
		t.Fatalf("create past the file budget = %d, want ENOSPC", e)
	}
	if _, err := os.Lstat(filepath.Join(dir, "b")); !os.IsNotExist(err) {
		t.Fatalf("b exists on the host after ENOSPC: %v", err)
	}
	if e := c.mkdir("d2"); e != proto.ENOSPC {
		t.Fatalf("mkdir past the file budget = %d, want ENOSPC", e)
	}
	// Abrir algo que ya existe no es una entrada nueva.
	if e, h := c.createFlags("a", proto.FlagRead|proto.FlagWrite, 0o644); e != 0 {
		t.Fatalf("create over an existing file = %d", e)
	} else {
		c.release(h)
	}
	if e := c.path(proto.OpRmdir, "d"); e != 0 {
		t.Fatalf("rmdir: %d", e)
	}
	if e, h := c.create("b", 0o644); e != 0 {
		t.Fatalf("create after rmdir: %d", e)
	} else {
		c.release(h)
	}
}

// Por debajo del mínimo libre del host, ENOSPC aunque quede presupuesto.
func TestCuotaMinimoLibre(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MinFree: 1 << 62}) // más de lo que tiene cualquier disco
	c := servirConCuota(t, dir, q)
	if e, _ := c.create("a", 0o644); e != proto.ENOSPC {
		t.Fatalf("create under the host's free minimum = %d, want ENOSPC", e)
	}
	if err := os.WriteFile(filepath.Join(dir, "b"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	e, h := c.open("b", proto.FlagWrite)
	if e != 0 {
		t.Fatalf("open: %d", e)
	}
	if e := c.write(h, 0, []byte("x")); e != proto.ENOSPC {
		t.Fatalf("write under the host's free minimum = %d, want ENOSPC", e)
	}
	if n := tamaño(t, filepath.Join(dir, "b")); n != 0 {
		t.Fatalf("b grew to %d", n)
	}
}

// Por debajo del mínimo libre, reescribir en su sitio un fichero sin huecos
// no gasta disco y no da ENOSPC (una base de datos que reescribe sus páginas
// con el disco casi lleno); rellenar un hueco sí lo gasta y sigue parándose.
func TestCuotaMinimoLibreReescribirEnSitio(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MinFree: 1 << 62}) // más de lo que tiene cualquier disco
	c := servirConCuota(t, dir, q)
	denso := filepath.Join(dir, "denso")
	if err := os.WriteFile(denso, make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	e, h := c.open("denso", proto.FlagWrite)
	if e != 0 {
		t.Fatalf("open: %d", e)
	}
	if e := c.write(h, 4096, []byte("página nueva")); e != 0 {
		t.Fatalf("rewriting a file with no holes in place = %d, want 0", e)
	}
	if e := c.write(h, 64<<10-1, []byte("xx")); e != proto.ENOSPC {
		t.Fatalf("growing it = %d, want ENOSPC", e)
	}
	c.release(h)

	disperso := filepath.Join(dir, "disperso")
	f, err := os.Create(disperso)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()
	e, h = c.open("disperso", proto.FlagWrite)
	if e != 0 {
		t.Fatalf("open: %d", e)
	}
	if e := c.write(h, 4096, []byte("x")); e != proto.ENOSPC {
		t.Fatalf("filling a hole under the host's free minimum = %d, want ENOSPC", e)
	}
}

// O_TRUNC devuelve lo que medía el fichero.
func TestCuotaOTrunc(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MaxBytes: 1000})
	c := servirConCuota(t, dir, q)
	e, h := c.create("a", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	if e := c.write(h, 0, make([]byte, 900)); e != 0 {
		t.Fatalf("write: %d", e)
	}
	c.release(h)
	e, h = c.open("a", proto.FlagWrite|proto.FlagTrunc)
	if e != 0 {
		t.Fatalf("open O_TRUNC: %d", e)
	}
	if b, _ := q.Usage(); b != 0 {
		t.Fatalf("usage after O_TRUNC %d, want 0", b)
	}
	if e := c.write(h, 0, make([]byte, 900)); e != 0 {
		t.Fatalf("write after O_TRUNC: %d", e)
	}
}

// La cuota es de la carpeta: una sesión nueva (reconexión) sigue la cuenta.
func TestCuotaSobreviveReconexion(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MaxBytes: 1000})
	c := servirConCuota(t, dir, q)
	e, h := c.create("a", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	if e := c.write(h, 0, make([]byte, 900)); e != 0 {
		t.Fatalf("write: %d", e)
	}
	c.release(h)
	c2 := servirConCuota(t, dir, q)
	e, h = c2.create("b", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	if e := c2.write(h, 0, make([]byte, 900)); e != proto.ENOSPC {
		t.Fatalf("write on a new session = %d, want ENOSPC", e)
	}
}

// Escrituras que alargan el mismo fichero a la vez, con SetSize por medio,
// cuentan exactamente lo que crece: ni de más (se perdería para siempre) ni de
// menos.
func TestCuotaEscriturasALaVez(t *testing.T) {
	dir := t.TempDir()
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	q := NewQuota(Limits{MaxBytes: 1 << 30})
	srv.SetQuota(q)
	f, err := os.Create(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, e := srv.writeAt(f, make([]byte, 4096), int64(i)*4096); e != 0 {
				t.Errorf("write %d: %d", i, e)
			}
			if i%8 == 0 {
				_ = srv.truncate(f, int64(i)*1024)
			}
		}()
	}
	wg.Wait()
	if b, n := q.Usage(); b != tamaño(t, filepath.Join(dir, "a")) {
		t.Fatalf("usage %d (%d files), file %d bytes", b, n, tamaño(t, filepath.Join(dir, "a")))
	}
}

// Una carpeta de solo lectura ignora la cuota.
func TestCuotaSoloLecturaIgnorada(t *testing.T) {
	srv, err := Open(t.TempDir(), true, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	srv.SetQuota(NewQuota(Limits{MaxBytes: 1}))
	if srv.quota != nil {
		t.Fatal("a read-only share kept a quota")
	}
}
