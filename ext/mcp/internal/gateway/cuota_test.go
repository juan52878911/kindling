package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// invitadoLento retiene cada tools/call hasta que se suelte: así una llamada
// sigue en vuelo mientras llega la siguiente.
type invitadoLento struct {
	invitadoRepetidor
	dentro chan struct{}
	soltar chan struct{}
}

func (v *invitadoLento) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if strings.Contains(string(body), `"tools/call"`) {
		v.dentro <- struct{}{}
		<-v.soltar
	}
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	v.invitadoRepetidor.ServeHTTP(w, r)
}

// M20: /mcp/_all aplica la cuota de peticiones en vuelo del tenant y cuenta la
// popularidad del servicio, igual que /mcp/<servicio>. Antes no hacía ninguna
// de las dos, y es el camino de los clientes reales.
func TestAgregadorAplicaCuotaYPopularidad(t *testing.T) {
	inv := &invitadoLento{dentro: make(chan struct{}, 4), soltar: make(chan struct{})}
	srvInv := httptest.NewServer(inv)
	defer srvInv.Close()
	sock := daemonDosMaquinas(t, "eco", strings.TrimPrefix(srvInv.URL, "http://"))
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	popu := t.TempDir() + "/pop.json"
	gw.SetPopularityFile(popu)
	gw.SetTenants([]scheduler.TenantLimit{{Name: "ana", Token: "tok-ana", MaxInflight: 1}})
	h := gw.Handler("")
	con := func(sid, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/mcp/_all", strings.NewReader(body))
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
		t.Fatal("la segunda llamada no contestó")
	}
	if rec.Code != http.StatusTooManyRequests {
		close(inv.soltar)
		t.Fatalf("segunda llamada con la cuota llena: %d %s; quería 429", rec.Code, rec.Body)
	}
	close(inv.soltar)
	if rec := <-primera; rec.Code != 200 {
		t.Fatalf("primera llamada: %d %s", rec.Code, rec.Body)
	}

	gw.Drain(t.Context()) // pliega la popularidad y la escribe
	b, err := os.ReadFile(popu)
	if err != nil || !strings.Contains(string(b), `"eco"`) {
		t.Fatalf("popularidad tras call_tool por _all: %q (%v); quería eco", b, err)
	}
}

// M20: un servicio enlazado (kling mcp link) también cuenta para la cuota del
// tenant: antes el desvío al enlace salía antes de TenantBegin.
func TestEnlaceAplicaCuotaDelTenant(t *testing.T) {
	dentro, soltar := make(chan struct{}, 4), make(chan struct{})
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dentro <- struct{}{}
		<-soltar
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
	}))
	defer ext.Close()
	sock, _ := mockRouterDaemon(t, nil, []*mcp.Link{{Name: "fuera", URL: ext.URL + "/mcp",
		Labels: map[string]string{api.LabelService: "fuera"}}}, 0, nil)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	gw.SetTenants([]scheduler.TenantLimit{{Name: "ana", Token: "tok-ana", MaxInflight: 1}})
	h := gw.Handler("")
	con := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/mcp/fuera", strings.NewReader(cuerpoCall))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok-ana")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	primera := make(chan *httptest.ResponseRecorder, 1)
	go func() { primera <- con() }()
	select {
	case <-dentro:
	case <-time.After(10 * time.Second):
		t.Fatal("la primera petición no llegó al enlace")
	}
	segunda := make(chan *httptest.ResponseRecorder, 1)
	go func() { segunda <- con() }()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-segunda:
	case <-dentro:
		close(soltar)
		t.Fatal("la segunda petición llegó al enlace con la cuota llena")
	case <-time.After(10 * time.Second):
		t.Fatal("la segunda petición no contestó")
	}
	close(soltar)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("segunda petición: %d %s; quería 429", rec.Code, rec.Body)
	}
	if r := <-primera; r.Code != 200 {
		t.Fatalf("primera petición: %d %s", r.Code, r.Body)
	}
}
