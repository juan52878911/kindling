package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// `kling start` manda el entorno en el cuerpo (nunca en un argv: -e KEY lo
// toma del entorno de kling) y, contra un daemon sin la ruta, dice que hay que
// actualizarlo en vez de un 404 sin contexto.
func TestStartEnviaElEntornoYDetectaDaemonViejo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("KLING_TEST_PW", "la-de-verdad")

	dir, err := os.MkdirTemp("/tmp", "kt")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip(err)
	}
	var got api.StartRequest
	var ruta atomic.Value
	var viejo atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		caps := []string{api.CapabilityStart}
		if viejo.Load() {
			caps = nil
		}
		_ = json.NewEncoder(w).Encode(api.Info{Version: "x", Capabilities: caps})
	})
	mux.HandleFunc("POST /machines/{ref}/start", func(w http.ResponseWriter, r *http.Request) {
		if viejo.Load() {
			http.NotFound(w, r)
			return
		}
		ruta.Store(r.PathValue("ref"))
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(api.Machine{ID: "0123456789abcdef", Name: "pg", State: api.StateRunning})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	if err := cmdStart([]string{"-H", sock, "-e", "KLING_TEST_PW", "pg"}); err != nil {
		t.Fatal(err)
	}
	if ruta.Load() != "pg" || !slices.Equal(got.Env, []string{"KLING_TEST_PW=la-de-verdad"}) {
		t.Fatalf("petición: ref %v env %q", ruta.Load(), got.Env)
	}
	if err := cmdStart([]string{"-H", sock, "-e", "1MAL=x", "pg"}); err == nil {
		t.Fatal("una clave inválida llegó al daemon")
	}
	viejo.Store(true)
	err = cmdStart([]string{"-H", sock, "pg"})
	if err == nil || !strings.Contains(err.Error(), "update it") {
		t.Fatalf("daemon antiguo: %v", err)
	}
}
