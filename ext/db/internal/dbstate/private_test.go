package dbstate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivadoIdaYVuelta(t *testing.T) {
	root := t.TempDir()
	t.Setenv("KLING_DB_STATE", filepath.Join(root, "st"))
	dir, err := EnsureDir("reports")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{filepath.Join(root, "st"), dir} {
		if st, _ := os.Stat(d); st.Mode().Perm() != 0o700 {
			t.Fatalf("%s mode %v", d, st.Mode().Perm())
		}
	}
	if _, err := EnsureDir("../x"); err == nil {
		t.Fatal("bad subdirectory accepted")
	}
	p := filepath.Join(dir, "r.json")
	if _, err := ReadPrivate(p, 10); !IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
	if err := WritePrivate(p, []byte("hola")); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	if b, err := ReadPrivate(p, 10); err != nil || string(b) != "hola" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := ReadPrivate(p, 3); err == nil {
		t.Error("the size limit was ignored")
	}
	_ = os.Chmod(p, 0o644)
	if _, err := ReadPrivate(p, 10); err == nil {
		t.Error("a world-readable file was accepted")
	}
	// Sobre un enlace: se sustituye el enlace, el destino no cambia.
	target := filepath.Join(root, "target")
	_ = os.WriteFile(target, []byte("x"), 0o644)
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if _, err := ReadPrivate(link, 10); err == nil {
		t.Error("a symlink was read")
	}
	if err := WritePrivate(link, []byte("secreto")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(target); string(b) != "x" {
		t.Errorf("the link target was written: %q", b)
	}
	if st, _ := os.Lstat(link); !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Errorf("link not replaced by a 0600 file: %v", st.Mode())
	}
}
