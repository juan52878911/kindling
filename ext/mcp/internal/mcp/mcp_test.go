package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juan52878911/kindling/pkg/api"
)

// fakeDaemon es un daemon mínimo sobre un socket unix. Con nuevo=true sirve las
// rutas de v0.5 (anotaciones, store, /info con capacidades); con nuevo=false
// solo las de v0.4 (/catalog, /health, /links), como un daemon sin actualizar.
type fakeDaemon struct {
	mu     sync.Mutex
	nuevo  bool
	annot  map[string]map[string]json.RawMessage // snapshot -> clave -> valor
	store  map[string][]byte
	links  []*Link
	legacy []string // rutas antiguas que se llamaron
	socket string
}

func newFakeDaemon(t *testing.T, nuevo bool) (*fakeDaemon, *api.Client) {
	t.Helper()
	f := &fakeDaemon{nuevo: nuevo, annot: map[string]map[string]json.RawMessage{"eco": {}}, store: map[string][]byte{}}
	f.socket = filepath.Join("/tmp", fmt.Sprintf("kmcp-%d.sock", time.Now().UnixNano()))
	mux := http.NewServeMux()
	if nuevo {
		mux.HandleFunc("GET /info", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(api.Info{Capabilities: []string{"annotations", "store"}})
		})
		mux.HandleFunc("PUT /snapshots/{name}/annotations/{key}", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			m, ok := f.annot[r.PathValue("name")]
			if !ok {
				w.WriteHeader(404)
				json.NewEncoder(w).Encode(api.Error{Message: fmt.Sprintf("snapshot %q does not exist", r.PathValue("name"))})
				return
			}
			b, _ := io.ReadAll(r.Body)
			m[r.PathValue("key")] = b
			json.NewEncoder(w).Encode(api.Snapshot{Name: r.PathValue("name"), Annotations: m})
		})
		mux.HandleFunc("GET /store/{ns}/{key}", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			b, ok := f.store[r.PathValue("ns")+"/"+r.PathValue("key")]
			if !ok {
				w.WriteHeader(404)
				json.NewEncoder(w).Encode(api.Error{Message: "does not exist"})
				return
			}
			w.Write(b)
		})
		mux.HandleFunc("PUT /store/{ns}/{key}", func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			defer f.mu.Unlock()
			b, _ := io.ReadAll(r.Body)
			f.store[r.PathValue("ns")+"/"+r.PathValue("key")] = b
			w.WriteHeader(204)
		})
	}
	mux.HandleFunc("PUT /snapshots/{name}/catalog", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.legacy = append(f.legacy, "catalog")
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.Snapshot{Name: r.PathValue("name")})
	})
	mux.HandleFunc("PUT /snapshots/{name}/health", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.legacy = append(f.legacy, "health")
		f.mu.Unlock()
		json.NewEncoder(w).Encode(api.Snapshot{Name: r.PathValue("name")})
	})
	mux.HandleFunc("GET /links", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.legacy = append(f.legacy, "links")
		json.NewEncoder(w).Encode(f.links)
	})
	mux.HandleFunc("PUT /links", func(w http.ResponseWriter, r *http.Request) {
		var l Link
		json.NewDecoder(r.Body).Decode(&l)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.legacy = append(f.legacy, "setlink")
		f.links = append(f.links, &l)
		json.NewEncoder(w).Encode(&l)
	})

	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close(); os.Remove(f.socket) })
	return f, api.NewClient("unix://" + f.socket)
}

func TestToolsOfYHealthOf(t *testing.T) {
	s := WithTools(&api.Snapshot{}, []ToolSpec{{Name: "b"}, {Name: "c"}})
	if tools, _ := ToolsOf(s); len(tools) != 2 {
		t.Fatalf("ToolsOf: %v", tools)
	}
	if tools, _ := ToolsOf(&api.Snapshot{}); tools != nil {
		t.Fatalf("sin anotación no hay catálogo: %v", tools)
	}
	if h := HealthOf(&api.Snapshot{}); h.Status != "" {
		t.Fatalf("sin sondeo el estado es vacío: %+v", h)
	}
}

func TestSetToolsYHealthContraDaemonNuevo(t *testing.T) {
	f, c := newFakeDaemon(t, true)
	ctx := context.Background()
	if err := SetTools(ctx, c, "eco", []ToolSpec{{Name: "echo"}}); err != nil {
		t.Fatal(err)
	}
	if err := SetHealth(ctx, c, "eco", false, "timeout"); err != nil {
		t.Fatal(err)
	}
	if len(f.legacy) != 0 {
		t.Fatalf("un daemon nuevo no debe recibir rutas antiguas: %v", f.legacy)
	}
	var h Health
	json.Unmarshal(f.annot["eco"][HealthKey], &h)
	if h.Status != Unhealthy || h.Error != "timeout" || h.At == nil {
		t.Fatalf("mcp.health mal escrita: %s", f.annot["eco"][HealthKey])
	}
	// Un snapshot inexistente es un error, NO un motivo para probar la ruta vieja.
	err := SetTools(ctx, c, "no-existe", nil)
	if err == nil || len(f.legacy) != 0 {
		t.Fatalf("snapshot inexistente: err=%v legacy=%v", err, f.legacy)
	}
}

// Contra un daemon anterior a las anotaciones no hay ruta a la que caer: el
// error tiene que decir qué hacer, no un 404 pelado.
func TestSetToolsYHealthContraDaemonViejo(t *testing.T) {
	f, c := newFakeDaemon(t, false)
	ctx := context.Background()
	for _, err := range []error{
		SetTools(ctx, c, "eco", []ToolSpec{{Name: "echo"}}),
		SetHealth(ctx, c, "eco", true, ""),
	} {
		if err == nil || !strings.Contains(err.Error(), "too old") {
			t.Fatalf("quería un error de daemon demasiado viejo: %v", err)
		}
	}
	if len(f.legacy) != 0 {
		t.Fatalf("no debe tocar las rutas de v0.4: %v", f.legacy)
	}
}

func TestLinksContraDaemonNuevoYViejo(t *testing.T) {
	ctx := context.Background()

	f, c := newFakeDaemon(t, true)
	if ls, err := Links(ctx, c); err != nil || len(ls) != 0 {
		t.Fatalf("store vacío: %v %v", ls, err)
	}
	if _, err := SetLink(ctx, c, &Link{Name: "engram", URL: "http://mac:9100/mcp"}); err != nil {
		t.Fatal(err)
	}
	first := time.Now()
	if _, err := SetLink(ctx, c, &Link{Name: "engram", URL: "http://mac:9101/mcp"}); err != nil {
		t.Fatal(err)
	}
	ls, err := Links(ctx, c)
	if err != nil || len(ls) != 1 || ls[0].URL != "http://mac:9101/mcp" || ls[0].CreatedAt.After(first) {
		t.Fatalf("actualizar un link debe conservar su fecha de alta: %+v %v", ls, err)
	}
	if err := RemoveLink(ctx, c, "nada"); err == nil {
		t.Fatal("quitar un link inexistente debe fallar")
	}
	if len(f.legacy) != 0 {
		t.Fatalf("un daemon nuevo no debe recibir /links: %v", f.legacy)
	}

	_, c = newFakeDaemon(t, false)
	if _, err := Links(ctx, c); err == nil || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("links contra un daemon sin store: %v", err)
	}
}
