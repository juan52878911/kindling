package share

import (
	"os"
	"path/filepath"
	"testing"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// F2: el guardado atómico (escribir en un temporal y renombrarlo encima) no
// gasta cuota: el fichero sustituido devuelve lo suyo como si se borrara.
func TestCuotaRenombrarEncimaDevuelve(t *testing.T) {
	dir := t.TempDir()
	q := NewQuota(Limits{MaxBytes: 8 << 20, MaxFiles: 10})
	c := servirConCuota(t, dir, q)
	trozo := make([]byte, 64<<10)
	for i := range 200 {
		e, h := c.create("f.tmp", 0o644)
		if e != 0 {
			t.Fatalf("vuelta %d: create: %d", i, e)
		}
		for off := 0; off < 1<<20; off += len(trozo) {
			if e := c.write(h, uint64(off), trozo); e != 0 {
				t.Fatalf("vuelta %d: write: %d", i, e)
			}
		}
		c.release(h)
		if e := c.rename("f.tmp", "f"); e != 0 {
			t.Fatalf("vuelta %d: rename: %d", i, e)
		}
	}
	if b, f := q.Usage(); b != 1<<20 || f != 1 {
		t.Fatalf("usage = %d bytes, %d files; want %d, 1", b, f, 1<<20)
	}
	// Renombrar un nombre encima de otro del mismo fichero no libera nada.
	must(t, os.Link(filepath.Join(dir, "f"), filepath.Join(dir, "g")))
	if e := c.rename("g", "f"); e != 0 {
		t.Fatalf("rename de un enlace duro encima de sí mismo: %d", e)
	}
	if b, _ := q.Usage(); b != 1<<20 {
		t.Fatalf("usage = %d tras renombrar un enlace encima del mismo fichero", b)
	}
	must(t, os.Remove(filepath.Join(dir, "g")))
	// Un fichero sustituido que sigue abierto devuelve al último cierre.
	e, h := c.open("f", proto.FlagRead)
	if e != 0 {
		t.Fatalf("open: %d", e)
	}
	e, h2 := c.create("n", 0o644)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	c.release(h2)
	if e := c.rename("n", "f"); e != 0 {
		t.Fatalf("rename: %d", e)
	}
	if b, _ := q.Usage(); b != 1<<20 {
		t.Fatalf("usage = %d con el sustituido aún abierto, want %d", b, 1<<20)
	}
	c.release(h)
	if b, _ := q.Usage(); b != 0 {
		t.Fatalf("usage = %d tras cerrar el sustituido, want 0", b)
	}
}

// F7: OpCreate sobre un fichero que ya existe lo abre sin cambiarle el modo
// (como open con O_CREAT y sin O_EXCL en POSIX).
func TestCreateExistenteNoCambiaModo(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a")
	must(t, os.WriteFile(p, []byte("x"), 0o600))
	must(t, os.Chmod(p, 0o600))
	c := servirConCuota(t, dir, nil)
	e, h := c.createFlags("a", proto.FlagRead|proto.FlagWrite, 0o777)
	if e != 0 {
		t.Fatalf("create: %d", e)
	}
	c.release(h)
	fi, err := os.Stat(p)
	must(t, err)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("modo = %v tras OpCreate sobre un fichero existente, want 0600", fi.Mode().Perm())
	}
}
