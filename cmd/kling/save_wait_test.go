package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/internal/machine"
	"github.com/juan52878911/kindling/pkg/api"
)

// Sin -wait, kling save pide al daemon la misma espera que el daemon da por
// defecto (120 s), no 60 s que recortaban en silencio la de la sonda.
func TestSaveWaitDefaultMatchesAPI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
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
	var got api.CommitRequest
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines/{ref}/commit", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(api.Snapshot{Name: got.Name})
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	if err := cmdSave([]string{"-H", sock, "-force", "m", "t"}); err != nil {
		t.Fatal(err)
	}
	if want := int(machine.DefaultReadyWait.Seconds()); got.ReadyTimeoutSeconds != want || want != 120 {
		t.Fatalf("ready_timeout_seconds = %d, want %d (the API's 120)", got.ReadyTimeoutSeconds, want)
	}
	if err := cmdSave([]string{"-H", sock, "-force", "-wait", "30s", "m", "t"}); err != nil {
		t.Fatal(err)
	}
	if got.ReadyTimeoutSeconds != 30 {
		t.Fatalf("-wait 30s: ready_timeout_seconds = %d", got.ReadyTimeoutSeconds)
	}
}
