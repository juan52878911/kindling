package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// daemonRoto es un daemon falso por socket Unix: lista los snapshots dados,
// falla al arrancar máquinas (POST → 500) y al grabar anotaciones (PUT → put).
func daemonRoto(t *testing.T, snaps string, put int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/info":
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/snapshots":
			_, _ = w.Write([]byte(snaps))
		case r.Method == http.MethodPut:
			w.WriteHeader(put)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"restore failed"}`))
		}
	}))
	srv.Listener.Close()
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	t.Setenv("KLING_CONFIG", filepath.Join(dir, "config.json"))
	return sock
}

// `kling mcp health` con un servicio que no arranca y un daemon que no deja
// grabar la salud: antes salía con 0 y sin decir por qué había fallado.
func TestHealthCuentaElFalloAunqueNoSePuedaGrabar(t *testing.T) {
	sock := daemonRoto(t, `[]`, http.StatusNotFound)
	var err error
	out, _ := capturarSalida(t, func() { err = mcpHealth([]string{"-H", sock, "-wait", "1s", "noexiste"}) })
	if err == nil {
		t.Fatalf("mcp health returned nil with the only service down:\n%s", out)
	}
	if !strings.Contains(out, "unhealthy") || !strings.Contains(out, "couldn't record health") {
		t.Errorf("the probe failure or the record failure is not shown:\n%s", out)
	}
}

func capturarSalida(t *testing.T, f func()) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- b.String()
	}()
	f()
	w.Close()
	os.Stdout = prev
	return <-done, nil
}
