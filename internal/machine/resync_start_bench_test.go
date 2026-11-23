package machine

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/juan52878911/kindling/pkg/api"
)

// agenteResyncStart es un agente falso que atiende /resync y /machine/start y
// cuenta las conexiones que se le abren.
func agenteResyncStart(tb testing.TB) (*httptest.Server, *atomic.Int64) {
	tb.Helper()
	var conexiones atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch r.URL.Path {
		case api.GuestResyncPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"generation":1}`)
		case api.GuestStartPath:
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conexiones.Add(1)
		}
	}
	srv.Start()
	tb.Cleanup(srv.Close)
	return srv, &conexiones
}

// secuenciaCopiaImagen es lo que runFromUna hace con una copia de un snapshot
// por imagen recién reanudada: el resync y, después, POST /machine/start.
func secuenciaCopiaImagen(ctx context.Context, m *Manager, id string, snap *api.Snapshot, ai *arranqueImagen) error {
	if _, err := m.resyncCopia(ctx, id, snap, 0); err != nil {
		return err
	}
	return m.entregarMaquina(ctx, id, ai)
}

// BenchmarkResyncYStart mide el resync y el start de una copia por imagen
// contra un agente falso en loopback, y cuántas conexiones abre (G-05).
func BenchmarkResyncYStart(b *testing.B) {
	srv, conexiones := agenteResyncStart(b)
	u, _ := url.Parse(srv.URL)
	m := newTestManager(b)
	mc := m.addForTest("copia")
	m.mu.Lock()
	mc.Forwards = map[string]string{"8080": u.Host}
	m.mu.Unlock()
	snap := &api.Snapshot{Name: "img", ConAgente: true}
	ai := &arranqueImagen{env: map[string]string{"A": "1"}}
	b.ReportAllocs()
	for b.Loop() {
		if err := secuenciaCopiaImagen(context.Background(), m, "copia", snap, ai); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(conexiones.Load())/float64(b.N), "conns/op")
}
