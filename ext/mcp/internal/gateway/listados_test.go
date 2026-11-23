package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
)

// CUÁNTAS VECES SE LISTAN LOS SNAPSHOTS EN EL PRIMER initialize DE /mcp/_all.
//
// Cada listado es un GET /snapshots al daemon que trae TODOS los snapshots con
// su catálogo de herramientas dentro: con N servicios, pedirlo una vez por
// servicio es N veces el mismo JSON decodificado.

// daemonContado es un daemon falso con n servicios importados con catálogo
// (10 herramientas con esquema cada uno) que cuenta los GET /snapshots.
func daemonContado(t testing.TB, n int) (sock string, listados *atomic.Int64) {
	sock, listados, _ = daemonContadoFallable(t, n)
	return sock, listados
}

// daemonContadoFallable es daemonContado con un interruptor: con fallar a
// true, GET /snapshots contesta 503, como un daemon que no responde.
func daemonContadoFallable(t testing.TB, n int) (sock string, listados *atomic.Int64, fallar *atomic.Bool) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kmcp-lst")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock = filepath.Join(dir, "d.sock")

	var snaps []*api.Snapshot
	for i := range n {
		svc := fmt.Sprintf("svc%02d", i)
		var tools []mcp.ToolSpec
		for j := range 10 {
			tools = append(tools, mcp.ToolSpec{
				Name:        fmt.Sprintf("tool%d", j),
				Description: "Una herramienta de prueba con una descripción de longitud normal",
				InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"n":{"type":"integer"}},"required":["path"]}`),
			})
		}
		snaps = append(snaps, mcp.WithTools(&api.Snapshot{
			Name: svc, Labels: map[string]string{api.LabelService: svc},
		}, tools))
	}
	cuerpo, _ := json.Marshal(snaps)

	listados, fallar = &atomic.Int64{}, &atomic.Bool{}
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots", func(w http.ResponseWriter, r *http.Request) {
		listados.Add(1)
		if fallar.Load() {
			http.Error(w, "daemon falso caído", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(cuerpo)
	})
	mux.HandleFunc("/store/mcp/links", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Info{Capabilities: []string{"annotations", "store"}})
	})
	mux.HandleFunc("/machines", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, listados, fallar
}

// BenchmarkPrimerInitializeAgregado mide el primer initialize de /mcp/_all
// (catálogo vacío) con 5, 20 y 50 servicios; listados/op son los GET
// /snapshots que hace al daemon.
func BenchmarkPrimerInitializeAgregado(b *testing.B) {
	// Sin el registro de cada petición, que se mezclaría con la salida.
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	for _, n := range []int{5, 20, 50} {
		b.Run(fmt.Sprintf("servicios=%d", n), func(b *testing.B) {
			sock, listados := daemonContado(b, n)
			c := api.NewClient(sock)
			listados.Store(0)
			b.ResetTimer()
			for range b.N {
				gw := New(c, 5*time.Minute, false, 0, "")
				if rec := pedir(b, gw.Handler(""), "POST", "/mcp/_all", "", cuerpoInit); rec.Code != 200 {
					b.Fatalf("initialize: %d %s", rec.Code, rec.Body)
				}
			}
			b.ReportMetric(float64(listados.Load())/float64(b.N), "listados/op")
		})
	}
}

// BenchmarkToolsOfCaducadoConcurrente mide 8 call_tool simultáneos al mismo
// servicio justo cuando su entrada del catálogo ha caducado: listados/op son
// los GET /snapshots que hacen entre todos para refrescarla.
func BenchmarkToolsOfCaducadoConcurrente(b *testing.B) {
	sock, listados := daemonContado(b, 20)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	cat := gw.agg.cat
	ctx := b.Context()
	listados.Store(0)
	b.ResetTimer()
	for range b.N {
		cat.mu.Lock()
		cat.fetched = map[string]time.Time{} // todo caducado
		cat.mu.Unlock()
		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := cat.toolsOf(ctx, "svc07"); err != nil {
					b.Error(err)
				}
			}()
		}
		wg.Wait()
	}
	b.ReportMetric(float64(listados.Load())/float64(b.N), "listados/op")
}

// El primer initialize de /mcp/_all con 20 servicios lista los snapshots UNA
// vez, no una por servicio.
func TestPrimerInitializeAgregadoListaUnaVez(t *testing.T) {
	sock, listados := daemonContado(t, 20)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	rec := pedir(t, gw.Handler(""), "POST", "/mcp/_all", "", cuerpoInit)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "svc19: tool0") {
		t.Fatalf("initialize: %d %s", rec.Code, rec.Body)
	}
	if n := listados.Load(); n != 1 {
		t.Fatalf("el initialize listó los snapshots %d veces; quería 1", n)
	}
	// Otra petición vuelve a listar: el recuerdo es de la petición, no una
	// caché que esconda un servicio recién importado.
	listados.Store(0)
	_ = pedir(t, gw.Handler(""), "POST", "/mcp/_all", "", cuerpoInit)
	if n := listados.Load(); n > 1 {
		t.Fatalf("el segundo initialize listó %d veces; quería como mucho 1", n)
	}
}

// Ocho refrescos simultáneos de una entrada caducada hacen un solo listado y
// todos reciben las herramientas.
func TestToolsOfCaducadoUnSoloRefresco(t *testing.T) {
	sock, listados := daemonContado(t, 5)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	cat := gw.agg.cat
	if _, err := cat.toolsOf(t.Context(), "svc01"); err != nil {
		t.Fatal(err)
	}
	cat.mu.Lock()
	cat.fetched["svc01"] = time.Time{}
	cat.mu.Unlock()
	listados.Store(0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ts, err := cat.toolsOf(t.Context(), "svc01"); err != nil || len(ts) != 10 {
				t.Errorf("toolsOf: %d herramientas, %v", len(ts), err)
			}
		}()
	}
	wg.Wait()
	if n := listados.Load(); n != 1 {
		t.Fatalf("8 refrescos simultáneos listaron %d veces; quería 1", n)
	}
}

// Con la entrada caducada y el daemon sin contestar, se sirve la copia vieja
// en vez de fallar; sin copia vieja, el error llega.
func TestToolsOfCaducadoSirveLoViejoSiElDaemonFalla(t *testing.T) {
	sock, _, fallar := daemonContadoFallable(t, 3)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	cat := gw.agg.cat
	if _, err := cat.toolsOf(t.Context(), "svc00"); err != nil {
		t.Fatal(err)
	}
	cat.mu.Lock()
	cat.fetched["svc00"] = time.Time{}
	cat.mu.Unlock()
	fallar.Store(true)
	ts, err := cat.toolsOf(t.Context(), "svc00")
	if err != nil || len(ts) != 10 {
		t.Fatalf("con el daemon caído: %d herramientas, %v; quería la copia vieja", len(ts), err)
	}
	if _, err := cat.toolsOf(t.Context(), "svc01"); err == nil {
		t.Fatal("svc01 nunca se leyó y el daemon está caído: quería un error")
	}
}
