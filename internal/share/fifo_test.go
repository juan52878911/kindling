package share

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	proto "github.com/juan52878911/kindling/internal/shareproto"
)

// TestFIFONoCuelga: un FIFO del host con el nombre de un directorio no deja
// esperando a quien lo abre. Antes, listar el FIFO (o renombrar algo «dentro»
// de él) se quedaba en open() esperando a un escritor, y la sesión no podía
// cerrarse: Serve esperaba a la operación, y con él la parada o la
// congelación de la máquina.
func TestFIFONoCuelga(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	must(t, syscall.Mkfifo(fifo, 0o644))
	must(t, os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644))
	srv, err := Open(dir, false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// Si algo se queda en open(), abrir el FIFO por el otro lado lo suelta y
	// el test acaba en vez de colgarse.
	destrabar := func() {
		if f, err := os.OpenFile(fifo, os.O_RDWR|syscall.O_NONBLOCK, 0); err == nil {
			time.Sleep(50 * time.Millisecond)
			f.Close()
		}
	}

	ops := []struct {
		nombre string
		op     byte
		build  func(*proto.Enc)
	}{
		{"list", proto.OpList, func(e *proto.Enc) { e.Str("fifo"); e.U64(0); e.U32(16) }},
		{"rename into", proto.OpRename, func(e *proto.Enc) { e.Str("f"); e.Str("fifo/g"); e.U32(0) }},
		{"rename from", proto.OpRename, func(e *proto.Enc) { e.Str("fifo/g"); e.Str("h"); e.U32(0) }},
	}
	for _, o := range ops {
		a, b := net.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		served := make(chan struct{})
		go func() { _ = srv.Serve(ctx, a); close(served) }()
		got := make(chan uint32, 1)
		go func() {
			// Sin cliente.call: su t.Fatal no vale fuera de la goroutine del test.
			e := proto.Request(1, o.op)
			o.build(e)
			if proto.WriteFrame(b, e.B) != nil {
				got <- ^uint32(0)
				return
			}
			body, err := proto.ReadFrame(b)
			if err != nil {
				got <- ^uint32(0)
				return
			}
			d := proto.NewDec(body)
			_ = d.U64()
			got <- d.U32()
		}()
		select {
		case e := <-got:
			if e != proto.ENOTDIR {
				t.Errorf("%s through a FIFO = %d, want ENOTDIR", o.nombre, e)
			}
		case <-time.After(2 * time.Second):
			t.Errorf("%s through a FIFO is still waiting after 2 s", o.nombre)
		}
		// Y la sesión se cierra: es lo que espera la parada de la máquina.
		cancel()
		b.Close()
		select {
		case <-served:
		case <-time.After(2 * time.Second):
			t.Errorf("%s: Serve does not return after closing the session", o.nombre)
			destrabar()
			<-served
		}
	}

	// La foto de la vigilancia tampoco se queda esperando.
	w, err := newWatcher(srv, func([]byte) {})
	if err != nil {
		t.Fatal(err)
	}
	defer w.close()
	hecho := make(chan struct{})
	go func() {
		w.snapshot("fifo", func(Ident) bool { return false })
		close(hecho)
	}()
	select {
	case <-hecho:
	case <-time.After(2 * time.Second):
		t.Errorf("watch snapshot of a FIFO is still waiting after 2 s")
		destrabar()
		<-hecho
	}
}
