package machine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// invitadoQueRedirige monta "B", que cuenta lo que le llega, y "A", un agente
// hostil que contesta a todo con codigo y Location hacia B (otra máquina).
func invitadoQueRedirige(t *testing.T, codigo int) (a *httptest.Server, recibidas *atomic.Int64) {
	t.Helper()
	recibidas = new(atomic.Int64)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recibidas.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(b.Close)
	a = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+r.URL.Path, codigo)
	}))
	t.Cleanup(a.Close)
	return a, recibidas
}

// Ningún cliente de este paquete hacia el agente repite la petición en el host
// del Location de un 3xx.
func TestClientesInvitadoNoSiguenRedirecciones(t *testing.T) {
	clientes := map[string]*http.Client{"listo": listoClient, "resync": resyncClient, "share": shareClient}
	for _, codigo := range []int{http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for nombre, c := range clientes {
			a, recibidas := invitadoQueRedirige(t, codigo)
			req, _ := http.NewRequest(http.MethodPost, a.URL+"/volume/release", strings.NewReader("{}"))
			resp, err := c.Do(req)
			if err != nil {
				t.Fatalf("%s %d: %v", nombre, codigo, err)
			}
			resp.Body.Close()
			if resp.StatusCode != codigo {
				t.Errorf("%s %d: llegó %d", nombre, codigo, resp.StatusCode)
			}
			if n := recibidas.Load(); n != 0 {
				t.Errorf("%s %d: la otra máquina recibió %d peticiones", nombre, codigo, n)
			}
		}
	}
}

// resyncOnce con un agente que redirige: error (no es un 200) y la otra
// máquina no recibe nada. Con 303: un 307 no se repetía de todos modos porque
// el cuerpo de resync no tiene GetBody, pero el 303 se sigue como GET.
func TestResyncNoSigueRedirecciones(t *testing.T) {
	a, recibidas := invitadoQueRedirige(t, http.StatusSeeOther)
	if _, err := resyncOnce(context.Background(), a.URL, "restore"); err == nil {
		t.Fatal("un 303 del agente dio el resync por bueno")
	}
	if n := recibidas.Load(); n != 0 {
		t.Fatalf("la otra máquina recibió %d peticiones", n)
	}
}
