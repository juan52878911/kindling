package guesthttp

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// invitados monta "B", que cuenta lo que le llega, y "A", un invitado hostil
// que contesta a todo con codigo y Location hacia B.
func invitados(t *testing.T, codigo int) (a *httptest.Server, recibidas *atomic.Int64) {
	t.Helper()
	recibidas = new(atomic.Int64)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recibidas.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(b.Close)
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/volume/release", codigo)
	}))
	t.Cleanup(a.Close)
	return a, recibidas
}

// Un 3xx del invitado vuelve a quien llama tal cual: nadie repite la petición
// en el host del Location.
func TestNoSigueRedirecciones(t *testing.T) {
	for _, codigo := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		a, recibidas := invitados(t, codigo)
		c := New(nil, 0)
		req, _ := http.NewRequest(http.MethodPost, a.URL+"/exec", strings.NewReader(`{"cmd":["true"]}`))
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("%d: %v", codigo, err)
		}
		resp.Body.Close()
		if resp.StatusCode != codigo {
			t.Errorf("%d: llegó %d", codigo, resp.StatusCode)
		}
		if n := recibidas.Load(); n != 0 {
			t.Errorf("%d: B recibió %d peticiones", codigo, n)
		}
	}
}

// Aunque alguien le quite el CheckRedirect al cliente, el transporte no deja
// salir la petición hacia otro host.
func TestGuardRechazaOtroHost(t *testing.T) {
	for _, codigo := range []int{http.StatusSeeOther, http.StatusTemporaryRedirect} {
		a, recibidas := invitados(t, codigo)
		c := New(nil, 0)
		c.CheckRedirect = nil // el de por defecto: seguir
		req, _ := http.NewRequest(http.MethodPost, a.URL+"/exec", strings.NewReader("x"))
		resp, err := c.Do(req)
		if err == nil {
			resp.Body.Close()
			t.Fatalf("%d: siguió la redirección (%d)", codigo, resp.StatusCode)
		}
		if !errors.Is(err, ErrOtroHost) {
			t.Errorf("%d: error %v, quería ErrOtroHost", codigo, err)
		}
		if n := recibidas.Load(); n != 0 {
			t.Errorf("%d: B recibió %d peticiones", codigo, n)
		}
	}
}

// RoundTrip directo con una petición derivada de otra hacia un host distinto.
func TestGuardRoundTrip(t *testing.T) {
	var llamadas atomic.Int64
	base := roundTripFunc(func(*http.Request) (*http.Response, error) {
		llamadas.Add(1)
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	g := Guard(base)
	orig, _ := http.NewRequest(http.MethodGet, "http://10.0.0.2:8080/healthz", nil)
	otro, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:9999/x", nil)
	otro.Response = &http.Response{StatusCode: http.StatusTemporaryRedirect, Request: orig}
	if _, err := g.RoundTrip(otro); !errors.Is(err, ErrOtroHost) {
		t.Fatalf("host distinto: %v", err)
	}
	if llamadas.Load() != 0 {
		t.Fatal("el transporte de debajo recibió la petición")
	}
	if resp, err := g.RoundTrip(orig); err != nil || resp.StatusCode != 200 {
		t.Fatalf("la petición original: %v", err)
	}
	if Guard(g) != g {
		t.Error("envolver dos veces anida guardias")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
