// Package apiclienttest prueba, de caja negra, el comportamiento de conexión
// de api.Client contra un servidor HTTP en un socket unix.
package apiclienttest

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

type cuenta struct {
	net.Listener
	n *atomic.Int64
}

func (c cuenta) Accept() (net.Conn, error) {
	conn, err := c.Listener.Accept()
	if err == nil {
		c.n.Add(1)
	}
	return conn, err
}

func arrancar(t *testing.T, sock string, n *atomic.Int64) *http.Server {
	t.Helper()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/machines", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte("{}"))
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(cuenta{ln, n})
	return srv
}

func TestSocketUnixReutilizaLaConexion(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "k.sock")
	var n atomic.Int64
	srv := arrancar(t, sock, &n)
	defer srv.Close()

	c := api.NewClient("unix://" + sock)
	for i := 0; i < 100; i++ {
		if _, err := c.List(context.Background()); err != nil {
			t.Fatalf("petición %d: %v", i, err)
		}
	}
	if got := n.Load(); got != 1 {
		t.Fatalf("100 peticiones en serie abrieron %d conexiones, quiero 1", got)
	}
}

// Tras reiniciar el daemon, la conexión ociosa está muerta: la siguiente
// petición (un POST también) no debe fallar.
func TestSocketUnixSobreviveAReinicio(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "k.sock")
	var n atomic.Int64
	srv := arrancar(t, sock, &n)
	c := api.NewClient(sock)
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	srv = arrancar(t, sock, &n)
	defer srv.Close()
	if _, err := c.Run(context.Background(), api.RunRequest{}); err != nil {
		t.Fatalf("POST tras el reinicio: %v", err)
	}
	if n.Load() != 2 {
		t.Fatalf("conexiones = %d, quiero 2", n.Load())
	}
}
