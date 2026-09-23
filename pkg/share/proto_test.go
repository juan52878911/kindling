package share

import (
	"bytes"
	"strings"
	"testing"
)

func TestCodecIdaYVuelta(t *testing.T) {
	e := Request(7, OpWrite)
	e.U64(1 << 40)
	e.I64(-5)
	e.Str("a/b")
	e.Bytes([]byte("data"))
	Attr{Mode: SIFREG | 0o644, Size: 9, Nlink: 1, Mtime: 3, Mtimen: 4}.Put(e)

	d := NewDec(e.B)
	if d.U64() != 7 || d.U8() != OpWrite || d.U64() != 1<<40 || d.I64() != -5 || d.Str() != "a/b" {
		t.Fatal("header or fields")
	}
	if string(d.Bytes(10)) != "data" {
		t.Fatal("bytes")
	}
	if a := GetAttr(d); a.Size != 9 || a.Mode != SIFREG|0o644 || a.Mtimen != 4 {
		t.Fatalf("attr %+v", a)
	}
	if err := d.Done(); err != nil {
		t.Fatal(err)
	}
}

func TestDecNoSeFiaDeLasLongitudes(t *testing.T) {
	// Un bloque que dice medir más de lo que hay, o más del tope.
	e := &Enc{}
	e.U32(1000)
	e.B = append(e.B, "abc"...)
	d := NewDec(e.B)
	if d.Bytes(4096) != nil || d.Err() == nil {
		t.Fatal("a truncated block was accepted")
	}
	e = &Enc{}
	e.Bytes(make([]byte, 100))
	if d := NewDec(e.B); d.Bytes(10) != nil || d.Err() == nil {
		t.Fatal("a block over the limit was accepted")
	}
	// Tras un error, todo vale cero y el error se queda.
	d = NewDec([]byte{1, 2})
	if d.U64() != 0 || d.U32() != 0 || d.Str() != "" || d.Done() == nil {
		t.Fatal("sticky error")
	}
	// Bytes de más: Done lo dice.
	if err := NewDec([]byte{1}).Done(); err == nil {
		t.Fatal("trailing bytes accepted")
	}
}

func TestTramas(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, []byte("hola")); err != nil {
		t.Fatal(err)
	}
	if b, err := ReadFrame(&buf); err != nil || string(b) != "hola" {
		t.Fatalf("%q %v", b, err)
	}
	if err := WriteFrame(&buf, make([]byte, MaxFrame+1)); err == nil {
		t.Fatal("oversized frame written")
	}
	if _, err := ReadFrame(bytes.NewReader([]byte{0xff, 0xff, 0xff, 0xff})); err == nil {
		t.Fatal("oversized frame read")
	}
}

func TestRutasYMontajes(t *testing.T) {
	for _, p := range []string{"", "a", "a/b", "a b/c", strings.Repeat("x", 255)} {
		if err := ValidPath(p); err != nil {
			t.Errorf("ValidPath(%q): %v", p, err)
		}
	}
	for _, p := range []string{"/a", "a/", "a//b", ".", "..", "a/../b", "./a", "a/./b", "a\x00b", strings.Repeat("x", 256), strings.Repeat("a/", 2049) + "a"} {
		if ValidPath(p) == nil {
			t.Errorf("ValidPath(%q) accepted", p)
		}
	}
	for _, m := range []string{"/work", "/home/u/src", "/data2"} {
		if err := ValidMount(m); err != nil {
			t.Errorf("ValidMount(%q): %v", m, err)
		}
	}
	for _, m := range []string{"", "work", "/", "/etc", "/usr/local/x", "/proc/1", "/a b", "/a,b", "/a:b", "/w/../etc", "/w/", "//w"} {
		if ValidMount(m) == nil {
			t.Errorf("ValidMount(%q) accepted", m)
		}
	}
}

func TestEnlaces(t *testing.T) {
	for _, c := range [][2]string{{"l", "a"}, {"d/l", "../a"}, {"d/e/l", "../../x/y"}, {"l", "./a/../b"}} {
		if err := CheckLink(c[0], c[1]); err != nil {
			t.Errorf("CheckLink(%q, %q): %v", c[0], c[1], err)
		}
	}
	for _, c := range [][2]string{{"l", "/etc"}, {"l", ".."}, {"d/l", "../../x"}, {"l", ""}, {"l", "a/../../b"}} {
		if CheckLink(c[0], c[1]) == nil {
			t.Errorf("CheckLink(%q, %q) accepted", c[0], c[1])
		}
	}
	links := map[string]bool{"a/b/up": true}
	isLink := func(p string) bool { return links[p] }
	if !LinkTraverses("x", "a/b/up/../..", isLink) {
		t.Error("a target through another link was not detected")
	}
	if LinkTraverses("x", "a/b/up", isLink) {
		t.Error("a link TO a link is fine: the other one is checked on its own")
	}
}
