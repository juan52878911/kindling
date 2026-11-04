package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

// Una imagen sin servicio no espera a que sirva (agotaba el plazo entero) y,
// sin sonda ni ganchos, tampoco pide al daemon la espera de listo. Con
// servicio, o con un agente que no sabe decirlo, se espera como siempre.
func TestSaveSinServicioNoEspera(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	casos := []struct {
		nombre                  string
		service, ready          string // cuerpos; "" = 404
		quiereProbe, quiereSkip bool
	}{
		{"sin servicio ni sonda", `{"declared":false}`, `{"ready":true}`, false, true},
		{"sin servicio con sonda", `{"declared":false}`, `{"ready":false,"probe":true}`, false, false},
		{"con servicio", `{"declared":true,"running":true}`, `{"ready":true}`, true, false},
		{"agente sin /service", "", "", true, false},
	}
	for _, tc := range casos {
		t.Run(tc.nombre, func(t *testing.T) {
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
			probe := false
			mux := http.NewServeMux()
			mux.HandleFunc("POST /machines/{ref}/guest", func(w http.ResponseWriter, r *http.Request) {
				var req api.GuestRequest
				_ = json.NewDecoder(r.Body).Decode(&req)
				var resp api.GuestResponse
				switch {
				case req.ProbeOnly:
					probe = true
					resp.Status = 200
				case strings.HasPrefix(req.Path, api.GuestServicePath) && tc.service != "":
					resp = api.GuestResponse{Status: 200, Body: tc.service}
				case req.Path == api.GuestReadyPath && tc.ready != "":
					resp = api.GuestResponse{Status: 200, Body: tc.ready}
				default:
					resp.Status = 404
				}
				_ = json.NewEncoder(w).Encode(resp)
			})
			mux.HandleFunc("POST /machines/{ref}/commit", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewDecoder(r.Body).Decode(&got)
				_ = json.NewEncoder(w).Encode(api.Snapshot{Name: got.Name})
			})
			srv := &http.Server{Handler: mux}
			go srv.Serve(ln)
			t.Cleanup(func() { srv.Close() })

			if err := cmdSave([]string{"-H", sock, "m", "t"}); err != nil {
				t.Fatal(err)
			}
			if probe != tc.quiereProbe || got.SkipReady != tc.quiereSkip {
				t.Fatalf("espera al puerto %v (quiero %v), skip_ready %v (quiero %v)", probe, tc.quiereProbe, got.SkipReady, tc.quiereSkip)
			}
		})
	}
}
