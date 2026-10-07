package api

// Test de N3 (A-01): el lado cliente de M-07/D-04. Sin LimitReader, un daemon
// viejo o comprometido que mande una consola de gigabytes haría que el
// cliente (CLI o SDK) cargara todo eso en memoria antes de poder ni imprimir
// una línea.

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogsAcotaLaRespuestaConLimitReader(t *testing.T) {
	// En un temporal corto y no en t.TempDir(): en macOS la ruta con el nombre
	// del test pasa de los 104 bytes de sun_path, y el test se saltaba siempre.
	dir, err := os.MkdirTemp("", "kl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("socket unix: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/machines/svc/logs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		chunk := strings.Repeat("x", 1<<20)
		for i := 0; i < 10; i++ { // 10 MiB, > maxLogsResponse (8 MiB)
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	got, err := c.Logs(ctx, "svc", 0)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	if int64(len(got)) != maxLogsResponse {
		t.Fatalf("Logs() devolvió %d bytes, quería exactamente maxLogsResponse (%d)", len(got), maxLogsResponse)
	}
}
