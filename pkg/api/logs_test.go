package api

// Test de N3 (A-01): el lado cliente de M-07/D-04. Sin LimitReader, un daemon
// viejo o comprometido que mande una consola de gigabytes haría que el
// cliente (CLI o SDK) cargara todo eso en memoria antes de poder ni imprimir
// una línea.

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogsAcotaLaRespuestaConLimitReader(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "kling.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
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
