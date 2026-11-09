//go:build darwin

package machine

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	share "github.com/juan52878911/kindling/internal/shareproto"
	"github.com/juan52878911/kindling/pkg/api"
)

// agenteFalso contesta a POST /share/attach según responder y apunta lo que
// le piden.
type agenteFalso struct {
	mu       sync.Mutex
	pedidos  []share.Attach
	responde func(a share.Attach, w http.ResponseWriter, r *http.Request)
}

func (f *agenteFalso) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var a share.Attach
	_ = json.NewDecoder(r.Body).Decode(&a)
	f.mu.Lock()
	f.pedidos = append(f.pedidos, a)
	f.mu.Unlock()
	w.Header().Set(share.HeaderAgent, share.Proto)
	f.responde(a, w, r)
}

// subir contesta 101 y cierra: una sesión FUSE que se corta en seguida.
func subir(w http.ResponseWriter) {
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: " + share.Proto + "\r\nConnection: Upgrade\r\n\r\n")
	brw.Flush()
	conn.Close()
}

// klingHVFalso escucha en el fc.sock de la máquina y contesta 101 a POST
// /kling/share/attach: devuelve los tags que le conectan.
func klingHVFalso(t *testing.T, sock string) chan int {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	tags := make(chan int, 4)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Tag int `json:"tag"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path != "/kling/share/attach" || !strings.EqualFold(r.Header.Get("Upgrade"), share.Proto) {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		tags <- b.Tag
		subir(w)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return tags
}

// El daemon negocia por dónde va cada carpeta viva: virtio-fs si la máquina
// tiene el dispositivo y el agente lo monta (la sesión va a kling-hv), FUSE
// si el agente dice que no puede, y virtio-fs si el agente ya la tiene así
// aunque el daemon no lo supiera.
func TestAttachNegociaTransporte(t *testing.T) {
	casos := []struct {
		nombre     string
		transporte string
		responde   func(a share.Attach, w http.ResponseWriter, r *http.Request)
		pedidos    []string // Transport de cada POST al agente
		via        string
	}{
		{"virtio-fs", share.TransportVirtioFS, func(a share.Attach, w http.ResponseWriter, _ *http.Request) {
			w.Header().Set(share.HeaderTransport, share.TransportVirtioFS)
			w.WriteHeader(http.StatusOK)
		}, []string{share.TransportVirtioFS}, share.TransportVirtioFS},
		{"kernel sin virtio-fs", share.TransportVirtioFS, func(a share.Attach, w http.ResponseWriter, _ *http.Request) {
			if a.Transport != "" {
				w.Header().Set(share.HeaderTransport, "fuse")
				http.Error(w, "no virtiofs", http.StatusNotImplemented)
				return
			}
			subir(w)
		}, []string{share.TransportVirtioFS, ""}, ""},
		{"agente anterior", share.TransportVirtioFS, func(a share.Attach, w http.ResponseWriter, _ *http.Request) {
			subir(w) // no sabe de Transport: monta por FUSE
		}, []string{share.TransportVirtioFS}, ""},
		{"el agente la tiene por virtio-fs", "", func(a share.Attach, w http.ResponseWriter, _ *http.Request) {
			if a.Transport == "" {
				w.Header().Set(share.HeaderTransport, share.TransportVirtioFS)
				http.Error(w, "mounted over virtio-fs", http.StatusNotImplemented)
				return
			}
			w.Header().Set(share.HeaderTransport, share.TransportVirtioFS)
			w.WriteHeader(http.StatusOK)
		}, []string{"", share.TransportVirtioFS}, share.TransportVirtioFS},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			// Raíz corta: el fc.sock tiene que caber en sun_path.
			root, err := os.MkdirTemp("/tmp", "kvfs")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			m := &Manager{root: root, byID: map[string]*api.Machine{}, socket: map[string]string{}}
			tags := klingHVFalso(t, m.socketDe("m1", 1))
			ag := &agenteFalso{responde: c.responde}
			ts := httptest.NewServer(ag)
			defer ts.Close()
			mc := &api.Machine{ID: "m1", Name: "m1", PID: 1, State: api.StateRunning,
				Forwards: map[string]string{strconv.Itoa(api.GuestPort): strings.TrimPrefix(ts.URL, "http://")}}
			s := api.ShareAttachment{Source: t.TempDir(), Mount: "/w", Mode: share.ModeRW, Transport: c.transporte}
			// attachOnce revalida la carpeta contra share_roots.
			m.SetShareConfig(func() ShareConfig { return ShareConfig{Roots: []string{s.Source}} })
			tg := &shareTag{tag: 2, status: "detached", first: make(chan struct{})}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := m.attachOnce(ctx, make(chan struct{}), mc, s, tg); err != nil {
				t.Fatal(err)
			}
			ag.mu.Lock()
			var got []string
			for _, p := range ag.pedidos {
				got = append(got, p.Transport)
				if p.Tag != 2 {
					t.Errorf("attach for tag %d", p.Tag)
				}
			}
			ag.mu.Unlock()
			if strings.Join(got, ",") != strings.Join(c.pedidos, ",") {
				t.Fatalf("the agent got transports %q, want %q", got, c.pedidos)
			}
			if tg.getVia() != c.via {
				t.Fatalf("served via %q, want %q", tg.getVia(), c.via)
			}
			select {
			case tag := <-tags:
				if c.via != share.TransportVirtioFS || tag != 2 {
					t.Fatalf("kling-hv got an attach for %d (via %q)", tag, c.via)
				}
			default:
				if c.via == share.TransportVirtioFS {
					t.Fatal("kling-hv got no attach")
				}
			}
		})
	}
}
