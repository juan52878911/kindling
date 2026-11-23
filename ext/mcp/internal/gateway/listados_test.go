package gateway

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

	listados = &atomic.Int64{}
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots", func(w http.ResponseWriter, r *http.Request) {
		listados.Add(1)
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
	return sock, listados
}

// BenchmarkPrimerInitializeAgregado mide el primer initialize de /mcp/_all
// (catálogo vacío) con 5, 20 y 50 servicios; listados/op son los GET
// /snapshots que hace al daemon.
func BenchmarkPrimerInitializeAgregado(b *testing.B) {
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
