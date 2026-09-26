package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// fakeDaemon sirve mux en un socket Unix y devuelve un cliente del API que le
// habla. El directorio va en /tmp y no en t.TempDir(): en macOS éste es tan
// largo que la ruta del socket pasa de los 104 bytes que admite sun_path.
func fakeDaemon(t *testing.T, mux http.Handler) *api.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kt")
	if err != nil {
		t.Skipf("no /tmp for a unix socket: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot listen on a unix socket: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return api.NewClient(sock)
}

func fakeSnapshotsMux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "tpl" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"snapshot \"` + r.PathValue("name") + `\" does not exist"}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"tpl","image":"toolchain"}`))
	})
	return m
}
