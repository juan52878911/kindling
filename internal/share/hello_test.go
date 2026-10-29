package share

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	proto "github.com/juan52878911/kindling/pkg/share"
)

// Sin OpHello (un agente anterior) la sesión habla BaseIO: una lectura mayor
// es EINVAL, como siempre.
func TestSinHelloSeQuedaEnBaseIO(t *testing.T) {
	dir, _ := arbol(t)
	c := servir(t, dir, false)
	e, h := c.open("hello.txt", proto.FlagRead)
	if e != 0 {
		t.Fatal(e)
	}
	if e, _ := c.call(proto.OpRead, func(e *proto.Enc) { e.U64(h); e.U64(0); e.U32(proto.BaseIO + 1) }); e != proto.EINVAL {
		t.Errorf("read of BaseIO+1 without hello = %d, want EINVAL", e)
	}
}

// OpHello contesta el menor de los dos, sin bajar de BaseIO ni pasar de MaxIO,
// y a partir de ahí se aceptan lecturas y escrituras de ese tamaño.
func TestHelloNegociaElMenor(t *testing.T) {
	dir, _ := arbol(t)
	c := servir(t, dir, false)
	hello := func(want uint32) uint32 {
		t.Helper()
		e, d := c.call(proto.OpHello, func(e *proto.Enc) { e.U32(want) })
		if e != 0 {
			t.Fatalf("hello(%d): %d", want, e)
		}
		n, caps := d.U32(), d.U32()
		if d.Done() != nil || caps&proto.CapWriteBehind == 0 {
			t.Fatalf("hello(%d): malformed reply", want)
		}
		return n
	}
	if n := hello(4096); n != proto.BaseIO {
		t.Errorf("hello(4096) = %d, want BaseIO", n)
	}
	if n := hello(256 << 10); n != 256<<10 {
		t.Errorf("hello(256K) = %d", n)
	}
	if n := hello(1 << 30); n != proto.MaxIO {
		t.Errorf("hello(1G) = %d, want MaxIO", n)
	}

	datos := bytes.Repeat([]byte("kindling"), proto.MaxIO/8)
	e, h := c.create("big.bin", 0o644)
	if e != 0 {
		t.Fatal(e)
	}
	e, d := c.call(proto.OpWrite, func(e *proto.Enc) { e.U64(h); e.U64(0); e.Bytes(datos) })
	if e != 0 || d.U32() != uint32(len(datos)) {
		t.Fatalf("1 MiB write after hello: %d", e)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "big.bin")); !bytes.Equal(b, datos) {
		t.Fatal("the host does not have what was written")
	}
	e, d = c.call(proto.OpRead, func(e *proto.Enc) { e.U64(h); e.U64(0); e.U32(proto.MaxIO) })
	if e != 0 || !bytes.Equal(d.Bytes(proto.MaxIO), datos) {
		t.Fatalf("1 MiB read after hello: %d", e)
	}
	if e, _ := c.call(proto.OpRead, func(e *proto.Enc) { e.U64(h); e.U64(0); e.U32(proto.MaxIO + 1) }); e != proto.EINVAL {
		t.Errorf("read past MaxIO = %d, want EINVAL", e)
	}
}

func TestBigOp(t *testing.T) {
	leer := func(size uint32) []byte {
		e := proto.Request(1, proto.OpRead)
		e.U64(1)
		e.U64(0)
		e.U32(size)
		return e.B
	}
	if bigOp(proto.OpRead, leer(proto.BaseIO)) || !bigOp(proto.OpRead, leer(proto.BaseIO+1)) {
		t.Error("bigOp misjudges reads")
	}
	if bigOp(proto.OpWrite, make([]byte, 100)) || !bigOp(proto.OpWrite, make([]byte, proto.BaseIO+100)) {
		t.Error("bigOp misjudges writes")
	}
	if bigOp(proto.OpStat, make([]byte, proto.BaseIO+100)) {
		t.Error("bigOp counts a stat")
	}
}
