package deb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"1.0", "1.0", 0},
		{"1.0", "1.1", -1},
		{"1.10", "1.9", 1},
		{"1.0~rc1", "1.0", -1},
		{"1.0", "1.0+b1", -1},
		{"2:1.0", "1:9.9", 1},
		{"1.8.11-2", "1.8.11-10", -1},
		{"3.5.7-1~deb13u2", "3.5.7-1", -1},
		{"1:2.75-10+deb13u1+b3", "1:2.75-10+deb13u1", 1},
		{"6.5+20250216-2", "6.5+20250216-2", 0},
		{"1.0a", "1.0", 1},
	} {
		got := CompareVersions(c.a, c.b)
		if (got < 0) != (c.want < 0) || (got > 0) != (c.want > 0) {
			t.Errorf("CompareVersions(%q, %q) = %d, want sign %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseDepends(t *testing.T) {
	d := ParseDepends("libc6 (>= 2.38), libxtables12 (= 1.8.11-2), debconf (>= 0.5) | debconf-2.0, foo:any [amd64]")
	if len(d) != 4 || d[0][0] != (Relation{"libc6", ">=", "2.38", ""}) || len(d[2]) != 2 || d[2][1].Name != "debconf-2.0" ||
		d[3][0].Name != "foo" || d[3][0].Arch != "any" {
		t.Fatalf("%+v", d)
	}
}

func TestParseParagraphs(t *testing.T) {
	ps, err := ParseParagraphs(strings.NewReader("Package: a\nDescription: x\n more\n .\n\nPackage: b\nVersion: 1\n"))
	if err != nil || len(ps) != 2 || ps[0].Get("description") != "x\n more\n ." || ps[1].Get("Version") != "1" {
		t.Fatalf("%v %+v", err, ps)
	}
	if s := ps[0].String(); s != "Package: a\nDescription: x\n more\n .\n" {
		t.Fatalf("%q", s)
	}
}

// Build arma un .deb con control.tar.gz y data.tar.gz.
func Build(t testing.TB, dir, name string, control map[string]string, data map[string]string) string {
	tgz := func(files map[string]string) []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755})
		for n, body := range files {
			if strings.HasSuffix(n, "/") {
				tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeDir, Mode: 0o755})
				continue
			}
			if l, ok := strings.CutPrefix(body, "->"); ok {
				tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeSymlink, Linkname: l, Mode: 0o777})
				continue
			}
			tw.WriteHeader(&tar.Header{Name: n, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(body))})
			tw.Write([]byte(body))
		}
		tw.Close()
		zw.Close()
		return b.Bytes()
	}
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	member := func(n string, b []byte) {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", n, 0, 0, 0, "100644", len(b))
		out.Write(b)
		if len(b)%2 == 1 {
			out.WriteByte('\n')
		}
	}
	member("debian-binary", []byte("2.0\n"))
	member("control.tar.gz", tgz(control))
	member("data.tar.gz", tgz(data))
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, out.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpen(t *testing.T) {
	p := Build(t, t.TempDir(), "x.deb", map[string]string{"./control": "Package: x\nVersion: 1\n", "./postinst": "#!/bin/sh\n"},
		map[string]string{"./usr/": "", "./usr/bin/x": "binario"})
	d, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := d.Control()
	if err != nil || string(c["control"]) != "Package: x\nVersion: 1\n" || c["postinst"] == nil {
		t.Fatalf("%v %v", err, c)
	}
	rc, err := d.Data()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	found := false
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Name == "./usr/bin/x" {
			b, _ := io.ReadAll(tr)
			found = string(b) == "binario"
		}
	}
	if !found {
		t.Fatal("./usr/bin/x not in data.tar")
	}
	os.WriteFile(p, []byte("not a deb"), 0o644)
	if _, err := Open(p); err == nil {
		t.Fatal("garbage accepted")
	}
}
