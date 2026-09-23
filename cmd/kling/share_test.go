package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostshare "github.com/juan52878911/kindling/internal/share"
	"github.com/juan52878911/kindling/pkg/api"
)

func TestParseShare(t *testing.T) {
	ok := map[string]api.ShareSpec{
		"./repo:/work":         {Mode: "copy", Source: "./repo", Mount: "/work"},
		"./repo:/work:copy":    {Mode: "copy", Source: "./repo", Mount: "/work"},
		"/srv/src:/src:ro":     {Mode: "ro", Source: "/srv/src", Mount: "/src"},
		"/srv/src:/src:rw":     {Mode: "rw", Source: "/srv/src", Mount: "/src"},
		"a:b/c:/mnt/x":         {Mode: "copy", Source: "a:b/c", Mount: "/mnt/x"},
		"/weird:dir:/mnt/x:rw": {Mode: "rw", Source: "/weird:dir", Mount: "/mnt/x"},
	}
	for in, want := range ok {
		got, err := api.ParseShare(in)
		if err != nil || got != want {
			t.Errorf("ParseShare(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "./repo", "./repo:", ":/work", "./repo:work", "./repo:/work:rwx:", "./repo:/", "./repo:/proc", "./repo:/a b", "./repo:/work/../etc"} {
		if s, err := api.ParseShare(in); err == nil {
			t.Errorf("ParseShare(%q) accepted: %+v", in, s)
		}
	}
	var f shareFlag
	if err := f.Set("./x:/work:ro"); err != nil || len(f) != 1 {
		t.Fatalf("flag: %v", err)
	}
	if err := f.Set("nope"); err == nil {
		t.Fatal("invalid flag accepted")
	}
}

// Lo que empaqueta el CLI lo acepta el daemon: se salta por su cuenta lo que el
// daemon rechazaría (enlaces que salen, FIFOs) en vez de hacer fallar la subida.
func TestTarDelCLIloAceptaElDaemon(t *testing.T) {
	src := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "a", "b"), 0o755))
	must(os.WriteFile(filepath.Join(src, "a", "b", "f.txt"), []byte("content"), 0o644))
	must(os.Link(filepath.Join(src, "a", "b", "f.txt"), filepath.Join(src, "hard.txt")))
	must(os.Symlink("a/b/f.txt", filepath.Join(src, "ok-link")))
	must(os.Symlink("/etc/passwd", filepath.Join(src, "abs-link")))
	must(os.Symlink("../../outside", filepath.Join(src, "a", "up-link")))
	must(os.Symlink("..", filepath.Join(src, "a", "b", "up")))
	must(os.Symlink("a/b/up/../..", filepath.Join(src, "chained")))

	pr, pw := io.Pipe()
	ch := make(chan []string, 1)
	go func() {
		sk, err := writeTar(pw, src)
		pw.CloseWithError(err)
		ch <- sk
	}()
	dst := t.TempDir()
	st, err := hostshare.Extract(pr, dst, 1<<20)
	if err != nil {
		t.Fatalf("the daemon rejected what the CLI sent: %v", err)
	}
	skipped := <-ch
	if st.Files != 2 {
		t.Errorf("files = %d, want 2 (the hard link as a copy)", st.Files)
	}
	joined := strings.Join(skipped, "\n")
	for _, want := range []string{"abs-link", "a/up-link", "chained"} {
		if !strings.Contains(joined, want) {
			t.Errorf("%s was not skipped: %v", want, skipped)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dst, "ok-link")); err != nil || string(b) != "content" {
		t.Errorf("ok-link: %q %v", b, err)
	}
}
