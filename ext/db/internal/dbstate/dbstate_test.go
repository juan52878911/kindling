package dbstate

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPasswordIdaYVuelta(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KLING_DB_STATE", root)
	const id = "0123456789abcdef"
	if _, err := ReadPassword(id); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("before writing: %v", err)
	}
	if err := WritePassword(id, "secreto"); err != nil {
		t.Fatal(err)
	}
	pw, err := ReadPassword(id)
	if err != nil || pw != "secreto" {
		t.Fatalf("got %q %v", pw, err)
	}
	p, _ := PasswordPath(id)
	if p != filepath.Join(root, "copies", id, "password") {
		t.Fatalf("path %s", p)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	for _, d := range []string{root, filepath.Join(root, "copies"), filepath.Dir(p)} {
		if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", d, st.Mode())
		}
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPassword(id); err == nil {
		t.Fatal("a world-readable password file must be refused")
	}
	if err := Remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPassword(id); !errors.Is(err, ErrNoPassword) {
		t.Fatalf("after remove: %v", err)
	}
}

func TestEnlaceRechazado(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KLING_DB_STATE", root)
	const id = "00000000000000aa"
	d, _ := CopyDir(id)
	if err := os.MkdirAll(d, 0o700); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, "other")
	if err := os.WriteFile(other, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(d, "password")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPassword(id); err == nil {
		t.Fatal("a symlink must be refused")
	}
}

func TestIDInvalido(t *testing.T) {
	t.Setenv("KLING_DB_STATE", t.TempDir())
	for _, id := range []string{"", "../x", "ABCDEF0123456789", "mi-copia", "0123456/89abcdef"} {
		if _, err := CopyDir(id); err == nil {
			t.Errorf("%q: want error", id)
		}
	}
}
