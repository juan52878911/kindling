package share

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	proto "github.com/juan52878911/kindling/pkg/share"
)

func (c *cliente) symlink(p, t string) uint32 {
	e, _ := c.call(proto.OpSymlink, func(e *proto.Enc) { e.Str(p); e.Str(t) })
	return e
}

// dentro comprueba que todos los enlaces de dir, resueltos de verdad, se quedan
// en dir.
func dentro(t *testing.T, dir string) {
	t.Helper()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			return nil // apunta a algo que no existe: no saca a nadie
		}
		if r != real && !strings.HasPrefix(r, real+string(filepath.Separator)) {
			t.Errorf("%s resolves to %s, outside %s", p, r, real)
		}
		return nil
	})
}

// A4: dos enlaces relativos, válidos cada uno por separado, no pueden sumarse
// para salir: "x -> a/up/../.." y "a/up -> ..", en los dos órdenes.
func TestEnlacesQueSeSuman(t *testing.T) {
	for _, orden := range [][2][2]string{
		{{"x", "a/up/../.."}, {"a/up", ".."}},
		{{"a/up", ".."}, {"x", "a/up/../.."}},
	} {
		base := t.TempDir()
		dir := filepath.Join(base, "r1", "r2", "share")
		must(t, os.MkdirAll(filepath.Join(dir, "a"), 0o755))
		c := servir(t, dir, false)
		for _, l := range orden {
			_ = c.symlink(l[0], l[1])
		}
		if _, err := os.Lstat(filepath.Join(dir, "x")); err == nil {
			t.Errorf("order %v: x was created", orden)
		}
		// Un enlace creado A TRAVÉS de a/up (el kernel no manda esto; un root
		// del invitado a pelo sí) se crearía en la raíz con la profundidad de a/.
		if e := c.symlink("a/up/l", "../x"); e != proto.EPERM {
			t.Errorf("symlink through a link: errno %d, want EPERM", e)
		}
		dentro(t, dir)
	}
}

// Un rename a un sitio nombrado a través de un enlace tampoco: la profundidad
// real no es la de la ruta.
func TestRenameATravesDeEnlace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "share")
	must(t, os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755))
	must(t, os.Symlink("../../x", filepath.Join(dir, "a", "b", "l")))
	must(t, os.Symlink("..", filepath.Join(dir, "a", "up")))
	c := servir(t, dir, false)
	// Léxicamente a/up/l está a la misma profundidad que a/b/l; de verdad, en
	// la raíz, donde ../../x sale dos niveles.
	if e := c.rename("a/b/l", "a/up/l"); e != proto.EXDEV {
		t.Fatalf("rename through a link: errno %d, want EXDEV", e)
	}
	dentro(t, dir)
}

// M6: un symlink y un rename concurrentes (un root del invitado habla el
// protocolo sin FUSE y no los serializa nadie) no pueden dejar un enlace que
// salga: o el rename ve el enlace y lo rechaza, o el symlink ya no encuentra
// su directorio.
func TestSymlinkYRenameConcurrentes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "share")
	must(t, os.MkdirAll(dir, 0o755))
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	n := 300
	if testing.Short() {
		n = 50
	}
	for i := 0; i < n; i++ {
		must(t, os.RemoveAll(filepath.Join(dir, "a")))
		must(t, os.RemoveAll(filepath.Join(dir, "c")))
		must(t, os.MkdirAll(filepath.Join(dir, "a", "b", "c"), 0o755))
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = srv.symlink("a/b/c/l", "../../../x") }()
		go func() { defer wg.Done(); _ = srv.rename("a/b/c", "c", 0) }()
		wg.Wait()
		if t2, err := os.Readlink(filepath.Join(dir, "c", "l")); err == nil {
			t.Fatalf("iteration %d: c/l -> %s survived the move up", i, t2)
		}
	}
}
