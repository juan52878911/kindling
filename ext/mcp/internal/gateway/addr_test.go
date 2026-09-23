package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// EL BACKEND NATIVO DE macOS (kling-vz).
//
// Allí todos los invitados comparten la misma IP interna (172.16.0.2, ver
// docs/backend-vz.md §3 en el núcleo) y esa IP no es alcanzable desde el host:
// se llega a cada uno por el puerto que su ayudante reenvía en 127.0.0.1
// (api.Machine.Forwards, resuelto con Machine.Addr). Un daemon falso que
// devuelve una máquina con Forwards, sin nada escuchando en su IP, reproduce
// exactamente esa situación: si el gateway todavía construyera la dirección a
// mano con IP+puerto, este test se quedaría sin respuesta (o fallaría al
// conectar a una IP que no existe en la red del test) en vez de encontrar el
// servidor MCP falso en el reenvío.

// guestFalso simula el servidor MCP dentro del invitado: basta con handshake +
// tools/list, que es todo lo que toca fetch().
func guestFalso(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := api.LeerCuerpo(r.Body, 1<<20)
		_ = json.Unmarshal(body, &rpc)
		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			w.Header().Set(SessionHeader, "sesion-falsa")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"serverInfo":{"name":"falso"}}}`, string(rpc.ID))
		case "tools/list":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[`+
				`{"name":"echo","description":"Repite lo que se le manda"}]}}`, string(rpc.ID))
		case "tools/call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"ok"}]}}`,
				string(rpc.ID))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
}

// daemonConReenvios levanta un daemon falso sobre un socket unix que simula lo
// justo del backend macOS: un snapshot sin catálogo capturado (para forzar el
// camino de fetch(), que es el que construye la dirección a mano) y una única
// máquina con Forwards apuntando al servidor MCP falso — nunca a su IP, que en
// este test es "172.16.0.2", una dirección que nada escucha.
func daemonConReenvios(t *testing.T, service string, guestAddr string) string {
	t.Helper()
	// /tmp y no t.TempDir(): en macOS este último pasa fácilmente de los 104
	// bytes de sun_path (mismo motivo que en pkg/scheduler/esperar_test.go del
	// núcleo).
	dir, err := os.MkdirTemp("/tmp", "kmcp-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")

	const machineID = "m1"
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshots", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]*api.Snapshot{{
			Name:   service,
			Labels: map[string]string{api.LabelService: service},
			Egress: "none",
		}})
	})
	mux.HandleFunc("/store/mcp/links", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Info{})
	})
	mux.HandleFunc("/machines", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			// runFresh: instancia del snapshot dorado. Se devuelve SIEMPRE la
			// misma máquina, con Forwards y una IP de invitado que no existe en
			// la red del test (172.16.0.2, la que usa kling-vz de verdad).
			_ = json.NewEncoder(w).Encode(&api.Machine{
				ID: machineID, Name: service, State: api.StateRunning,
				IP:       "172.16.0.2",
				Forwards: map[string]string{fmt.Sprint(GuestPort): guestAddr},
			})
			return
		}
		// GET: acquire() mira antes si ya hay alguna en marcha.
		_ = json.NewEncoder(w).Encode([]*api.Machine{})
	})
	mux.HandleFunc("/machines/"+machineID+"/guest", func(w http.ResponseWriter, r *http.Request) {
		// esperarListo (pkg/scheduler) pregunta aquí con probe_only cuando la
		// máquina trae Forwards, en vez de sondear la dirección directamente
		// (ver docs/backend-vz.md §3: el puerto de loopback siempre acepta).
		// Contestar 200 basta para decir "el invitado ya escucha".
		_ = json.NewEncoder(w).Encode(api.GuestResponse{Status: 200})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "daemon falso: ruta no implementada "+r.URL.Path, http.StatusNotFound)
	})

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("sin sockets unix: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// TestFetchUsaElReenvioNoLaIP reproduce el camino de catalog.fetch(): sin
// catálogo capturado en el snapshot, tiene que despertar la instancia (que
// llega con Forwards, como en macOS) y hablar con su servidor MCP para pedirle
// tools/list. Antes del cambio a Addr, esto construía "http://172.16.0.2:8080"
// a mano: una dirección que nada escucha en la red del test, así que el
// fetch fallaría en vez de encontrar el catálogo.
func TestFetchUsaElReenvioNoLaIP(t *testing.T) {
	guest := guestFalso(t)
	defer guest.Close()
	guestAddr := strings.TrimPrefix(guest.URL, "http://")

	sock := daemonConReenvios(t, "eco", guestAddr)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tools, err := gw.agg.cat.toolsOf(ctx, "eco")
	if err != nil {
		t.Fatalf("toolsOf: %v (con la IP a mano habría fallado por no poder conectar)", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v, quería [echo]", tools)
	}
}

// TestForwardDeAggregatorUsaElReenvio es el mismo contrato, pero por el camino
// que usan de verdad los clientes MCP: tools/call a través del agregador
// (aggregate.go forward), que también construía la dirección con IP+puerto.
func TestForwardDeAggregatorUsaElReenvio(t *testing.T) {
	guest := guestFalso(t)
	defer guest.Close()
	guestAddr := strings.TrimPrefix(guest.URL, "http://")

	sock := daemonConReenvios(t, "eco", guestAddr)
	gw := New(api.NewClient(sock), 5*time.Minute, false, 0, "")
	a := gw.agg

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s := &aggSession{id: "t", services: []string{"eco"}, mode: modeProxy, backing: map[string]string{}}
	if _, fault := a.forward(ctx, s, "eco.echo", nil); fault != nil {
		t.Fatalf("forward: %+v (con la IP a mano habría fallado por no poder conectar)", fault)
	}
}
