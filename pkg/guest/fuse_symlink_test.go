package guest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/share"
)

func TestRelativeTarget(t *testing.T) {
	for _, c := range []struct{ p, target, want string }{
		{"node_modules/.bin/mime", "/app/node_modules/mime/cli.js", "../mime/cli.js"},
		{"l", "/app/a/b", "a/b"},
		{"a/b/l", "/app", "../.."},
		{"a/l", "/app/a", "."},
	} {
		got, ok := relativeTarget("/app", c.p, c.target)
		if !ok || got != c.want {
			t.Errorf("relativeTarget(%q, %q) = %q, %v; want %q", c.p, c.target, got, ok, c.want)
		}
	}
	for _, bad := range []string{"/usr/bin/python3", "/apple", "/"} {
		if _, ok := relativeTarget("/app", "l", bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func symlinkReq(name, target string) []byte { return append(cstr(name), cstr(target)...) }

// El invitado crea enlaces que se quedan dentro de la carpeta (npm y sus
// node_modules/.bin), nunca uno que saque a un proceso del host fuera de ella.
func TestSymlinkVivo(t *testing.T) {
	dir := carpeta(t)
	m := montar(t, dir, false)
	e, sub, _ := m.lookup(t, rootID, "sub")
	if e != 0 {
		t.Fatal(e)
	}
	if e, _ := m.dev.pedir(t, opSymlink, sub, symlinkReq("bin", "../hello.txt")); e != 0 {
		t.Fatalf("relative symlink inside: errno %d", e)
	}
	if got, err := os.Readlink(filepath.Join(dir, "sub", "bin")); err != nil || got != "../hello.txt" {
		t.Fatalf("on the host: %q %v", got, err)
	}
	if e, _ := m.dev.pedir(t, opSymlink, rootID, symlinkReq("abs", "/work/sub/bin")); e != 0 {
		t.Fatalf("absolute symlink inside the mount: errno %d", e)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "abs")); got != "sub/bin" {
		t.Fatalf("absolute target not made relative: %q", got)
	}
	for _, c := range [][2]string{{"fuera", "../../etc/passwd"}, {"etc2", "/etc/passwd"}, {"via", "sub/bin/../../.."}} {
		if e, _ := m.dev.pedir(t, opSymlink, rootID, symlinkReq(c[0], c[1])); e != share.EPERM {
			t.Errorf("symlink %s -> %s: errno %d, want EPERM", c[0], c[1], e)
		}
		if _, err := os.Lstat(filepath.Join(dir, c[0])); err == nil {
			t.Errorf("%s was created on the host", c[0])
		}
	}

	// Subir de nivel un directorio con un enlace que dejaría de valer: EXDEV
	// (mv copia y vuelve a crear el enlace, con su comprobación).
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../hello.txt", filepath.Join(dir, "a", "b", "up")); err != nil {
		t.Fatal(err)
	}
	e, a, _ := m.lookup(t, rootID, "a")
	if e != 0 {
		t.Fatal(e)
	}
	rn := func(oldParent uint64, oldName string, newParent uint64, newName string) uint32 {
		e, _ := m.dev.pedir(t, opRename, oldParent, u64s(newParent), cstr(oldName), cstr(newName))
		return e
	}
	if e := rn(a, "b", rootID, "b"); e != share.EXDEV {
		t.Fatalf("moving a/b up: errno %d, want EXDEV", e)
	}
	if e := rn(a, "b", a, "c"); e != 0 {
		t.Fatalf("renaming a/b to a/c (same depth): errno %d", e)
	}
	if e := rn(sub, "bin", rootID, "bin"); e != share.EXDEV {
		t.Fatalf("moving the symlink sub/bin up: errno %d, want EXDEV", e)
	}
}
