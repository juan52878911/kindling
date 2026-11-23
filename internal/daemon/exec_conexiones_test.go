package daemon

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
	"github.com/juan52878911/kindling/pkg/guest"
)

// agenteContado sirve el /exec/stream del agente de verdad y cuenta las
// conexiones nuevas que acepta.
func agenteContado(tb testing.TB) (*httptest.Server, *atomic.Int64) {
	tb.Helper()
	var nuevas atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/exec/stream", guest.StreamExecHandler(nil))
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			nuevas.Add(1)
		}
	}
	srv.Start()
	tb.Cleanup(srv.Close)
	return srv, &nuevas
}

// execContraAgente hace un exec como handleExec con ?wait=1: abre el flujo,
// lo junta y cierra el cuerpo. Devuelve si la conexión al agente era reutilizada.
func execContraAgente(tb testing.TB, base string) bool {
	tb.Helper()
	var reusada bool
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(i httptrace.GotConnInfo) { reusada = i.Reused },
	})
	body, code, err := openGuestExec(ctx, base, api.ExecRequest{Cmd: []string{"sh", "-c", "printf hola"}})
	if err != nil {
		tb.Fatalf("%d %v", code, err)
	}
	res, err := aggregateExec(readExecEvents(body, 1<<20))
	body.Close()
	if err != nil || string(res.Stdout) != "hola" {
		tb.Fatalf("%+v %v", res, err)
	}
	return reusada
}

// BenchmarkExecConexionesAlAgente mide cuántas conexiones nuevas al agente
// cuesta cada exec (G-06): net/http solo devuelve una conexión al pool si el
// cuerpo llegó a EOF.
func BenchmarkExecConexionesAlAgente(b *testing.B) {
	srv, nuevas := agenteContado(b)
	execContraAgente(b, srv.URL) // calienta
	nuevas.Store(0)
	reusadas := 0
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if execContraAgente(b, srv.URL) {
			reusadas++
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(nuevas.Load())/float64(b.N), "conns/op")
	b.ReportMetric(float64(reusadas)/float64(b.N), "reused/op")
}

// agenteConTerminadorTardio contesta como pkg/guest (eventos con Flush y el
// terminador del chunked al volver el manejador), pero deja pasar un momento
// entre el evento final y el terminador, como pasa cuando las dos escrituras
// cruzan la red de la microVM en segmentos separados.
func agenteConTerminadorTardio(tb testing.TB) (*httptest.Server, *atomic.Int64) {
	tb.Helper()
	var nuevas atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, `{"stream":"stdout","data":"aG9sYQ=="}`+"\n")
		fl.Flush()
		_, _ = io.WriteString(w, `{"exit":0,"duration_ms":1}`+"\n")
		fl.Flush()
		time.Sleep(2 * time.Millisecond)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			nuevas.Add(1)
		}
	}
	srv.Start()
	tb.Cleanup(srv.Close)
	return srv, &nuevas
}

func BenchmarkExecConexionesTerminadorTardio(b *testing.B) {
	srv, nuevas := agenteConTerminadorTardio(b)
	execContraAgente(b, srv.URL) // calienta
	time.Sleep(10 * time.Millisecond)
	nuevas.Store(0)
	reusadas := 0
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if execContraAgente(b, srv.URL) {
			reusadas++
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(nuevas.Load())/float64(b.N), "conns/op")
	b.ReportMetric(float64(reusadas)/float64(b.N), "reused/op")
}
