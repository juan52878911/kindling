package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// clienteDePrueba sirve mux en un socket unix corto (sun_path en macOS) y
// devuelve un cliente que le habla.
func clienteDePrueba(t *testing.T, mux http.Handler) *Client {
	t.Helper()
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
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return NewClient(sock)
}

// Un evento de más de 64 KiB (el búfer por defecto de bufio.Scanner) no corta
// el stream.
func TestEventsAguantaLineasLargas(t *testing.T) {
	grande := strings.Repeat("x", 200<<10)
	mux := http.NewServeMux()
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		_ = enc.Encode(Event{Type: "a", Message: grande})
		_ = enc.Encode(Event{Type: "b"})
	})
	c := clienteDePrueba(t, mux)
	var tipos []string
	if err := c.Events(context.Background(), func(ev Event) { tipos = append(tipos, ev.Type) }); err != nil {
		t.Fatalf("Events: %v", err)
	}
	if strings.Join(tipos, ",") != "a,b" {
		t.Fatalf("eventos = %v; quería a,b", tipos)
	}
}

type transporteRoto struct{}

func (transporteRoto) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("por el cliente corto")
}

// Freeze, Thaw y Start pueden pasar del minuto de ResponseHeaderTimeout del
// cliente normal: van por el largo, como Commit.
func TestFreezeThawStartVanPorElClienteLargo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /machines/m/{op}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"m"}`))
	})
	c := clienteDePrueba(t, mux)
	c.http = &http.Client{Transport: transporteRoto{}}
	ctx := context.Background()
	if _, err := c.Freeze(ctx, "m"); err != nil {
		t.Errorf("Freeze: %v", err)
	}
	if _, err := c.Thaw(ctx, "m"); err != nil {
		t.Errorf("Thaw: %v", err)
	}
	if _, err := c.Start(ctx, "m", nil); err != nil {
		t.Errorf("Start: %v", err)
	}
}
