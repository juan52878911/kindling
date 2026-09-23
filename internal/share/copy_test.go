package share

import (
	"archive/tar"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entrada struct {
	name, link string
	typ        byte
	body       string
	mode       int64
}

func tarDe(t *testing.T, es ...entrada) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Mode: e.mode, Size: int64(len(e.body))}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func TestExtraeUnArbolNormal(t *testing.T) {
	dst := t.TempDir()
	st, err := Extract(tarDe(t,
		entrada{name: "./", typ: tar.TypeDir},
		entrada{name: "src/main.go", typ: tar.TypeReg, body: "package main\n"},
		entrada{name: "src/", typ: tar.TypeDir, mode: 0o755},
		entrada{name: "bin/run", typ: tar.TypeReg, body: "#!/bin/sh\n", mode: 0o4755},
		entrada{name: "link", typ: tar.TypeSymlink, link: "src/main.go"},
		entrada{name: "src/up", typ: tar.TypeSymlink, link: "../bin/run"},
	), dst, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2 || st.Symlinks != 2 || st.Bytes != int64(len("package main\n#!/bin/sh\n")) {
		t.Errorf("stats %+v", st)
	}
	b, err := os.ReadFile(filepath.Join(dst, "link"))
	if err != nil || string(b) != "package main\n" {
		t.Errorf("link reads %q, %v", b, err)
	}
	fi, _ := os.Stat(filepath.Join(dst, "bin/run"))
	if fi.Mode()&os.ModeSetuid != 0 || fi.Mode().Perm() != 0o755 {
		t.Errorf("setuid survived: %v", fi.Mode())
	}
}

func TestRechazaTrampas(t *testing.T) {
	cases := map[string][]entrada{
		"absolute":        {{name: "/etc/passwd", typ: tar.TypeReg, body: "x"}},
		"dotdot":          {{name: "../escape", typ: tar.TypeReg, body: "x"}},
		"inner dotdot":    {{name: "a/../../escape", typ: tar.TypeReg, body: "x"}},
		"abs symlink":     {{name: "l", typ: tar.TypeSymlink, link: "/etc/passwd"}},
		"escaping link":   {{name: "a/l", typ: tar.TypeSymlink, link: "../../x"}},
		"through symlink": {{name: "l", typ: tar.TypeSymlink, link: "sub"}, {name: "l/file", typ: tar.TypeReg, body: "x"}},
		"hard link":       {{name: "a", typ: tar.TypeReg, body: "x"}, {name: "b", typ: tar.TypeLink, link: "a"}},
		"char device":     {{name: "tty", typ: tar.TypeChar}},
		"block device":    {{name: "sda", typ: tar.TypeBlock}},
		"fifo":            {{name: "p", typ: tar.TypeFifo}},
		"duplicate":       {{name: "a", typ: tar.TypeReg, body: "x"}, {name: "a", typ: tar.TypeReg, body: "y"}},
		"long component":  {{name: strings.Repeat("n", 256), typ: tar.TypeReg, body: "x"}},
		// Un enlace que llega fuera atravesando otro enlace del mismo árbol:
		// léxicamente "a/up/../.." es la raíz, de verdad es su padre.
		"chained escape": {
			{name: "a/b/up", typ: tar.TypeSymlink, link: ".."},
			{name: "x", typ: tar.TypeSymlink, link: "a/b/up/../../.."},
		},
	}
	for name, es := range cases {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			dst := filepath.Join(base, "tree")
			if err := os.Mkdir(dst, 0o700); err != nil {
				t.Fatal(err)
			}
			if _, err := Extract(tarDe(t, es...), dst, 1<<20); err == nil {
				t.Fatal("accepted")
			}
			if _, err := os.Stat(filepath.Join(base, "escape")); err == nil {
				t.Fatal("something was written outside")
			}
		})
	}
}

func TestTopeDeTamano(t *testing.T) {
	_, err := Extract(tarDe(t, entrada{name: "big", typ: tar.TypeReg, body: strings.Repeat("x", 2048)}), t.TempDir(), 1024)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}
