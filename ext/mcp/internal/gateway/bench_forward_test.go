package gateway

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// EL CAMINO CALIENTE DE call_tool CON VARIAS LLAMADAS A LA VEZ.
//
// Un agente lanza herramientas en paralelo. Con el tope por defecto de
// net/http (2 conexiones ociosas por host), la tercera llamada concurrente al
// mismo invitado cerraba su conexión al acabar y la siguiente volvía a
// marcar: un dial TCP por llamada en vez de reutilizar.

const concurrentes = 8

// invitadoContado es un puente falso que cuenta las conexiones TCP que acepta
// y contesta a tools/call con un resultado de tam bytes.
func invitadoContado(t testing.TB, tam int) (addr string, conexiones *atomic.Int64) {
	t.Helper()
	relleno := strings.Repeat("x", tam)
	conexiones = &atomic.Int64{}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := api.LeerCuerpo(r.Body, 1<<20)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(SessionHeader, "s1")
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case strings.Contains(string(body), `"tools/list"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo"}]}}`))
		case strings.Contains(string(body), `"tools/call"`):
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":%q}]}}`, relleno)
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
		}
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conexiones.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), conexiones
}

// rafaga lanza n llamadas call_tool repartidas en `concurrentes` goroutines.
func rafaga(t testing.TB, h http.Handler, sid string, n int) {
	var wg sync.WaitGroup
	var fallos atomic.Int64
	for g := range concurrentes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := g; i < n; i += concurrentes {
				if rec := pedir(t, h, "POST", "/mcp/_all", sid, cuerpoCallTool); rec.Code != 200 ||
					bytes.Contains(rec.Body.Bytes(), []byte(`"error"`)) {
					fallos.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if f := fallos.Load(); f > 0 {
		t.Errorf("%d llamadas fallaron", f)
	}
}

func montarAgregado(t testing.TB, tam int) (http.Handler, string, *atomic.Int64) {
	addr, conexiones := invitadoContado(t, tam)
	gw := New(api.NewClient(daemonDosMaquinas(t, "eco", addr)), 5*time.Minute, false, 0, "")
	h := gw.Handler("")
	sid := abrirAgregado(t, h)
	// Calienta: catálogo, sesión del invitado y las conexiones de la primera
	// ráfaga.
	rafaga(t, h, sid, concurrentes)
	return h, sid, conexiones
}

// Con 8 llamadas concurrentes, las conexiones al invitado se reutilizan: tras
// calentar, una ráfaga larga no abre conexiones nuevas (antes, una por cada
// llamada que no cabía en las 2 ociosas).
func TestAgregadorReutilizaConexionesConcurrentes(t *testing.T) {
	h, sid, conexiones := montarAgregado(t, 1024)
	antes := conexiones.Load()
	rafaga(t, h, sid, 400)
	if nuevas := conexiones.Load() - antes; nuevas > concurrentes {
		t.Fatalf("400 llamadas con %d concurrentes abrieron %d conexiones nuevas al invitado; quería ≤ %d",
			concurrentes, nuevas, concurrentes)
	}
}

// BenchmarkAgregadorCallToolConcurrente mide call_tool por /mcp/_all con 8
// llamadas a la vez y un resultado de 32 KiB. conns/op es cuántas conexiones
// TCP nuevas al invitado cuesta cada llamada.
func BenchmarkAgregadorCallToolConcurrente(b *testing.B) {
	log.SetOutput(io.Discard) // los avisos del gateway ensucian la salida del banco
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	h, sid, conexiones := montarAgregado(b, 32<<10)
	antes := conexiones.Load()
	b.ReportAllocs()
	b.ResetTimer()
	rafaga(b, h, sid, b.N)
	b.StopTimer()
	b.ReportMetric(float64(conexiones.Load()-antes)/float64(b.N), "conns/op")
}
