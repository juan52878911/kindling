package android

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// servidorFijo sirve body en /f y cuenta las peticiones.
func servidorFijo(t *testing.T, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if r.URL.Path != "/f" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// sinRestos comprueba que en dir no queda el .part de una descarga.
func sinRestos(t *testing.T, dir string) {
	t.Helper()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("leftover %s", e.Name())
		}
	}
}

func TestFetchVerifiedHashCorrecto(t *testing.T) {
	body := []byte("contenido de prueba del artefacto")
	srv, n := servidorFijo(t, body)
	dir := t.TempDir()
	dst := filepath.Join(dir, "a.bin")
	ctx := context.Background()

	if err := fetchVerified(ctx, srv.URL+"/f", sha256Hex(body), int64(len(body)), dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("dst = %q, %v", got, err)
	}
	sinRestos(t, dir)

	// Ya está y cuadra: no se vuelve a bajar.
	if err := fetchVerified(ctx, srv.URL+"/f", sha256Hex(body), int64(len(body)), dst); err != nil {
		t.Fatal(err)
	}
	if c := n.Load(); c != 1 {
		t.Fatalf("%d requests, want 1 (the second call should reuse dst)", c)
	}

	// Sin tamaño (0) también vale, con el hash.
	dst2 := filepath.Join(dir, "b.bin")
	if err := fetchVerified(ctx, srv.URL+"/f", sha256Hex(body), 0, dst2); err != nil {
		t.Fatal(err)
	}
}

// Un hash que no cuadra se rechaza, y no deja nada en dst ni el .part.
func TestFetchVerifiedHashMalo(t *testing.T) {
	body := []byte("lo que sirve el servidor")
	srv, _ := servidorFijo(t, body)
	dir := t.TempDir()
	dst := filepath.Join(dir, "a.bin")

	otro := sha256Hex([]byte("lo que se esperaba"))
	err := fetchVerified(context.Background(), srv.URL+"/f", otro, int64(len(body)), dst)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want a sha256 mismatch", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst exists after a bad hash: %v", err)
	}
	sinRestos(t, dir)

	// Un dst viejo con otro contenido no se da por bueno: se baja y se rechaza.
	if err := os.WriteFile(dst, []byte("viejo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fetchVerified(context.Background(), srv.URL+"/f", otro, int64(len(body)), dst); err == nil {
		t.Fatal("a stale dst with another hash was accepted")
	}
	if b, _ := os.ReadFile(dst); string(b) != "viejo" {
		t.Fatalf("a rejected download replaced dst: %q", b)
	}
}

// El tamaño es un tope: un servidor que manda de más (o de menos) se rechaza
// aunque el hash fuera el esperado de lo servido, y no se lee más allá del
// tope + 1.
func TestFetchVerifiedTope(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 4096)
	srv, _ := servidorFijo(t, body)
	dir := t.TempDir()
	ctx := context.Background()

	dst := filepath.Join(dir, "grande.bin")
	err := fetchVerified(ctx, srv.URL+"/f", sha256Hex(body[:100]), 100, dst)
	if err == nil || !strings.Contains(err.Error(), "101 bytes, expected 100") {
		t.Fatalf("err = %v, want the read cut at size+1", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("dst exists after an oversized body: %v", err)
	}

	dst = filepath.Join(dir, "corto.bin")
	err = fetchVerified(ctx, srv.URL+"/f", sha256Hex(body), int64(len(body))+1, dst)
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("err = %v, want a short-body error", err)
	}
	sinRestos(t, dir)
}

func TestFetchVerifiedNo200(t *testing.T) {
	srv, _ := servidorFijo(t, nil)
	dst := filepath.Join(t.TempDir(), "a.bin")
	err := fetchVerified(context.Background(), srv.URL+"/no-esta", sha256Hex(nil), 0, dst)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the 404", err)
	}
}
