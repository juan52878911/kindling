package klingc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
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

// Command lleva el mismo binario y el mismo daemon que Run (kling shell para
// el sqlite3 de una copia SQLite). Sin stdin: cmd.Stdin nil es /dev/null.
func TestCommandPasaHost(t *testing.T) {
	t.Setenv("KLING_HOST", "ssh://otro")
	c := &CLI{Bin: fakeBin(t), Host: "ssh://lab"}
	out, err := c.Command(context.Background(), "shell", "m", "--", "sqlite3", "/x").Output()
	if err != nil {
		t.Fatal(err)
	}
	if want := "args=shell m -- sqlite3 /x\nhost=ssh://lab\n"; string(out) != want {
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

// List y Thaw van por el API del daemon (sin lanzar kling), al host de -H.
func TestMachinerPorElAPI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket")
	}
	dir, err := os.MkdirTemp("/tmp", "klingc")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/machines":
			json.NewEncoder(w).Encode([]*api.Machine{{ID: "abc", State: api.StateWarm}})
		case "/machines/abc/thaw":
			json.NewEncoder(w).Encode(api.Machine{ID: "abc", State: api.StateRunning, IP: "172.30.0.9"})
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()
	t.Setenv("KLING_CONFIG", filepath.Join(dir, "config.json"))
	c := &CLI{Bin: "/nonexistent/kling", Host: "unix://" + sock}
	l, err := c.List(context.Background())
	if err != nil || len(l) != 1 || l[0].ID != "abc" {
		t.Fatalf("List = %v, %v", l, err)
	}
	mc, err := c.Thaw(context.Background(), "abc")
	if err != nil || mc.State != api.StateRunning || mc.IP != "172.30.0.9" {
		t.Fatalf("Thaw = %+v, %v", mc, err)
	}
	if want := []string{"GET /machines", "POST /machines/abc/thaw"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("requests %v, want %v", got, want)
	}
}
