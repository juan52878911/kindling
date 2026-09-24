package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// El token se genera 0600, se relee igual y un fichero legible por otros se
// rechaza en vez de usarse.
func TestAITokenFichero(t *testing.T) {
	t.Setenv("KLING_AI_TOKEN", "")
	p := filepath.Join(t.TempDir(), "sub", "ai.token")
	if _, err := aiToken(p, false); err == nil {
		t.Fatal("missing token file without create should fail")
	}
	t1, err := aiToken(p, true)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, %v", st.Mode().Perm(), err)
	}
	t2, err := aiToken(p, true)
	if err != nil || t2 != t1 {
		t.Fatalf("reread token %q != %q (%v)", t2, t1, err)
	}
	_ = os.Chmod(p, 0o644)
	if _, err := aiToken(p, true); err == nil {
		t.Fatal("world-readable token accepted")
	}
	t.Setenv("KLING_AI_TOKEN", "from-env-0123456789")
	if got, _ := aiToken(p, true); got != "from-env-0123456789" {
		t.Fatalf("env token ignored: %q", got)
	}
}

// El socket nace 0600, no pisa a un gateway vivo y sí retira uno muerto.
func TestAISocket0600(t *testing.T) {
	dir, err := os.MkdirTemp("", "ai")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "ai.sock")
	ln, err := listenUnix0600(p)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode = %v", st.Mode().Perm())
	}
	if _, err := listenUnix0600(p); err == nil {
		t.Fatal("a second gateway took a live socket")
	}
	ln.(interface{ SetUnlinkOnClose(bool) }).SetUnlinkOnClose(false)
	ln.Close()
	ln2, err := listenUnix0600(p)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ln2.Close()
	if _, err := listenUnix0600(filepath.Join(dir, strings.Repeat("a", 120))); err == nil {
		t.Fatal("overlong socket path accepted")
	}
}
