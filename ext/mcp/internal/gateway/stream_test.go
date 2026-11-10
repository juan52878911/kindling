package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/scheduler"
)

// invitadoConStream es un puente falso que, además de contestar POST, mantiene
// abierto el GET de sesión (el stream SSE) hasta que el cliente lo suelte.
type invitadoConStream struct {
	invitadoRepetidor
	abierto chan struct{}
}

func (v *invitadoConStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		v.invitadoRepetidor.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	close(v.abierto)
	<-r.Context().Done()
}

// A6: el stream SSE de una sesión no retiene cuota del tenant ni mantiene la
// microVM en vuelo. Con MaxInflight 1, una llamada con el stream abierto daba
// 429; y al retirar la instancia (lo que hacen el segador y el desalojo) el
// stream se quedaba colgado contra el invitado en vez de acabar.
func TestStreamSSENoRetieneCuotaYSeCortaAlRetirar(t *testing.T) {
	inv := &invitadoConStream{abierto: make(chan struct{})}
	srvInv := httptest.NewServer(inv)
	defer srvInv.Close()
	sock := daemonDosMaquinas(t, "eco", strings.TrimPrefix(srvInv.URL, "http://"))
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	gw.SetTenants([]scheduler.TenantLimit{{Name: "ana", Token: "tok-ana", MaxInflight: 1}})
	srv := httptest.NewServer(gw.Handler(""))
	defer srv.Close()

	pedirAna := func(metodo, sid, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(metodo, srv.URL+"/mcp/eco", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok-ana")
		if sid != "" {
			req.Header.Set(SessionHeader, sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	r := pedirAna("POST", "", cuerpoInit)
	_ = r.Body.Close()
	sid := r.Header.Get(SessionHeader)
	if r.StatusCode != 200 || sid == "" {
		t.Fatalf("initialize: %d", r.StatusCode)
	}

	stream := pedirAna("GET", sid, "")
	defer func() { _ = stream.Body.Close() }()
	if stream.StatusCode != 200 {
		t.Fatalf("GET de sesión: %d", stream.StatusCode)
	}
	select {
	case <-inv.abierto:
	case <-time.After(5 * time.Second):
		t.Fatal("el stream no llegó al invitado")
	}

	// Con el stream abierto, la cuota de 1 sigue libre para trabajo de verdad.
	call := pedirAna("POST", sid, cuerpoCall)
	_ = call.Body.Close()
	if call.StatusCode != 200 {
		t.Fatalf("tools/call con el stream abierto: %d; el stream retenía la cuota", call.StatusCode)
	}

	// Retirar la instancia corta el stream: el cliente ve su fin.
	gw.DropInstance("eco", "m1")
	fin := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, stream.Body)
		fin <- err
	}()
	select {
	case <-fin:
	case <-time.After(5 * time.Second):
		t.Fatal("el stream sigue abierto tras retirar la instancia")
	}
}
