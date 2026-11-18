package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// S7: el ref va escapado en la ruta, así que un ref con "/" o ".." llega
// entero al handler de su ruta y no a otra; y el cliente no sigue
// redirecciones (ServeMux redirige las rutas con "..").
func TestRefEscapadoYSinRedirecciones(t *testing.T) {
	dir, err := os.MkdirTemp("", "kl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}
	var refs []string
	otra := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /machines/{ref}", func(w http.ResponseWriter, r *http.Request) {
		refs = append(refs, r.PathValue("ref"))
		_ = json.NewEncoder(w).Encode(Machine{Name: r.PathValue("ref")})
	})
	mux.HandleFunc("POST /machines/{ref}/stop", func(w http.ResponseWriter, r *http.Request) {
		refs = append(refs, r.PathValue("ref"))
		_ = json.NewEncoder(w).Encode(Machine{Name: r.PathValue("ref")})
	})
	mux.HandleFunc("GET /snapshots/{name}", func(w http.ResponseWriter, r *http.Request) {
		otra++
		_ = json.NewEncoder(w).Encode(Machine{})
	})
	mux.HandleFunc("GET /machines/{ref}/ready", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/snapshots/x", http.StatusTemporaryRedirect)
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const raro = "a/../../snapshots/x"
	if _, err := c.Get(ctx, raro); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := c.Stop(ctx, raro); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if otra != 0 || len(refs) != 2 || refs[0] != raro || refs[1] != raro {
		t.Fatalf("refs %q, peticiones a otra ruta %d", refs, otra)
	}
	if _, err := c.Ready(ctx, "m", 0); err == nil || otra != 0 {
		t.Fatalf("una redirección no debe seguirse: err=%v, otra=%d", err, otra)
	}
}
