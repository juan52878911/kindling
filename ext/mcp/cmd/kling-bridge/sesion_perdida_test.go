package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Una sesión que el puente ya no tiene (segada, reinicio del puente) es un 404,
// como pide Streamable HTTP para que el cliente rehaga el initialize; antes era
// un 400, que solo el gateway sabía interpretar mirando el texto.
func TestSesionPerdidaDa404(t *testing.T) {
	b := &bridge{argv: []string{"cat"}, idle: time.Minute, maxSessions: 8,
		sessions: map[string]*session{}}
	req := httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	req.Header.Set(SessionHeader, "no-existe")
	rec := httptest.NewRecorder()
	b.handlePost(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("sesión perdida: %d %s; quería 404", rec.Code, rec.Body)
	}

	// Sin cabecera sigue siendo un error del cliente, no una sesión perdida.
	req = httptest.NewRequest(http.MethodPost, "/",
		strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	rec = httptest.NewRecorder()
	b.handlePost(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("sin cabecera: %d; quería 400", rec.Code)
	}
}
