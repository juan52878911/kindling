package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// demonioFalso sirve h en un socket unix y devuelve su endpoint para -H.
func demonioFalso(t *testing.T, h http.Handler) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, err := net.Listen("unix", dir+"/s")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return "unix://" + dir + "/s"
}

// salidaDe ejecuta f y devuelve lo que escribió en la salida estándar.
func salidaDe(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestExecDevuelveElPidYProcEsperaConSuCodigo(t *testing.T) {
	var recibido api.ProcessStartRequest
	veces := 0
	h := http.NewServeMux()
	h.HandleFunc("POST /machines/m/processes", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&recibido)
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(api.ProcessInfo{PID: 4242, State: api.ProcessRunning})
	})
	h.HandleFunc("GET /machines/m/processes/4242", func(w http.ResponseWriter, r *http.Request) {
		veces++
		p := api.ProcessInfo{PID: 4242, State: api.ProcessRunning}
		if veces >= 2 {
			c := 9
			p.State, p.ExitCode = api.ProcessExited, &c
		}
		_ = json.NewEncoder(w).Encode(p)
	})
	h.HandleFunc("GET /machines/m/processes", func(w http.ResponseWriter, r *http.Request) {
		c := 0
		_ = json.NewEncoder(w).Encode([]api.ProcessInfo{{PID: 7, Cmd: []string{"sleep", "9"}, State: api.ProcessExited, ExitCode: &c, StartedAt: time.Now()}})
	})
	ep := demonioFalso(t, h)

	var code int
	var err error
	out := salidaDe(t, func() {
		code, err = cmdExec([]string{"-H", ep, "-d", "-s", "dev", "-w", "/srv", "m", "--", "sleep", "9"})
	})
	if err != nil || code != 0 || strings.TrimSpace(out) != "4242" {
		t.Fatalf("exec -d: code=%d err=%v out=%q", code, err, out)
	}
	if recibido.Session != "dev" || recibido.Dir != "/srv" || strings.Join(recibido.Cmd, " ") != "sleep 9" {
		t.Fatalf("petición %+v", recibido)
	}
	if code, err := cmdExec([]string{"-H", ep, "-d", "-i", "m", "true"}); code != 2 || err == nil {
		t.Fatalf("-d con -i debe ser un error de uso: %d %v", code, err)
	}

	if code, err := cmdProc([]string{"wait", "-H", ep, "m", "4242"}); code != 9 || err != nil {
		t.Fatalf("proc wait: code=%d err=%v", code, err)
	}
	out = salidaDe(t, func() { _, err = cmdProc([]string{"ls", "-H", ep, "m"}) })
	if err != nil || !strings.Contains(out, "exit 0") || !strings.Contains(out, "sleep 9") {
		t.Fatalf("proc ls: %v\n%s", err, out)
	}
	if code, err := cmdProc([]string{"nope"}); code != 2 || err == nil {
		t.Fatalf("subcomando desconocido: %d %v", code, err)
	}
	if code, err := cmdProc([]string{"status", "-H", ep, "m", "xx"}); err == nil {
		t.Fatalf("pid inválido: %d %v", code, err)
	}
}

func TestEstadoCorto(t *testing.T) {
	c := 3
	for want, p := range map[string]api.ProcessInfo{
		"running":     {State: api.ProcessRunning},
		"exit 3":      {State: api.ProcessExited, ExitCode: &c},
		"killed KILL": {State: api.ProcessExited, ExitCode: &c, Signal: "KILL"},
	} {
		if got := estadoCorto(p); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
