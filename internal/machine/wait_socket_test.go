package machine

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juan52878911/kindling/internal/fc"
)

// Un VMM que acepta la conexión y no contesta no puede colgar waitSocket más
// allá de su plazo: cada Ping lleva el suyo (200 ms). Sin él, el primer Ping
// esperaba los 30 s del cliente.
func TestWaitSocketNoSeCuelgaDeUnSocketMudo(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "kws")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "fc.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}
	conns := make(chan net.Conn, 64)
	t.Cleanup(func() {
		ln.Close()
		for len(conns) > 0 {
			(<-conns).Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select { // aceptar y callar
			case conns <- c:
			default:
				c.Close()
			}
		}
	}()

	m := newTestManager(t)
	hecho := make(chan error, 1)
	inicio := time.Now()
	go func() { hecho <- m.waitSocket(context.Background(), fc.New(sock), "ab000000000000ff", 0) }()
	select {
	case err := <-hecho:
		if err == nil {
			t.Fatal("un socket mudo se dio por listo")
		}
		if d := time.Since(inicio); d > 7*time.Second {
			t.Fatalf("waitSocket tardó %s", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waitSocket se colgó de un socket que no contesta")
	}
}
