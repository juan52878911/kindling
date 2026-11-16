package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// /mcp/_all del router multi-host aplica la cuota de peticiones en vuelo del
// tenant, como el de un solo host. Antes no la tomaba: con -hosts, un tenant
// con max_inflight no tenía tope en el camino de los clientes reales.
func TestRouterAllAplicaCuotaDelTenant(t *testing.T) {
	inv := &invitadoLento{dentro: make(chan struct{}, 4), soltar: make(chan struct{})}
	srvInv := httptest.NewServer(inv)
	defer srvInv.Close()
	gw := New(api.NewClient(daemonDosMaquinas(t, "eco", strings.TrimPrefix(srvInv.URL, "http://"))),
		5*time.Minute, false, 0, "")
	gw.SetTenants([]scheduler.TenantLimit{{Name: "ana", Token: "tok-ana", MaxInflight: 1}})
	h := NewRouter([]RouterHost{{Name: "uno", GW: gw}}).Handler("")
	con := func(sid, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/mcp/_all", strings.NewReader(body))
		req.Host = "127.0.0.1"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok-ana")
		if sid != "" {
			req.Header.Set(SessionHeader, sid)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := con("", cuerpoInit)
	sid := rec.Header().Get(SessionHeader)
	if rec.Code != 200 || sid == "" {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	llamada := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"eco.echo","arguments":{}}}`
	primera := make(chan *httptest.ResponseRecorder, 1)
	go func() { primera <- con(sid, llamada) }()
	select {
	case <-inv.dentro:
	case <-time.After(10 * time.Second):
		t.Fatal("la primera llamada no llegó al invitado")
	}
	segunda := make(chan *httptest.ResponseRecorder, 1)
	go func() { segunda <- con(sid, llamada) }()
	select {
	case rec = <-segunda:
	case <-inv.dentro:
		close(inv.soltar)
		t.Fatal("la segunda llamada llegó al invitado con la cuota llena")
	case <-time.After(10 * time.Second):
		close(inv.soltar)
		t.Fatal("la segunda llamada no contestó")
	}
	close(inv.soltar)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("segunda llamada con la cuota llena: %d %s; quería 429", rec.Code, rec.Body)
	}
	if rec := <-primera; rec.Code != 200 {
		t.Fatalf("primera llamada: %d %s", rec.Code, rec.Body)
	}
}
