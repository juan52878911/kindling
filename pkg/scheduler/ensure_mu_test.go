package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// M22: pedir servicios que no existen no deja un candado por nombre en
// ensureMu. La ruta /mcp/<servicio> admite cualquier nombre de quien tenga
// token, y cada uno dejaba un mutex para siempre.
func TestEnsureSinSnapshotNoDejaCandado(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/snapshots", "/machines":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	g := New(api.NewClient("unix://"+sock), time.Minute, false, 0)

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Dos por nombre, a la vez: también se cubre a quien esperaba en el
			// candado que se quitó.
			if _, err := g.ensure(context.Background(), fmt.Sprintf("nada-%d", i/2)); err == nil {
				t.Error("ensure de un servicio sin snapshot no falló")
			}
		}(i)
	}
	wg.Wait()
	n := 0
	g.ensureMu.Range(func(k, _ any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("ensureMu guarda %d candados de servicios sin snapshot", n)
	}
}
