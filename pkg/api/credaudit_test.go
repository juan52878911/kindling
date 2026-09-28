package api

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

// CredAudit manda los filtros como query, decodifica el NDJSON y traduce los
// errores: el del daemon tal cual y un 404 sin JSON (daemon anterior) a algo
// que diga qué hacer.
func TestCredAuditCliente(t *testing.T) {
	dir, err := os.MkdirTemp("", "kl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "k.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("no se puede abrir un socket unix aquí: %v", err)
	}
	var query string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /machines/svc/credaudit", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = w.Write([]byte(`{"ts":"2026-09-28T10:00:00Z","kind":"http","method":"GET","host":"a.com","path":"/x","status":200,"creds":["KEY"],"req_bytes":0,"resp_bytes":5,"ms":3}` + "\n\n" +
			`{"ts":"2026-09-28T10:00:01Z","kind":"dropped","req_bytes":0,"resp_bytes":0,"ms":0,"dropped":4}` + "\n"))
	})
	mux.HandleFunc("GET /machines/roto/credaudit", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"invalid tail"}`))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	since := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	got, err := c.CredAudit(ctx, "svc", CredAuditQuery{Tail: 50, Denied: true, Since: since})
	if err != nil {
		t.Fatal(err)
	}
	if query != "denied=1&since=2026-09-28T09%3A00%3A00Z&tail=50" {
		t.Fatalf("query %q", query)
	}
	if len(got) != 2 || got[0].Creds[0] != "KEY" || got[0].RespBytes != 5 || got[1].Kind != "dropped" || got[1].Dropped != 4 {
		t.Fatalf("registros: %+v", got)
	}
	if _, err := c.CredAudit(ctx, "roto", CredAuditQuery{}); err == nil || err.Error() != "invalid tail" {
		t.Fatalf("error del daemon: %v", err)
	}
	if _, err := c.CredAudit(ctx, "viejo", CredAuditQuery{}); err == nil || !strings.Contains(err.Error(), "update it") {
		t.Fatalf("daemon sin la ruta: %v", err)
	}
}
