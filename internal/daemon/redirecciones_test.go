package daemon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// invitadoQueRedirige monta "B", que cuenta lo que le llega, y "A", un agente
// hostil que contesta a todo con codigo y Location hacia B (otra máquina).
func invitadoQueRedirige(t *testing.T, codigo int) (a *httptest.Server, recibidas *atomic.Int64) {
	t.Helper()
	recibidas = new(atomic.Int64)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recibidas.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(b.Close)
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+r.URL.Path, codigo)
	}))
	t.Cleanup(a.Close)
	return a, recibidas
}

// guestClient (exec, files, shell, proxy de herramientas) devuelve el 3xx del
// invitado tal cual: la petición no se repite en otra máquina.
func TestGuestClientNoSigueRedirecciones(t *testing.T) {
	for _, codigo := range []int{http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		a, recibidas := invitadoQueRedirige(t, codigo)
		req, _ := http.NewRequest(http.MethodPost, a.URL+"/exec", strings.NewReader(`{"cmd":["id"]}`))
		resp, err := guestClient.Do(req)
		if err != nil {
			t.Fatalf("%d: %v", codigo, err)
		}
		resp.Body.Close()
		if resp.StatusCode != codigo {
			t.Errorf("%d: llegó %d", codigo, resp.StatusCode)
		}
		if n := recibidas.Load(); n != 0 {
			t.Errorf("%d: la otra máquina recibió %d peticiones", codigo, n)
		}
	}
}

// El agente de una VM de construcción tampoco puede mandar el exec, los ficheros
// o la espera a /healthz a otra parte.
func TestInvitadoHTTPNoSigueRedirecciones(t *testing.T) {
	a, recibidas := invitadoQueRedirige(t, http.StatusTemporaryRedirect)
	g := nuevoInvitadoHTTP(a.URL)
	ctx := context.Background()
	if _, _, err := g.Exec(ctx, []string{"true"}); err == nil {
		t.Error("Exec dio por bueno un 307")
	}
	if err := g.Put(ctx, "/tmp/x", 0o644, strings.NewReader("x")); err == nil {
		t.Error("Put dio por bueno un 307")
	}
	if rc, err := g.Get(ctx, "/etc/passwd"); err == nil {
		rc.Close()
		t.Error("Get dio por bueno un 307")
	}
	_ = esperarInvitado(ctx, a.URL, time.Second)
	if n := recibidas.Load(); n != 0 {
		t.Fatalf("la otra máquina recibió %d peticiones", n)
	}
}
