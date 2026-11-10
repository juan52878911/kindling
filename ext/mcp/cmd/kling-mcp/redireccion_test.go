package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// directPost no sigue redirecciones: un 307 repetía el POST, con su cuerpo y
// su Mcp-Session-Id, en el host que dijera el servidor.
func TestDirectPostNoSigueRedirecciones(t *testing.T) {
	var vistas atomic.Int32
	otro := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vistas.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	defer otro.Close()
	redirige := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, otro.URL+"/mcp", http.StatusTemporaryRedirect)
	}))
	defer redirige.Close()

	_, _, err := directPost(context.Background(), redirige.URL+"/mcp")("sesion-1", `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	if err == nil || !strings.Contains(err.Error(), "redirects") {
		t.Fatalf("POST con 307: err = %v; quería el rechazo de la redirección", err)
	}
	if n := vistas.Load(); n != 0 {
		t.Fatalf("el destino de la redirección recibió %d peticiones", n)
	}
}
