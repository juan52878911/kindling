package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// Un gateway que redirige a otro origen no se lleva el token consigo.
func TestClienteNoReenviaElTokenTrasUnaRedireccion(t *testing.T) {
	var conToken atomic.Int32
	otro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			conToken.Add(1)
		}
	}))
	t.Cleanup(otro.Close)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otro.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(gw.Close)
	t.Setenv("KLING_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	t.Setenv("KLING_SANDBOX_URL", gw.URL)
	t.Setenv("KLING_SANDBOX_TOKEN", "secreto")

	c, err := nuevoCliente()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.llamar(context.Background(), http.MethodGet, "/v1/sandboxes", nil, nil); err == nil {
		t.Fatal("the redirect to another origin was followed")
	}
	if n := conToken.Load(); n != 0 {
		t.Fatalf("the token reached the other origin %d time(s)", n)
	}
}
