package scheduler

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// Retirar una instancia cierra las conexiones en reposo de su proxy: su
// invitado ya no las atiende y, sin esto, quedaban abiertas hasta que el
// sistema las tirase.
func TestRetirarInstanciaCierraConexiones(t *testing.T) {
	var cerradas atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed {
			cerradas.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	for _, extra := range []bool{false, true} {
		e := &entry{machineID: "m1", proxy: proxyInvitado(u)}
		e.proxy.Transport = transporteInvitado()
		g := &Scheduler{services: map[string]*entry{}, extra: map[string][]*entry{}}
		if extra {
			g.extra["s"] = []*entry{e}
		} else {
			g.services["s"] = e
		}
		rec := httptest.NewRecorder()
		e.proxy.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
		if rec.Code != 200 {
			t.Fatalf("proxy: %d", rec.Code)
		}
		antes := cerradas.Load()
		g.removeEntryLocked("s", "m1")
		plazo := time.Now().Add(2 * time.Second)
		for cerradas.Load() == antes {
			if time.Now().After(plazo) {
				t.Fatalf("extra=%v: la conexión en reposo siguió abierta al retirar la instancia", extra)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}
