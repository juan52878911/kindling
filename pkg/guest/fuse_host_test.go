package guest

import (
	"bytes"
	"context"
	"encoding/gob"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	hostshare "github.com/juan52878911/kindling/internal/share"
)

// servidorHost es HostFS sobre un /dev/fuse falso, con la sesión de un
// hostshare.Server sobre dir: lo que monta kling-hv por cada carpeta.
type servidorHost struct {
	dev  *devFalso
	h    *HostFS
	srv  *hostshare.Server
	stop func()
}

func servirHost(t *testing.T, dir string, h *HostFS, dev *devFalso) *servidorHost {
	t.Helper()
	srv, err := hostshare.Open(dir, false, t.Name())
	if err != nil {
		t.Fatal(err)
	}
	h.fs.grace = 2 * time.Second
	go h.Serve()
	s := &servidorHost{dev: dev, h: h, srv: srv}
	s.stop = s.conectar()
	t.Cleanup(func() {
		s.stop()
		close(dev.in)
		srv.Close()
	})
	return s
}

func (s *servidorHost) conectar() func() {
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.srv.Serve(ctx, a); close(done) }()
	s.h.Attach(b)
	return func() { cancel(); b.Close(); <-done }
}

// El estado de HostFS (lo que kling-hv guarda en el snapshot) devuelve, en un
// servidor nuevo, los mismos nodos, el fichero abierto (que se reabre solo
// por ruta), el directorio abierto y lo pendiente de la escritura diferida:
// el kernel restaurado sigue nombrándolos.
func TestHostFSEstado(t *testing.T) {
	dir := carpeta(t)
	dev := newDevFalso()
	s := servirHost(t, dir, NewHostFS(dev, "kling-share-0", false), dev)
	if e, _ := dev.pedir(t, opInit, 0, u32s(7, 38, 1<<20, 0xffffffff)); e != 0 {
		t.Fatalf("init: %d", e)
	}
	look := func(d *devFalso, parent uint64, name string) uint64 {
		e, out := d.pedir(t, opLookup, parent, cstr(name))
		if e != 0 {
			t.Fatalf("lookup %s: %d", name, e)
		}
		return le.Uint64(out)
	}
	hello := look(dev, rootID, "hello.txt")
	sub := look(dev, rootID, "sub")
	_, out := dev.pedir(t, opOpen, hello, u32s(linuxORdwr, 0))
	fh := le.Uint64(out)
	e, out := dev.pedir(t, opOpendir, sub, u32s(0, 0))
	if e != 0 {
		t.Fatalf("opendir: %d", e)
	}
	dfh := le.Uint64(out)
	// Lo pendiente de la escritura diferida: un trozo ya contestado al
	// kernel que no llegó al host (como si la sesión se cortara al congelar).
	n := s.h.fs.nodes.get(hello)
	w := wbOf(n)
	w.mu.Lock()
	w.flying = append(w.flying, &wbChunk{h: s.h.fs.file(fh), off: 0, data: []byte("HOLA!")})
	w.bytes += 5
	w.mu.Unlock()
	nodos := s.h.Nodes()

	st, err := s.h.State()
	if err != nil {
		t.Fatal(err)
	}
	// El proceso de kling-hv muere: la sesión y el servidor también.
	s.stop()
	s.stop = func() {}
	w.mu.Lock()
	w.flying = nil // que el viejo no lo mande
	w.mu.Unlock()

	dev2 := newDevFalso()
	h2, err := NewHostFSFromState(dev2, "kling-share-0", false, st)
	if err != nil {
		t.Fatal(err)
	}
	if h2.Nodes() != nodos {
		t.Fatalf("restored %d nodes, had %d", h2.Nodes(), nodos)
	}
	servirHost(t, dir, h2, dev2)
	// El mismo nodo sin LOOKUP, el fichero abierto sin OPEN.
	if e, _ := dev2.pedir(t, opGetattr, hello, u32s(0, 0), u64s(0)); e != 0 {
		t.Fatalf("getattr of a restored node: %d", e)
	}
	e, data := dev2.pedir(t, opRead, hello, readIn(fh, 0, 20))
	if e != 0 || string(data) != "HOLA! from the host\n" {
		t.Fatalf("read through the restored handle: %d %q", e, data)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "hello.txt")); string(b) != "HOLA! from the host\n" {
		t.Fatalf("the pending write did not reach the host: %q", b)
	}
	if e, _ := dev2.pedir(t, opReaddir, sub, readIn(dfh, 0, 4096)); e != 0 {
		t.Fatalf("readdir through the restored directory handle: %d", e)
	}
	// Y un nodo nuevo sigue numerando detrás de lo que había.
	if err := os.WriteFile(filepath.Join(dir, "sub", "nuevo"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if id := look(dev2, sub, "nuevo"); id <= sub || id <= hello {
		t.Fatalf("new node id %d reuses an old one", id)
	}
}

// Un estado roto (no lo escribió este programa, o se corrompió) se rechaza
// entero en vez de dejar una tabla de nodos incoherente.
func TestHostFSEstadoRoto(t *testing.T) {
	cod := func(st hostState) []byte {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(&st); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	ok := hostState{Version: hostStateVersion, Next: 4, Nodes: []hostNode{{ID: 2, Parent: rootID, Name: "a"}, {ID: 3, Parent: 2, Name: "b"}}}
	if _, err := NewHostFSFromState(newDevFalso(), "t", false, cod(ok)); err != nil {
		t.Fatalf("a good state: %v", err)
	}
	casos := map[string]hostState{
		"versión":         {Version: 99},
		"id repetido":     {Version: hostStateVersion, Next: 4, Nodes: []hostNode{{ID: 2, Parent: rootID, Name: "a"}, {ID: 2, Parent: rootID, Name: "b"}}},
		"id de la raíz":   {Version: hostStateVersion, Next: 4, Nodes: []hostNode{{ID: rootID, Parent: rootID, Name: "a"}}},
		"id tras next":    {Version: hostStateVersion, Next: 3, Nodes: []hostNode{{ID: 3, Parent: rootID, Name: "a"}}},
		"sin padre":       {Version: hostStateVersion, Next: 4, Nodes: []hostNode{{ID: 2, Parent: 3, Name: "a"}}},
		"nombre inválido": {Version: hostStateVersion, Next: 4, Nodes: []hostNode{{ID: 2, Parent: rootID, Name: "a/b"}}},
		"nombre doble":    {Version: hostStateVersion, Next: 4, Nodes: []hostNode{{ID: 2, Parent: rootID, Name: "a"}, {ID: 3, Parent: rootID, Name: "a"}}},
		"ciclo":           {Version: hostStateVersion, Next: 4, Nodes: []hostNode{{ID: 2, Parent: 3, Name: "a"}, {ID: 3, Parent: 2, Name: "b"}}},
		"handle sin nodo": {Version: hostStateVersion, Next: 2, NextFH: 1, Files: []hostFile{{FH: 1, Node: 7}}},
		"handle tras next": {Version: hostStateVersion, Next: 3, NextFH: 1, Nodes: []hostNode{{ID: 2, Parent: rootID, Name: "a"}},
			Files: []hostFile{{FH: 2, Node: 2}}},
		"pendiente sin nodo": {Version: hostStateVersion, Next: 2, Pending: []hostWB{{Node: 9}}},
	}
	for nombre, st := range casos {
		if _, err := NewHostFSFromState(newDevFalso(), "t", false, cod(st)); err == nil {
			t.Errorf("%s: accepted", nombre)
		}
	}
	if _, err := NewHostFSFromState(newDevFalso(), "t", false, []byte("basura")); err == nil {
		t.Error("garbage: accepted")
	}
}
