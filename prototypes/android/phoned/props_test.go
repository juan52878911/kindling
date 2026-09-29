package main

import (
	"encoding/binary"
	"strings"
	"testing"
	"unsafe"
)

// testArea construye un área de propiedades como la de bionic
// (prop_area::find_property con alloc_if_needed), para probar la lectura y la
// reescritura sin Android.
type testArea struct {
	propArea
}

func newTestArea(t *testing.T, size int) *testArea {
	t.Helper()
	words := make([]uint32, size/4) // alineado a 4
	b := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), size)
	le := binary.LittleEndian
	le.PutUint32(b[8:], propAreaMagic)
	le.PutUint32(b[12:], propAreaVersion)
	// bytes_used: la raíz (prop_bt vacío) y el área de respaldo.
	le.PutUint32(b[0:], propBtSize+propValueMax)
	return &testArea{propArea{b: b}}
}

func (a *testArea) alloc(n int) uint32 {
	le := binary.LittleEndian
	used := le.Uint32(a.b[0:])
	n = (n + 3) &^ 3
	le.PutUint32(a.b[0:], used+uint32(n))
	return used
}

func (a *testArea) newBt(name string) uint32 {
	off := a.alloc(propBtSize + len(name) + 1)
	le := binary.LittleEndian
	le.PutUint32(a.b[data(off):], uint32(len(name)))
	copy(a.b[data(off)+propBtSize:], name)
	return off
}

func (a *testArea) add(name, value string) {
	le := binary.LittleEndian
	cur := uint32(0)
	for _, seg := range strings.Split(name, ".") {
		ch := le.Uint32(a.b[data(cur)+16:])
		if ch == 0 {
			n := a.newBt(seg)
			le.PutUint32(a.b[data(cur)+16:], n)
			cur = n
			continue
		}
		off := ch
		for {
			c := cmpPropName([]byte(seg), a.btName(off))
			if c == 0 {
				break
			}
			slot := data(off) + 12
			if c < 0 {
				slot = data(off) + 8
			}
			next := le.Uint32(a.b[slot:])
			if next == 0 {
				next = a.newBt(seg)
				le.PutUint32(a.b[slot:], next)
				off = next
				break
			}
			off = next
		}
		cur = off
	}
	pi := a.alloc(4 + propValueMax + len(name) + 1)
	le.PutUint32(a.b[data(pi):], uint32(len(value))<<24)
	copy(a.b[data(pi)+4:], value)
	copy(a.b[data(pi)+4+propValueMax:], name)
	le.PutUint32(a.b[data(cur)+4:], pi)
}

func TestPropAreaGetSet(t *testing.T) {
	a := newTestArea(t, 32<<10)
	props := map[string]string{
		"ro.serialno":              "KINDLINGGOLDEN",
		"ro.boot.serialno":         "KINDLINGGOLDEN",
		"sys.boot_completed":       "1",
		"ro.adb.secure":            "1",
		"ro.build.version.release": "13",
		"a":                        "x",
		"ro.b":                     "",
		"ro.zzz.long.name.here":    "v",
	}
	for k, v := range props {
		a.add(k, v)
	}
	for k, v := range props {
		got, err := a.get(k)
		if err != nil || got != v {
			t.Fatalf("get(%s) = %q, %v; want %q", k, got, err, v)
		}
	}
	for _, missing := range []string{"ro.serial", "ro.serialno.x", "sys", "nope.nope"} {
		if _, err := a.get(missing); err == nil {
			t.Fatalf("get(%s) found something", missing)
		}
	}
	serial0 := a.u32(data(mustFind(t, a, "ro.serialno")))
	if err := a.set("ro.serialno", "ABCDEF0123456789XY"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.get("ro.serialno"); got != "ABCDEF0123456789XY" {
		t.Fatalf("after set: %q", got)
	}
	s1 := a.u32(data(mustFind(t, a, "ro.serialno")))
	if s1>>24 != 18 || s1&1 != 0 || (s1&0xffffff) == (serial0&0xffffff) {
		t.Fatalf("serial after set %#x (before %#x)", s1, serial0)
	}
	// Más corto: no quedan restos del valor anterior.
	if err := a.set("ro.serialno", "SHORT1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := a.get("ro.serialno"); got != "SHORT1" {
		t.Fatalf("after short set: %q", got)
	}
	if got, _ := a.get("ro.boot.serialno"); got != "KINDLINGGOLDEN" {
		t.Fatalf("neighbour changed: %q", got)
	}
	if err := a.set("ro.serialno", strings.Repeat("x", propValueMax)); err == nil {
		t.Fatal("a 92-byte value was accepted")
	}
	if err := a.set("ro.nope", "x"); err == nil {
		t.Fatal("set of a missing property worked")
	}
	before := a.u32(4)
	if err := a.bumpSerial(); err != nil || a.u32(4) != before+1 {
		t.Fatalf("bumpSerial: %v %d→%d", err, before, a.u32(4))
	}
}

func mustFind(t *testing.T, a *testArea, name string) uint32 {
	t.Helper()
	p, err := a.find(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPropAreaRejectsGarbage(t *testing.T) {
	a := newTestArea(t, 4096)
	a.b[8] = 0 // magia rota
	if _, err := a.get("ro.serialno"); err == nil {
		t.Fatal("bad magic accepted")
	}
	// Desplazamientos fuera del área: no puede entrar en pánico.
	b := newTestArea(t, 4096)
	binary.LittleEndian.PutUint32(b.b[data(0)+16:], 1<<30)
	if _, err := b.get("ro.x"); err == nil {
		t.Fatal("out-of-range child accepted")
	}
	c := newTestArea(t, 4096)
	c.add("ro.x", "1")
	// Un bucle en el árbol (izquierda apuntándose a sí mismo) termina.
	ch := binary.LittleEndian.Uint32(c.b[data(0)+16:])
	binary.LittleEndian.PutUint32(c.b[data(ch)+8:], ch)
	if _, err := c.get("aa.x"); err == nil {
		t.Fatal("found in a looped tree")
	}
}
