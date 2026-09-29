package klingc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeBin escribe un "kling" de shell que imprime su argv, su KLING_HOST y su
// stdin, y falla si el primer argumento es "fail".
func fakeBin(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	p := filepath.Join(t.TempDir(), "kling")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = fail ]; then echo boom >&2; exit 3; fi\n" +
		"echo \"args=$*\"\necho \"host=$KLING_HOST\"\ncat\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunPasaHostYStdin(t *testing.T) {
	t.Setenv("KLING_HOST", "ssh://otro")
	c := &CLI{Bin: fakeBin(t), Host: "ssh://lab"}
	out, err := c.Run(context.Background(), strings.NewReader("hola\n"), "exec", "-i", "m", "--", "cat")
	if err != nil {
		t.Fatal(err)
	}
	want := "args=exec -i m -- cat\nhost=ssh://lab\nhola\n"
	if string(out) != want {
		t.Fatalf("got %q, want %q", out, want)
	}
}

func TestRunSinHostHeredaElEntorno(t *testing.T) {
	t.Setenv("KLING_HOST", "ssh://entorno")
	c := &CLI{Bin: fakeBin(t)}
	out, err := c.Run(context.Background(), nil, "ps")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "host=ssh://entorno") {
		t.Fatalf("got %q", out)
	}
}

func TestRunError(t *testing.T) {
	c := &CLI{Bin: fakeBin(t)}
	_, err := c.Run(context.Background(), nil, "fail")
	var ke *Error
	if !errors.As(err, &ke) || ke.Stderr != "boom" || ke.Args[0] != "fail" {
		t.Fatalf("got %#v", err)
	}
}

func TestResolve(t *testing.T) {
	t.Setenv("KLING", "/opt/kling")
	if p, err := Resolve(); err != nil || p != "/opt/kling" {
		t.Fatalf("got %q %v", p, err)
	}
	t.Setenv("KLING", "kling -H ssh://lab")
	if _, err := Resolve(); err == nil {
		t.Fatal("KLING with flags must be rejected")
	}
	t.Setenv("KLING", "")
	t.Setenv("KLING_BIN", "/usr/lib/kling")
	if p, err := Resolve(); err != nil || p != "/usr/lib/kling" {
		t.Fatalf("got %q %v", p, err)
	}
}
