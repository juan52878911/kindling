package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// El puente de un invitado que contesta con un 3xx hacia otra máquina: el
// gateway no repite allí ni el initialize ni el tools/call.
func TestPuenteNoSigueRedirecciones(t *testing.T) {
	var recibidas atomic.Int64
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recibidas.Add(1)
		w.Header().Set(SessionHeader, "s")
		w.WriteHeader(http.StatusOK)
	}))
	defer b.Close()
	for _, codigo := range []int{http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, b.URL+r.URL.Path, codigo)
		}))
		ctx := context.Background()
		if _, err := mcpInit(ctx, a.URL); err == nil {
			t.Errorf("%d: initialize dio por bueno el 3xx", codigo)
		}
		if _, err := mcpCall(ctx, a.URL, "s", `{"jsonrpc":"2.0","id":2,"method":"tools/call"}`); err == nil {
			t.Errorf("%d: tools/call dio por bueno el 3xx", codigo)
		}
		a.Close()
	}
	if n := recibidas.Load(); n != 0 {
		t.Fatalf("la otra máquina recibió %d peticiones", n)
	}
}
