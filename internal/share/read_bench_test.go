package share

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// BenchmarkLectura mide el camino de una lectura de MaxIO (1 MiB) por una
// sesión: petición, ReadAt, codificación y trama. El lado del agente lee en un
// búfer fijo, así que las reservas que cuenta son las del daemon.
//
//	go test -run '^$' -bench Lectura -benchmem ./internal/share/
func BenchmarkLectura(b *testing.B) {
	dir := b.TempDir()
	datos := bytes.Repeat([]byte("kindling"), proto.MaxIO/8)
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), datos, 0o644); err != nil {
		b.Fatal(err)
	}
	srv, err := Open(dir, true, "")
	if err != nil {
		b.Fatal(err)
	}
	defer srv.Close()
	a, conn := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx, a); close(done) }()
	defer func() { cancel(); conn.Close(); <-done }()

	var id uint64
	resp := make([]byte, proto.MaxFrame)
	call := func(op byte, build func(*proto.Enc)) *proto.Dec {
		id++
		e := proto.Request(id, op)
		build(e)
		if err := proto.WriteFrame(conn, e.B); err != nil {
			b.Fatal(err)
		}
		var head [4]byte
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			b.Fatal(err)
		}
		n := binary.BigEndian.Uint32(head[:])
		if _, err := io.ReadFull(conn, resp[:n]); err != nil {
			b.Fatal(err)
		}
		d := proto.NewDec(resp[:n])
		if got := d.U64(); got != id {
			b.Fatalf("response id %d, want %d", got, id)
		}
		if e := d.U32(); e != 0 {
			b.Fatalf("op %d: errno %d", op, e)
		}
		return d
	}
	call(proto.OpHello, func(e *proto.Enc) { e.U32(proto.MaxIO) })
	h := call(proto.OpOpen, func(e *proto.Enc) { e.Str("big.bin"); e.U32(proto.FlagRead) }).U64()

	b.SetBytes(proto.MaxIO)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d := call(proto.OpRead, func(e *proto.Enc) { e.U64(h); e.U64(0); e.U32(proto.MaxIO) })
		if got := d.Bytes(proto.MaxIO); len(got) != proto.MaxIO {
			b.Fatalf("read %d bytes, want %d", len(got), proto.MaxIO)
		}
	}
}
