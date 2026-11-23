package api

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// daemonContado sirve en un socket unix un exec en flujo (como handleExec) y
// un POST /sandboxes, y cuenta las conexiones nuevas que acepta.
func daemonContado(tb testing.TB) (*Client, *atomic.Int64) {
	tb.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines/{ref}/exec", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, `{"stream":"stdout","data":"aG9sYQ=="}`+"\n")
		fl.Flush()
		_, _ = io.WriteString(w, `{"exit":0,"duration_ms":1}`+"\n")
		fl.Flush()
	})
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"sb1","name":"sb1","state":"running"}`+"\n")
	})
	dir, err := os.MkdirTemp("", "kl")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		tb.Fatalf("socket unix: %v", err)
	}
	var nuevas atomic.Int64
	srv := &http.Server{Handler: mux, ConnState: func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			nuevas.Add(1)
		}
	}}
	go srv.Serve(ln)
	tb.Cleanup(func() { srv.Close() })
	return NewClient(sock), &nuevas
}

// BenchmarkClienteLargoConexiones mide cuántas conexiones nuevas al daemon
// abre cada llamada de las que van por c.long (G-07).
func BenchmarkClienteLargoConexiones(b *testing.B) {
	ctx := context.Background()
	b.Run("exec", func(b *testing.B) {
		c, nuevas := daemonContado(b)
		nuevas.Store(0)
		for i := 0; i < b.N; i++ {
			res, err := c.Exec(ctx, "m", ExecRequest{Cmd: []string{"true"}}, nil)
			if err != nil || string(res.Stdout) != "hola" {
				b.Fatalf("%+v %v", res, err)
			}
		}
		b.ReportMetric(float64(nuevas.Load())/float64(b.N), "conns/op")
	})
	b.Run("create-sandbox", func(b *testing.B) {
		c, nuevas := daemonContado(b)
		nuevas.Store(0)
		for i := 0; i < b.N; i++ {
			if _, err := c.CreateSandbox(ctx, SandboxRequest{}); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(nuevas.Load())/float64(b.N), "conns/op")
	})
}
