package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// Los códigos de salida son contrato (docs/guide.md, "Exit codes"). Estos
// tests comprueban el cableado de punta a punta: que el comando, tal como lo
// corre main, sale con el código que toca, no solo que codigoDeSalida mapea
// bien un error que alguien construyó a mano.

// socketTemporal es una ruta de socket Unix corta (los de t.TempDir() pasan
// del límite de 104 bytes en macOS) en un directorio que se borra al acabar.
func socketTemporal(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kt")
	if err != nil {
		t.Skipf("no /tmp for a unix socket: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}

// TestStatusJSONDaemonCaidoSaleCon1: sin daemon, status sale con 1 en texto y
// también con -json, y el JSON sigue saliendo entero y dice por qué.
func TestStatusJSONDaemonCaidoSaleCon1(t *testing.T) {
	hermetic(t)
	t.Setenv("KLING_HOST", "unix://"+socketTemporal(t)) // nadie escucha ahí

	for _, args := range [][]string{nil, {"-json"}} {
		out, err := capturarStdout(t, func() error { return cmdStatus(args) })
		if got := codigoDeSalida(err); got != salidaError {
			t.Errorf("status %v: exit %d (err %v), want %d", args, got, err, salidaError)
		}
		if len(args) == 0 {
			continue
		}
		var rep struct {
			Endpoint string `json:"endpoint"`
			Daemon   struct {
				OK    bool   `json:"ok"`
				Error string `json:"error"`
			} `json:"daemon"`
		}
		if err := json.Unmarshal(out, &rep); err != nil {
			t.Fatalf("status -json does not print JSON with the daemon down: %v\n%s", err, out)
		}
		if rep.Daemon.OK || rep.Daemon.Error == "" || rep.Endpoint == "" {
			t.Errorf("status -json with the daemon down: %s", out)
		}
	}
}

// TestRunAgenteSinCapacidadSaleCon5: run -e contra una imagen cuyo agente no
// lee el entorno sale con 5, y con -json imprime igual la máquina.
func TestRunAgenteSinCapacidadSaleCon5(t *testing.T) {
	hermetic(t)
	maquina := api.Machine{
		ID:    "bbbbbbbbbbbbbbbb",
		Name:  "m1",
		Agent: &api.GuestAgent{Agent: "kling-guest", Version: "0.18.0", Caps: []string{api.GuestCapExec}},
	}
	m := http.NewServeMux()
	responder := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	m.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
		responder(w, api.Info{Version: "test", Capabilities: []string{api.CapabilityMachineEnv}})
	})
	m.HandleFunc("POST /machines", func(w http.ResponseWriter, r *http.Request) {
		responder(w, &maquina)
	})
	sock := socketTemporal(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("cannot listen on a unix socket: %v", err)
	}
	srv := &http.Server{Handler: m}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	t.Setenv("KLING_HOST", "unix://"+sock)

	out, err := capturarStdout(t, func() error { return cmdRun([]string{"-json", "-e", "A=1"}) })
	if got := codigoDeSalida(err); got != salidaAgenteSinCap {
		t.Fatalf("run -e with an agent without %q: exit %d (err %v), want %d", api.GuestCapEnv, got, err, salidaAgenteSinCap)
	}
	var mc api.Machine
	if err := json.Unmarshal(out, &mc); err != nil || mc.ID != maquina.ID {
		t.Errorf("run -json does not print the machine before exiting with %d (%v):\n%s", salidaAgenteSinCap, err, out)
	}
}
