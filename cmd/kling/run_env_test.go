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
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// `kling run -e` manda el entorno en el cuerpo de POST /machines (no lo
// hornea en una imagen, ni exige una referencia de Docker), y con -from se
// rechaza sin llegar al daemon.
func TestRunEnviaElEntornoEnLaPeticion(t *testing.T) {
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
	var got api.RunRequest
	var llamadas atomic.Int32
	var viejo atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		caps := []string{api.CapabilityMachineEnv, api.CapabilityDisk}
		if viejo.Load() {
			caps = nil
		}
		_ = json.NewEncoder(w).Encode(api.Info{Version: "x", Capabilities: caps})
	})
	mux.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		llamadas.Add(1)
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.Machine{ID: "0123456789abcdef", Name: "m", StartedAt: ptr(time.Now())})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	if err := cmdRun([]string{"-H", sock, "-image", "mi-imagen", "-e", "KLING_TEST_PW", "-e", "MODE=dev", "-json"}); err != nil {
		t.Fatal(err)
	}
	if got.Image != "mi-imagen" || !slices.Equal(got.Env, []string{"KLING_TEST_PW=la-de-verdad", "MODE=dev"}) {
		t.Fatalf("petición: image %q env %q", got.Image, got.Env)
	}

	err = cmdRun([]string{"-H", sock, "-from", "dorado", "-e", "MODE=dev"})
	if err == nil || !strings.Contains(err.Error(), "-from") || llamadas.Load() != 1 {
		t.Fatalf("-from con -e: err %v, llamadas al daemon %d", err, llamadas.Load())
	}
	err = cmdRun([]string{"-H", sock, "-image", "mi-imagen", "-e", "1MAL=x"})
	if err == nil || llamadas.Load() != 1 {
		t.Fatalf("clave inválida: err %v, llamadas al daemon %d", err, llamadas.Load())
	}
	// Un daemon que no anuncia la capacidad ignoraría el campo: no se manda.
	viejo.Store(true)
	err = cmdRun([]string{"-H", sock, "-image", "mi-imagen", "-e", "MODE=dev"})
	if err == nil || !strings.Contains(err.Error(), "update") || llamadas.Load() != 1 {
		t.Fatalf("daemon antiguo: err %v, llamadas %d", err, llamadas.Load())
	}
	// Igual con -disk: un daemon antiguo daría 512 MiB en silencio.
	err = cmdRun([]string{"-H", sock, "-image", "mi-imagen", "-disk", "2G"})
	if err == nil || !strings.Contains(err.Error(), "-disk") || llamadas.Load() != 1 {
		t.Fatalf("daemon antiguo con -disk: err %v, llamadas %d", err, llamadas.Load())
	}
	viejo.Store(false)
	if err := cmdRun([]string{"-H", sock, "-image", "mi-imagen", "-disk", "2G", "-json"}); err != nil {
		t.Fatal(err)
	}
	if got.DiskMiB != 2048 || llamadas.Load() != 2 {
		t.Fatalf("-disk 2G: disk_mib %d, llamadas %d", got.DiskMiB, llamadas.Load())
	}
}

func ptr[T any](v T) *T { return &v }
