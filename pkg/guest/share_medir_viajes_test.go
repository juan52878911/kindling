package guest

// Viajes al host de las carpetas compartidas, sin KVM ni root: el agente
// (fuseFS) con un /dev/fuse falso y el servidor del host, unidos por TCP de
// loopback. El test hace de kernel y cuenta las tramas que salen hacia el host.

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	hostshare "github.com/juan52878911/kindling/internal/share"
	share "github.com/juan52878911/kindling/internal/shareproto"
)

type connContada struct {
	net.Conn
	tramas atomic.Int64
}

func (c *connContada) Write(p []byte) (int, error) {
	c.tramas.Add(1)
	return c.Conn.Write(p)
}

type banco struct {
	dev       *devFalso
	fs        *fuseFS
	conn      *connContada
	initFlags uint32
}

func montarTCP(t *testing.T, dir string, ro, watch bool) *banco {
	t.Helper()
	srv, err := hostshare.Open(dir, ro, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_ = srv.Serve(ctx, c)
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	cc := &connContada{Conn: c}
	dev := newDevFalso()
	fs := newFuseFS(dev, "/work", ro)
	go fs.serve()
	s := newShareSession(cc)
	s.hello()
	if watch && !s.watch() {
		t.Fatal("the host does not watch")
	}
	fs.attach(s)
	t.Cleanup(func() {
		cancel()
		c.Close()
		ln.Close()
		<-done
		close(dev.in)
		srv.Close()
	})
	e, out := dev.pedir(t, opInit, 0, u32s(7, 38, 1<<20, 0xffffffff))
	if e != 0 {
		t.Fatalf("init: %d", e)
	}
	if s.maxIO != share.MaxIO {
		t.Fatalf("maxIO %d", s.maxIO)
	}
	return &banco{dev: dev, fs: fs, conn: cc, initFlags: le.Uint32(out[12:])}
}

func (b *banco) n() int64 { return b.conn.tramas.Load() }

const (
	opReaddirplusK   = 44
	initReaddirplusK = 1 << 13
)

// lookupK es un LOOKUP como lo hace el kernel: con su caché de entradas. Una
// entrada negativa (nodeid 0) con validez no se vuelve a preguntar.
func (b *banco) lookupK(t *testing.T, neg map[string]time.Time, name string) (uint64, uint32) {
	t.Helper()
	if until, ok := neg[name]; ok && time.Now().Before(until) {
		return 0, share.ENOENT
	}
	e, out := b.dev.pedir(t, opLookup, rootID, cstr(name))
	if e == 0 && le.Uint64(out) == 0 {
		neg[name] = time.Now().Add(time.Duration(le.Uint64(out[16:])) * time.Second)
		return 0, share.ENOENT
	}
	if e != 0 {
		return 0, e
	}
	return le.Uint64(out), 0
}

func llenar(t *testing.T, dir string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%04d.txt", i)), []byte("hola\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// ls -l de 1000 ficheros: el kernel abre el directorio, lo lee (con
// READDIRPLUS si el agente lo ofreció) y hace un LOOKUP por cada entrada que
// el READDIRPLUS no trajo ya.
func TestMedirViajes(t *testing.T) {
	dir := t.TempDir()
	const N = 1000
	llenar(t, dir, N)
	b := montarTCP(t, dir, true, true)

	a0 := b.n()
	e, out := b.dev.pedir(t, opOpendir, rootID, u32s(0, 0))
	if e != 0 {
		t.Fatal(e)
	}
	dh := le.Uint64(out)
	plus := b.initFlags&initReaddirplusK != 0
	op := uint32(opReaddir)
	if plus {
		op = opReaddirplusK
	}
	conAttr := map[string]bool{}
	nombres := 0
	for off := uint64(0); ; {
		e, out := b.dev.pedir(t, op, rootID, readIn(dh, off, 4096))
		if e != 0 || len(out) == 0 {
			break
		}
		for len(out) > 0 {
			if plus {
				id := le.Uint64(out)
				out = out[entryOutSize:]
				off = le.Uint64(out[8:])
				nl := int(le.Uint32(out[16:]))
				if id != 0 {
					conAttr[string(out[24:24+nl])] = true
				}
			} else {
				off = le.Uint64(out[8:])
			}
			nl := int(le.Uint32(out[16:]))
			nombres++
			out = out[(24+nl+7)&^7:]
		}
	}
	if nombres < N {
		t.Fatalf("READDIR returned %d entries", nombres)
	}
	neg := map[string]time.Time{}
	for i := 0; i < N; i++ {
		name := fmt.Sprintf("f%04d.txt", i)
		if conAttr[name] {
			continue
		}
		if _, e := b.lookupK(t, neg, name); e != 0 {
			t.Fatal(e)
		}
	}
	if v := b.n() - a0; v > 12 {
		t.Errorf("ls -l of %d files took %d round trips to the host, want <= 12", N, v)
	}
}

// Un nombre que no existe se pregunta una vez: el kernel guarda el ENOENT.
func TestMedirNegativo(t *testing.T) {
	dir := t.TempDir()
	llenar(t, dir, 3)
	b := montarTCP(t, dir, true, true)
	neg := map[string]time.Time{}
	if _, e := b.lookupK(t, neg, "no-existe"); e != share.ENOENT {
		t.Fatalf("first lookup: %d", e)
	}
	a := b.n()
	if _, e := b.lookupK(t, neg, "no-existe"); e != share.ENOENT {
		t.Fatalf("second lookup: %d", e)
	}
	if v := b.n() - a; v != 0 {
		t.Errorf("second LOOKUP of a missing name took %d round trips, want 0", v)
	}
}

// En rw, un open repetido de un fichero que no cambió conserva la caché.
func TestMedirOpenRW(t *testing.T) {
	dir := t.TempDir()
	llenar(t, dir, 1)
	b := montarTCP(t, dir, false, true)
	_, out := b.dev.pedir(t, opLookup, rootID, cstr("f0000.txt"))
	nodo := le.Uint64(out)
	abrir := func() uint32 {
		e, out := b.dev.pedir(t, opOpen, nodo, u32s(2, 0))
		if e != 0 {
			t.Fatal(e)
		}
		b.dev.pedir(t, opRelease, nodo, u64s(le.Uint64(out), 0, 0))
		return le.Uint32(out[8:])
	}
	abrir()
	if abrir()&fopenKeepCache == 0 {
		t.Error("second rw open of an unchanged file does not keep the page cache")
	}
	// Tras un cambio hecho en el host, el open siguiente no la conserva.
	if err := os.WriteFile(filepath.Join(dir, "f0000.txt"), []byte("otra cosa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if abrir()&fopenKeepCache != 0 {
		t.Error("open after a host change kept the page cache")
	}
}
