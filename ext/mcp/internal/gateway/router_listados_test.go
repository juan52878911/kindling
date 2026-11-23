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
	"sync/atomic"
	"testing"
	"time"

	"github.com/juan52878911/kindling/ext/mcp/internal/mcp"
	"github.com/juan52878911/kindling/pkg/api"
)

// LO QUE CUESTA CADA PETICIÓN DEL ROUTER MULTI-HOST.
//
// Cada call_tool por /mcp/_all y cada POST sin sesión a /mcp/<servicio>
// preguntaban a TODOS los daemons qué servicios tienen, uno detrás de otro y
// sin recordarlo: un host lento sumaba su retraso a cada petición.

// contadorDaemon cuenta lo que un daemon falso del router recibe.
type contadorDaemon struct {
	listados  atomic.Int64 // GET /snapshots
	procstats atomic.Int64 // GET /procstats
	maquinas  atomic.Int64 // POST /machines
}

// daemonRouterContado levanta un daemon falso con los enlaces dados que tarda
// retraso en contestar a /snapshots.
func daemonRouterContado(t testing.TB, links []*mcp.Link, retraso time.Duration) (string, *contadorDaemon) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kmcp-rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	c := &contadorDaemon{}
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots", func(w http.ResponseWriter, r *http.Request) {
		c.listados.Add(1)
		time.Sleep(retraso)
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/store/mcp/links", func(w http.ResponseWriter, r *http.Request) {
		m := map[string]*mcp.Link{}
		for _, l := range links {
			m[l.Name] = l
		}
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Info{Capabilities: []string{"annotations", "store"}})
	})
	mux.HandleFunc("/machines", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			c.maquinas.Add(1)
			http.Error(w, `{"message":"no machines in this fake"}`, http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/procstats", func(w http.ResponseWriter, r *http.Request) {
		c.procstats.Add(1)
		_ = json.NewEncoder(w).Encode(api.ProcStats{AvailableMiB: 1024})
	})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, c
}

// servicioEnlazadoBench es un servidor MCP externo mínimo.
func servicioEnlazadoBench(t testing.TB, name string) *mcp.Link {
	srv := newServidorEco(t, name)
	return &mcp.Link{
		Name: name, URL: srv + "/mcp",
		Labels: map[string]string{api.LabelService: name},
		Tools:  []mcp.ToolSpec{{Name: "hace", Description: "hace algo"}},
	}
}

func newServidorEco(t testing.TB, name string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(SessionHeader, name+"-sess")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// routerTresHosts monta un router con tres hosts: "uno" y "dos" sirven alfa y
// beta; "tres" no sirve nada y su daemon tarda retraso en listar.
func routerTresHosts(t testing.TB, retraso time.Duration) (http.Handler, []*contadorDaemon) {
	alfa := servicioEnlazadoBench(t, "alfa")
	beta := servicioEnlazadoBench(t, "beta")
	s1, c1 := daemonRouterContado(t, []*mcp.Link{alfa}, 0)
	s2, c2 := daemonRouterContado(t, []*mcp.Link{alfa, beta}, 0)
	s3, c3 := daemonRouterContado(t, nil, retraso)
	r := NewRouter([]RouterHost{
		{Name: "uno", GW: New(api.NewClient(s1), 5*time.Minute, false, 0, "")},
		{Name: "dos", GW: New(api.NewClient(s2), 5*time.Minute, false, 0, "")},
		{Name: "tres", GW: New(api.NewClient(s3), 5*time.Minute, false, 0, "")},
	})
	return r.Handler(""), []*contadorDaemon{c1, c2, c3}
}

func sumar(cs []*contadorDaemon, f func(*contadorDaemon) int64) int64 {
	var n int64
	for _, c := range cs {
		n += f(c)
	}
	return n
}

func callToolRouter(name string) string {
	return fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"call_tool","arguments":{"name":%q,"arguments":{}}}}`, name)
}

// BenchmarkRouterCallTool mide call_tool por /mcp/_all del router con tres
// hosts, uno de ellos lento (2 ms por listado).
func BenchmarkRouterCallTool(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	h, cs := routerTresHosts(b, 2*time.Millisecond)
	sid := abrirAgregado(b, h)
	cuerpo := callToolRouter("beta.hace")
	if rec := pedir(b, h, "POST", "/mcp/_all", sid, cuerpo); rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok") {
		b.Fatalf("call_tool: %d %s", rec.Code, rec.Body)
	}
	antes := sumar(cs, func(c *contadorDaemon) int64 { return c.listados.Load() })
	b.ResetTimer()
	for range b.N {
		_ = pedir(b, h, "POST", "/mcp/_all", sid, cuerpo)
	}
	b.StopTimer()
	n := sumar(cs, func(c *contadorDaemon) int64 { return c.listados.Load() }) - antes
	b.ReportMetric(float64(n)/float64(b.N), "listados/op")
}

// BenchmarkRouterProxySinSesion mide un POST sin sesión a /mcp/alfa, que está
// en dos de los tres hosts (el tercero, lento).
func BenchmarkRouterProxySinSesion(b *testing.B) {
	log.SetOutput(io.Discard)
	b.Cleanup(func() { log.SetOutput(os.Stderr) })
	h, cs := routerTresHosts(b, 2*time.Millisecond)
	_ = pedir(b, h, "POST", "/mcp/alfa", "", cuerpoInit)
	l0 := sumar(cs, func(c *contadorDaemon) int64 { return c.listados.Load() })
	p0 := sumar(cs, func(c *contadorDaemon) int64 { return c.procstats.Load() })
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = pedir(b, h, "POST", "/mcp/alfa", "", cuerpoInit)
	}
	b.StopTimer()
	l := sumar(cs, func(c *contadorDaemon) int64 { return c.listados.Load() }) - l0
	p := sumar(cs, func(c *contadorDaemon) int64 { return c.procstats.Load() }) - p0
	b.ReportMetric(float64(l)/float64(b.N), "listados/op")
	b.ReportMetric(float64(p)/float64(b.N), "procstats/op")
}

// BenchmarkGatewayRoutes mide construir el mux de un host, que el router hacía
// en cada petición que le delega.
func BenchmarkGatewayRoutes(b *testing.B) {
	gw := New(api.NewClient(filepath.Join("/tmp", "no-existe.sock")), 5*time.Minute, false, 0, "")
	b.ReportAllocs()
	for range b.N {
		_ = gw.routes()
	}
}
