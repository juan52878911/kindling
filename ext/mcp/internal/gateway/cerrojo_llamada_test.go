package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// Un initialize lento de una conversación no para las llamadas de otra que ya
// tiene su sesión con el mismo servicio. Con un cerrojo por servicio, la de B
// esperaba a que terminase el initialize de A.
func TestInitializeLentoNoParaOtrasConversaciones(t *testing.T) {
	v := &invitadoRepetidor{nombre: "A"}
	var retener atomic.Bool
	suelta := make(chan struct{})
	inv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if retener.Load() && bytes.Contains(body, []byte(`"initialize"`)) {
			<-suelta
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		v.ServeHTTP(w, r)
	}))
	t.Cleanup(inv.Close)
	t.Cleanup(func() {
		if retener.Load() {
			close(suelta)
		}
	})
	d := &daemonAislado{service: "notas", invitados: []string{strings.TrimPrefix(inv.URL, "http://")}, compartido: true}
	h := New(api.NewClient(levantarDaemonAislado(t, d)), 5*time.Minute, false, 0, "").Handler("")

	conversar := func() string {
		t.Helper()
		rec := pedir(t, h, "POST", "/mcp/_all?mode=expand", "", cuerpoInit)
		sid := rec.Header().Get(SessionHeader)
		if rec.Code != 200 || sid == "" {
			t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
		}
		return sid
	}
	llamada := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"notas.echo","arguments":{}}}`

	b := conversar()
	if rec := pedir(t, h, "POST", "/mcp/_all", b, llamada); rec.Code != 200 || !strings.Contains(rec.Body.String(), "soy") {
		t.Fatalf("primera llamada de B: %d %s", rec.Code, rec.Body)
	}
	a := conversar()
	retener.Store(true)
	go pedir(t, h, "POST", "/mcp/_all", a, llamada) // se queda en el initialize de A
	time.Sleep(100 * time.Millisecond)

	inicio := time.Now()
	rec := pedir(t, h, "POST", "/mcp/_all", b, llamada)
	dur := time.Since(inicio)
	t.Logf("llamada de B con el initialize de A retenido: %s", dur)
	if rec.Code != 200 || dur > 500*time.Millisecond {
		t.Fatalf("B tardó %s (%d): esperó al initialize de A", dur, rec.Code)
	}
}
